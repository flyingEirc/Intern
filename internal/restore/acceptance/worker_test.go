//go:build integration

package acceptance

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/flyingEirc/Intern/internal/artifact"
	"github.com/flyingEirc/Intern/internal/physical"
	"github.com/flyingEirc/Intern/internal/restore"
)

// TestP4Worker is the single native P4 entry point. Every case is required: a
// missing fixture, a skip or an unexpected success fails the run.
func TestP4Worker(t *testing.T) {
	w := newWorker(t)
	w.produce()
	steps := []struct {
		name string
		run  func(*testing.T, *worker)
	}{
		{"a1-plan", a1Plan},
		{"a2-local", a2Local},
		{"a5-argv", a5Argv},
		{"a6-enospc", a6ENOSPC},
		{"a3-ftp", a3FTP},
		{"a4-reject", a4Reject},
		{"a6-cancel", a6Cancel},
		{"a6-sigkill", a6SigKill},
		{"a7-security", a7Security},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) { step.run(t, w) })
	}
}

// logf emits one evidence line after proving it contains no fixture secret.
func (w *worker) logf(format string, args ...any) {
	w.t.Helper()
	message := fmt.Sprintf(format, args...)
	for _, key := range []string{"INTERN_P4_PASSWORD", "INTERN_P4_FTP_PASSWORD"} {
		if secret := os.Getenv(key); secret != "" && strings.Contains(message, secret) {
			w.t.Fatal("evidence contained a fixture credential")
		}
	}
	if root := os.Getenv("INTERN_P4_ROOT"); root != "" && strings.Contains(message, filepath.Join(root, "keys")) {
		w.t.Fatal("evidence contained the KEK path")
	}
	w.t.Log(message)
}

// failureTree renders the whole diagnostic chain of a refused run, one withheld
// line per cause, so a native run names the refusing sub-step — for an
// authentication failure: record, member, payload or tool_metadata — instead of
// only its fixed code. ExecError.Error() deliberately stops at code and phase;
// the cause is reachable through Unwrap, carries only fixed labels, and every
// line still passes through withhold before it is logged.
func failureTree(err error) string {
	if err == nil {
		return "<nil>"
	}
	var lines []string
	appendCauses(err, 0, func(depth int, message string) {
		lines = append(lines, strings.Repeat("  ", depth)+withhold(message))
	})
	return strings.Join(lines, "\n")
}

func appendCauses(err error, depth int, visit func(int, string)) {
	if err == nil {
		return
	}
	visit(depth, err.Error())
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, part := range joined.Unwrap() {
			appendCauses(part, depth+1, visit)
		}
		return
	}
	appendCauses(errors.Unwrap(err), depth+1, visit)
}

func (w *worker) planFor(t *testing.T, opts restore.Options, backupID string) restore.Plan {
	t.Helper()
	plan, err := restore.BuildPlan(t.Context(), opts, backupID)
	if err != nil {
		w.logf("plan failed: %s", withhold(err.Error()))
		t.Fatalf("plan %s: %v", backupID, err)
	}
	return plan
}

func sourcesOf(plan restore.Plan) string {
	values := make([]string, 0, len(plan.Links))
	for _, link := range plan.Links {
		values = append(values, link.Source)
	}
	return strings.Join(values, ",")
}

func (w *worker) assertContinuous(t *testing.T, plan restore.Plan) {
	t.Helper()
	for index, link := range plan.Links {
		if link.Seq != index+1 {
			t.Fatalf("plan link %d has seq %d", index, link.Seq)
		}
		if index > 0 && link.FromLSN != plan.Links[index-1].ToLSN {
			t.Fatal("plan links are not LSN-continuous")
		}
	}
	if last := plan.Links[len(plan.Links)-1]; last.ToLSN != plan.TargetToLSN {
		t.Fatal("plan target LSN differs from its last link")
	}
}

// a1Plan builds one plan per produced backup and checks the link set itself: the
// full backup restores with one link, incremental 1 with two and incremental 2
// with three, each link naming the backup the harness produced at that position.
// The locked case targets incremental 1 (not the newest) and incremental 2.
func a1Plan(t *testing.T, w *worker) {
	opts := w.options()
	plans := make([]restore.Plan, 0, len(w.manifest.Backups))
	for index, file := range w.manifest.Backups {
		plan := w.planFor(t, opts, file.BackupID)
		w.assertContinuous(t, plan)
		assertPlanLinks(t, w, plan, index+1)
		if want := strings.TrimSuffix(strings.Repeat("local,", index+1), ","); sourcesOf(plan) != want {
			t.Fatalf("plan for seq=%d sources = %s, want %s", file.Seq, sourcesOf(plan), want)
		}
		plans = append(plans, plan)
	}
	inc1, inc2 := plans[1], plans[2]
	assertPlanSpace(t, w, inc1)
	assertPlanRisks(t, inc2)
	w.assertPlanReadOnly(t, opts, inc2)
	keyID, err := artifact.KeyID(w.paths.key)
	must(t, err)
	w.logf("P4-A1 plan target=inc1 sources=%s links=%d lsn_continuous=true", sourcesOf(inc1), len(inc1.Links))
	w.logf("P4-A1 plan target=inc2 sources=%s links=%d lsn_continuous=true digest=%s", sourcesOf(inc2), len(inc2.Links), inc2.Digest)
	w.logf("P4-A1 plan_readonly db=unchanged repo=unchanged export=empty ftp_writes=0")
	w.logf("P4-A1 space required_work=%d required_export=%d available_work=%d available_export=%d matches_statfs=true",
		inc1.RequiredWorkBytes, inc1.RequiredExportBytes, inc1.AvailableWorkBytes, inc1.AvailableExportBytes)
	w.logf("P4-A1 matrix server=%s tool=%s prepare_version=%s key_id=%s",
		inc1.ServerVersion, inc1.ToolVersion, w.preflight.Tool.Version, keyID)
}

// assertPlanLinks requires exactly the links of one prefix of the produced
// chain, in sequence, each naming the backup and recorded sizes the harness
// produced at that position, and the target LSN of the last one.
func assertPlanLinks(t *testing.T, w *worker, plan restore.Plan, links int) {
	t.Helper()
	if len(plan.Links) != links {
		t.Fatalf("plan links = %d, want %d", len(plan.Links), links)
	}
	for index, link := range plan.Links {
		produced := w.manifest.backup(t, index)
		if link.BackupID != produced.BackupID || link.Seq != produced.Seq {
			t.Fatalf("plan link %d is %s seq %d, want %s seq %d",
				index, link.BackupID, link.Seq, produced.BackupID, produced.Seq)
		}
		if link.PayloadSize != produced.PayloadSize || link.MetadataSize != produced.MetadataSize {
			t.Fatalf("plan link %d sizes %d/%d differ from the produced %d/%d",
				index, link.PayloadSize, link.MetadataSize, produced.PayloadSize, produced.MetadataSize)
		}
	}
	if last := plan.Links[len(plan.Links)-1]; last.ToLSN != plan.TargetToLSN {
		t.Fatal("plan target LSN differs from its last link")
	}
}

func assertPlanSpace(t *testing.T, w *worker, plan restore.Plan) {
	t.Helper()
	if plan.RequiredWorkBytes != sumPayload(plan)*(2+8) || plan.RequiredExportBytes != sumPayload(plan)*8 {
		t.Fatal("space estimate does not follow the documented formula")
	}
	workSpace, err := physical.QuerySpace(w.paths.work)
	must(t, err)
	exportSpace, err := physical.QuerySpace(w.paths.export)
	must(t, err)
	if !closeCapacity(plan.AvailableWorkBytes, workSpace.AvailableBytes) || !closeCapacity(plan.AvailableExportBytes, exportSpace.AvailableBytes) {
		t.Fatalf("plan capacity %d/%d differs from Statfs %d/%d",
			plan.AvailableWorkBytes, plan.AvailableExportBytes, workSpace.AvailableBytes, exportSpace.AvailableBytes)
	}
}

func assertPlanRisks(t *testing.T, plan restore.Plan) {
	t.Helper()
	for _, risk := range []string{restore.RiskNoAutoReplaceProduction, restore.RiskServerUUIDDuplicate, restore.RiskPITRUnavailable} {
		if !containsValue(plan.Risks, risk) {
			t.Fatalf("plan risks omit %s", risk)
		}
	}
}

// assertPlanReadOnly proves the read-only plan contract from observable state.
func (w *worker) assertPlanReadOnly(t *testing.T, opts restore.Options, plan restore.Plan) {
	t.Helper()
	dbBefore := fileDigestOf(t, w.paths.db)
	repoBefore := treeDigest(t, w.paths.repo)
	exportBefore := strings.Join(entryNames(t, w.paths.export), ",")
	ftpBefore := w.remoteDigest(t)
	if _, err := restore.BuildPlan(t.Context(), opts, plan.TargetBackupID); err != nil {
		t.Fatalf("second plan: %v", err)
	}
	if dbBefore != fileDigestOf(t, w.paths.db) || repoBefore != treeDigest(t, w.paths.repo) ||
		exportBefore != strings.Join(entryNames(t, w.paths.export), ",") || ftpBefore != w.remoteDigest(t) {
		t.Fatal("plan changed database, repository, export root or replica")
	}
}

func sumPayload(plan restore.Plan) int64 {
	var total int64
	for _, link := range plan.Links {
		total += link.PayloadSize
	}
	return total
}

// closeCapacity tolerates other writers between two filesystem observations.
func closeCapacity(planned, observed int64) bool {
	difference := planned - observed
	if difference < 0 {
		difference = -difference
	}
	return difference <= max(int64(1<<20), observed/100)
}

func containsValue(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// events observes phase boundaries and records whether the rehearsal supervisor
// really runs in a process group of its own.
type events struct {
	mu      sync.Mutex
	onPhase func(restore.Phase, map[string]any)
	phases  []restore.Phase
	pgidOK  bool
}

func (e *events) report(phase restore.Phase, data map[string]any) {
	e.mu.Lock()
	e.phases = append(e.phases, phase)
	callback := e.onPhase
	e.mu.Unlock()
	if phase == restore.PhaseRehearsing {
		if pid, ok := data["pid"].(int); ok && pid > 0 {
			if pgid, err := processGroup(pid); err == nil && pgid != syscall.Getpgrp() {
				e.mu.Lock()
				e.pgidOK = true
				e.mu.Unlock()
			}
		}
	}
	if callback != nil {
		callback(phase, data)
	}
}

// killProcessTree SIGKILLs the whole observed tree: the worker process group,
// every descendant group (the supervised tool) and the descendant processes
// themselves. It is the SIGKILL case the restore recovery must survive.
func killProcessTree(pid int) error {
	pids := descendantPIDs(pid)
	var result error
	for group := range processGroups(pids, pid) {
		result = errors.Join(result, signalGroup(group))
	}
	for _, candidate := range pids {
		result = errors.Join(result, signalProcess(candidate))
	}
	return result
}

func processGroups(pids []int, root int) map[int]bool {
	groups := map[int]bool{}
	for _, candidate := range pids {
		if group, err := syscall.Getpgid(candidate); err == nil && group > 0 {
			groups[group] = true
		}
	}
	if group, err := syscall.Getpgid(root); err == nil {
		groups[group] = true
	}
	return groups
}

func signalGroup(group int) error {
	if err := syscall.Kill(-group, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

func signalProcess(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

func descendantPIDs(root int) []int {
	children, err := childProcesses()
	if err != nil {
		return []int{root}
	}
	result := []int{root}
	for queue := []int{root}; len(queue) > 0; queue = queue[1:] {
		for _, child := range children[queue[0]] {
			result = append(result, child)
			queue = append(queue, child)
		}
	}
	return result
}

// childProcesses maps every live pid to its children from /proc.
func childProcesses() (map[int][]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	children := map[int][]int{}
	for _, entry := range entries {
		if !digitsOnly(entry.Name()) {
			continue
		}
		pid, parent, ok := processIdentities(filepath.Join("/proc", entry.Name(), "stat"))
		if ok {
			children[parent] = append(children[parent], pid)
		}
	}
	return children, nil
}

func processIdentities(path string) (pid, parent int, ok bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, false
	}
	name := filepath.Base(filepath.Dir(path))
	pid, err = strconv.Atoi(name)
	if err != nil {
		return 0, 0, false
	}
	body := string(raw)
	end := strings.LastIndexByte(body, ')')
	if end < 0 || end+2 >= len(body) {
		return 0, 0, false
	}
	fields := strings.Fields(body[end+2:])
	if len(fields) < 2 {
		return 0, 0, false
	}
	parent, err = strconv.Atoi(fields[1])
	return pid, parent, err == nil
}

func processGroup(pid int) (int, error) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, err
	}
	// The command name may contain spaces; the fields after the closing
	// parenthesis are: state ppid pgrp ...
	body := string(raw)
	if end := strings.LastIndexByte(body, ')'); end >= 0 && end+2 < len(body) {
		fields := strings.Fields(body[end+2:])
		if len(fields) >= 3 {
			return strconv.Atoi(fields[2])
		}
	}
	return 0, errors.New("cannot parse process group")
}

func (w *worker) execute(t *testing.T, opts restore.Options, request restore.Request, progress *events) (restore.Result, error) {
	t.Helper()
	if progress == nil {
		progress = &events{}
	}
	return restore.Execute(t.Context(), opts, request, progress.report)
}

func a2Local(t *testing.T, w *worker) {
	opts := w.options()
	progress := &events{}
	var lastVerify *restore.VerifyResult
	for index, name := range []string{"inc1", "inc2"} {
		file := w.manifest.backup(t, index+1)
		plan := w.planFor(t, opts, file.BackupID)
		if sourcesOf(plan) != strings.TrimSuffix(strings.Repeat("local,", len(plan.Links)), ",") {
			t.Fatalf("%s plan did not stay local: %s", name, sourcesOf(plan))
		}
		request := restore.Request{
			RestoreID: randomRestoreID(t), Plan: plan, Rehearse: true,
			VerifySQL: []string{
				"SELECT COUNT(*) FROM fixture.records",
				"SELECT MAX(id) FROM fixture.records",
			},
		}
		result, err := w.execute(t, opts, request, progress)
		if err != nil {
			w.logf("restore %s failed:\n%s", name, failureTree(err))
			t.Fatalf("restore %s: %v", name, err)
		}
		w.assertRestored(t, file, request, result)
		w.assertRehearsal(t, file, result.Verify)
		lastVerify = result.Verify
		w.facts.prepareArgs[request.RestoreID] = result.PrepareArgs
		w.facts.sources[request.RestoreID] = sourcesOf(plan)
		w.logf("P4-A2 restore=%s source=local export=%s expected_snapshot=%s rows=%d max_id=%d prepare_checkpoints=full-prepared to_lsn=%d",
			name, request.RestoreID, file.SnapshotSHA256, file.RecordsRows, file.MaxRecordID, result.PreparedToLSN)
	}
	if progress.pgidOK {
		supervisorEvidence = true
	}
	w.logf("P4-A2 rehearsal passed system_tables=true schemas=%d check_table=%d verify_sql=2 verify_sql_match=true",
		len(lastVerify.Schemas), len(lastVerify.CheckTable))
	w.logf("P4-A2 export_perms dir=0750 file=0640 owner=exact")
}

// supervisorEvidence records that the engine's rehearsal used a separate group.
var supervisorEvidence bool

func (w *worker) assertRestored(t *testing.T, file producedFile, request restore.Request, result restore.Result) {
	t.Helper()
	export := filepath.Join(w.paths.export, request.RestoreID)
	if result.ExportPath != export {
		t.Fatalf("export path = %s, want %s", result.ExportPath, export)
	}
	if result.PreparedToLSN != file.ToLSN || result.ExportBytes <= 0 || len(result.PrepareArgs) == 0 {
		t.Fatal("restore result does not describe a prepared export")
	}
	for _, args := range result.PrepareArgs {
		if args[0] != "--no-defaults" {
			t.Fatal("prepare argv does not start with --no-defaults")
		}
	}
	assertExportModes(t, export)
	assertRestoreTargetsGone(t, w, request.RestoreID)
}

func assertExportModes(t *testing.T, export string) {
	t.Helper()
	info, err := os.Lstat(filepath.Join(export, "data"))
	must(t, err)
	if !info.IsDir() || info.Mode().Perm() != 0o750 {
		t.Fatalf("export data mode = %o, want 0750", info.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Join(export, "data"))
	must(t, err)
	if len(entries) == 0 {
		t.Fatal("restored datadir is empty")
	}
	assertExportFileOwner(t, filepath.Join(export, "data"), entries)
}

func assertExportFileOwner(t *testing.T, dir string, entries []os.DirEntry) {
	t.Helper()
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		fileInfo, err := entry.Info()
		must(t, err)
		if fileInfo.Mode().Perm() != 0o640 {
			t.Fatalf("export file %s mode = %o, want 0640", entry.Name(), fileInfo.Mode().Perm())
		}
		stat, ok := fileInfo.Sys().(*syscall.Stat_t)
		if !ok || int(stat.Uid) != os.Geteuid() || int(stat.Gid) != os.Getegid() {
			t.Fatalf("export file %s owner differs from the worker", entry.Name())
		}
		return
	}
}

func assertRestoreTargetsGone(t *testing.T, w *worker, restoreID string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(w.paths.work, "restore-"+restoreID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the work directory survived a successful restore")
	}
	if _, err := os.Lstat(filepath.Join(w.paths.export, ".pending-"+restoreID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a pending export survived")
	}
}

func (w *worker) assertRehearsal(t *testing.T, file producedFile, verify *restore.VerifyResult) {
	t.Helper()
	if verify == nil || verify.Status != "passed" || !verify.Started || !verify.SystemTablesReadable {
		w.logf("rehearsal failure: %+v", verify)
		t.Fatal("rehearsal did not pass")
	}
	assertVerifyQueries(t, file, verify.Queries)
	assertCheckTables(t, verify.CheckTable)
}

func assertVerifyQueries(t *testing.T, file producedFile, queries []restore.QueryResult) {
	t.Helper()
	if len(queries) != 2 {
		t.Fatalf("verify queries = %d", len(queries))
	}
	for _, query := range queries {
		if query.ErrorCode != "" || len(query.Rows) != 1 || len(query.Rows[0]) != 1 {
			t.Fatalf("verify query failed: %+v", query)
		}
		value, err := strconv.ParseInt(query.Rows[0][0], 10, 64)
		must(t, err)
		if strings.Contains(strings.ToUpper(query.SQL), "COUNT") {
			if value != file.RecordsRows {
				t.Fatalf("restored row count = %d, want %d", value, file.RecordsRows)
			}
		} else if value != file.MaxRecordID {
			t.Fatalf("restored max id = %d, want %d", value, file.MaxRecordID)
		}
	}
}

func assertCheckTables(t *testing.T, checks []restore.CheckStat) {
	t.Helper()
	if len(checks) == 0 {
		t.Fatal("rehearsal performed no CHECK TABLE")
	}
	for _, check := range checks {
		if !strings.EqualFold(check.Status, "OK") {
			t.Fatalf("CHECK TABLE %s = %s", check.Table, check.Status)
		}
	}
}

func a5Argv(t *testing.T, w *worker) {
	// A single-link chain is the third real shape: the unreplicated full backup
	// produced for the rejection cases is restored here, without a rehearsal,
	// purely to observe the real one-step prepare argv.
	opts := w.options()
	plan := w.planFor(t, opts, w.rejectBackup.BackupID)
	request := restore.Request{RestoreID: randomRestoreID(t), Plan: plan}
	result, err := w.execute(t, opts, request, nil)
	must(t, err)
	w.facts.prepareArgs[request.RestoreID] = result.PrepareArgs
	w.facts.sources[request.RestoreID] = sourcesOf(plan)
	w.assertRestored(t, w.rejectBackup, request, result)
	if recorded := assertRecordedShapes(t, w); recorded != 3 {
		t.Fatalf("recorded argv shapes = %d, want 3", recorded)
	}
	w.logf("P4-A5 argv shapes=full-only,full+1,full+2 no_defaults_first=true matches=true")
}

func assertRecordedShapes(t *testing.T, w *worker) int {
	t.Helper()
	checked := 0
	for _, args := range w.facts.prepareArgs {
		if len(args) == 0 || args[0][0] != "--no-defaults" {
			t.Fatal("a recorded prepare argv does not start with --no-defaults")
		}
		if !assertPrepareShape(t, w, args) {
			t.Fatalf("unexpected prepare step count %d", len(args))
		}
		checked++
	}
	return checked
}

func assertPrepareShape(t *testing.T, w *worker, args [][]string) bool {
	t.Helper()
	switch len(args) {
	case 1:
		return assertOneLinkShape(t, w, args[0])
	case 2:
		return assertTwoLinkShape(t, w, args)
	case 3:
		return assertThreeLinkShape(t, w, args)
	default:
		return false
	}
}

func assertOneLinkShape(t *testing.T, w *worker, args []string) bool {
	t.Helper()
	want := "--no-defaults --prepare --target-dir=" + filepath.Join(w.paths.work, "0")
	if strings.Join(args, " ") != want {
		t.Fatalf("one-link argv = %q, want %q", args, want)
	}
	return true
}

func assertTwoLinkShape(t *testing.T, w *worker, args [][]string) bool {
	t.Helper()
	first, final := strings.Join(args[0], " "), strings.Join(args[1], " ")
	if !strings.Contains(first, "--apply-log-only") || strings.Contains(first, "--incremental-dir") {
		t.Fatalf("two-link first argv = %q", args[0])
	}
	if strings.Contains(final, "--apply-log-only") || !strings.Contains(final, "--incremental-dir="+filepath.Join(w.paths.work, "1")) {
		t.Fatalf("two-link final argv = %q", args[1])
	}
	return true
}

func assertThreeLinkShape(t *testing.T, w *worker, args [][]string) bool {
	t.Helper()
	first, middle, final := strings.Join(args[0], " "), strings.Join(args[1], " "), strings.Join(args[2], " ")
	if !strings.Contains(first, "--apply-log-only") || !strings.Contains(middle, "--apply-log-only") || strings.Contains(final, "--apply-log-only") {
		t.Fatalf("three-link argv = %q", args)
	}
	if !strings.Contains(middle, "--incremental-dir="+filepath.Join(w.paths.work, "1")) ||
		!strings.Contains(final, "--incremental-dir="+filepath.Join(w.paths.work, "2")) {
		t.Fatalf("three-link incremental argv = %q", args)
	}
	return true
}

func a6ENOSPC(t *testing.T, w *worker) {
	opts := w.options()
	opts.WorkRoot = w.paths.smallWork
	opts.ExpansionFactor = 1
	plan := w.planFor(t, opts, w.manifest.backup(t, 1).BackupID)
	before := strings.Join(entryNames(t, w.paths.export), ",")
	space, err := physical.QuerySpace(w.paths.smallWork)
	must(t, err)
	filler := filepath.Join(w.paths.smallWork, "fill")
	progress := &events{onPhase: func(phase restore.Phase, _ map[string]any) {
		if phase != restore.PhaseFetching {
			return
		}
		if _, statErr := os.Lstat(filler); statErr == nil {
			return
		}
		leave := plan.Links[0].PayloadSize / 2
		body := make([]byte, max(int(space.AvailableBytes-leave), 0))
		must(t, os.WriteFile(filler, body, 0o600))
	}}
	_, err = w.execute(t, opts, restore.Request{RestoreID: randomRestoreID(t), Plan: plan}, progress)
	_ = os.Remove(filler)
	var execErr *restore.ExecError
	if !errors.As(err, &execErr) || execErr.Code != restore.CodeDiskFull {
		w.logf("enospc error:\n%s", failureTree(err))
		t.Fatalf("ENOSPC error = %v, want disk_full", err)
	}
	if execErr.SideEffects.ExportPublished || !execErr.SideEffects.WorkDirRemoved {
		t.Fatalf("ENOSPC side effects = %+v", execErr.SideEffects)
	}
	if before != strings.Join(entryNames(t, w.paths.export), ",") {
		t.Fatal("ENOSPC changed the export root")
	}
	entries, err := os.ReadDir(w.paths.smallWork)
	must(t, err)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "restore-") {
			t.Fatal("ENOSPC left a work directory")
		}
	}
	w.logf("P4-A6 enospc filesystem=tmpfs code=disk_full export=none work_gone=true published_untouched=true")
}

func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

func a6Cancel(t *testing.T, w *worker) {
	opts := w.options()
	plan := w.planFor(t, opts, w.manifest.backup(t, 1).BackupID)
	before := strings.Join(entryNames(t, w.paths.export), ",")
	for _, phase := range []restore.Phase{restore.PhaseExtracting, restore.PhasePreparing} {
		id := randomRestoreID(t)
		ctx, cancel := context.WithCancel(t.Context())
		progress := &events{onPhase: func(current restore.Phase, _ map[string]any) {
			if current == phase {
				cancel()
			}
		}}
		_, err := restore.Execute(ctx, opts, restore.Request{RestoreID: id, Plan: plan}, progress.report)
		cancel()
		var execErr *restore.ExecError
		if !errors.As(err, &execErr) || execErr.Code != restore.CodeCanceled {
			w.logf("cancel during %s error:\n%s", phase, failureTree(err))
			t.Fatalf("cancel during %s error = %v, want canceled", phase, err)
		}
		if execErr.SideEffects.ExportPublished || !execErr.SideEffects.WorkDirRemoved || !execErr.SideEffects.PendingExportRemoved {
			t.Fatalf("cancel side effects = %+v", execErr.SideEffects)
		}
		if _, statErr := os.Lstat(filepath.Join(w.paths.work, "restore-"+id)); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatal("cancel left a work directory")
		}
		if before != strings.Join(entryNames(t, w.paths.export), ",") {
			t.Fatal("cancel changed the export root")
		}
		w.logf("P4-A6 cancel during=%s code=canceled work_gone=true pending_gone=true export_unchanged=true", phase)
	}
}

func a6SigKill(t *testing.T, w *worker) {
	id := randomRestoreID(t)
	ready := filepath.Join(w.paths.root, "kill-ready-"+id)
	foreign := randomRestoreID(t)
	seedForeignRestore(t, w, foreign)
	before := strings.Join(entryNames(t, w.paths.export), ",")
	command, output := startKillChild(t, w, id, ready)
	waitForFile(t, ready, 60*time.Second)
	waitForProcess(t, "xtrabackup", 60*time.Second)
	killChild(t, command, output)
	waitForNoProcess(t, "xtrabackup", 30*time.Second)
	assertKillRecovery(t, w, id, foreign, before)
	w.logf("P4-A6 sigkill inject=prepare process_tree=gone work_gone=true pending_gone=true export_unchanged=true others_untouched=true second_recover=noop")
}

// seedForeignRestore creates the two durable targets and a published export of
// another restore id, which recovery must never touch.
func seedForeignRestore(t *testing.T, w *worker, foreign string) {
	t.Helper()
	for _, dir := range []string{
		filepath.Join(w.paths.work, "restore-"+foreign),
		filepath.Join(w.paths.export, ".pending-"+foreign),
		filepath.Join(w.paths.export, foreign),
	} {
		must(t, os.Mkdir(dir, 0o700))
		must(t, os.WriteFile(filepath.Join(dir, "sentinel"), []byte(foreign), 0o600))
	}
}

func startKillChild(t *testing.T, w *worker, id, ready string) (*exec.Cmd, *lockedBuffer) {
	t.Helper()
	executable, err := os.Executable()
	must(t, err)
	command := exec.CommandContext(t.Context(), executable, "-test.v", "-test.run=^TestP4KillChildWorker$", "-test.timeout=3m")
	command.Env = append(os.Environ(), "INTERN_P4_KILL_ID="+id, "INTERN_P4_KILL_READY="+ready)
	output := &lockedBuffer{}
	command.Stdout, command.Stderr = output, output
	must(t, command.Start())
	return command, output
}

func killChild(t *testing.T, command *exec.Cmd, output *lockedBuffer) {
	t.Helper()
	finished := make(chan error, 1)
	go func() { finished <- command.Wait() }()
	must(t, killProcessTree(command.Process.Pid))
	select {
	case err := <-finished:
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("child exit = %v, want a signal; output: %s", err, output.String())
		}
		status, ok := exit.Sys().(syscall.WaitStatus)
		if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
			t.Fatalf("child exit status = %v, want SIGKILL", status)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("killed restore worker did not exit")
	}
}

func assertKillRecovery(t *testing.T, w *worker, id, foreign, before string) {
	t.Helper()
	must(t, restore.Recover(t.Context(), w.paths.work, w.paths.export, []string{id}))
	for _, target := range []string{
		filepath.Join(w.paths.work, "restore-"+id),
		filepath.Join(w.paths.export, ".pending-"+id),
		filepath.Join(w.paths.export, id),
	} {
		if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("killed restore left %s", target)
		}
	}
	for _, target := range []string{
		filepath.Join(w.paths.work, "restore-"+foreign),
		filepath.Join(w.paths.export, ".pending-"+foreign),
		filepath.Join(w.paths.export, foreign, "sentinel"),
	} {
		if _, err := os.Lstat(target); err != nil {
			t.Fatalf("recovery removed another restore target %s", target)
		}
	}
	if before != strings.Join(entryNames(t, w.paths.export), ",") {
		t.Fatal("the killed restore changed unrelated export entries")
	}
	must(t, restore.Recover(t.Context(), w.paths.work, w.paths.export, []string{id}))
	if _, err := os.Lstat(filepath.Join(w.paths.work, "restore-"+foreign)); err != nil {
		t.Fatal("a second recovery changed another restore")
	}
}

func a7Security(t *testing.T, w *worker) {
	refusal := assertSymlinkedExportRefused(t, w)
	assertNoCredentialInRefusal(t, refusal)
	if !supervisorEvidence {
		t.Fatal("the rehearsal server did not run in a separate process group")
	}
	waitForNoProcess(t, "xtrabackup", 30*time.Second)
	waitForNoProcess(t, "mysqld", 30*time.Second)
	for _, id := range w.exportRestoreIDs(t) {
		assertExportModes(t, filepath.Join(w.paths.export, id))
	}
	w.logf("P4-A7 secrets=clean")
	w.logf("P4-A7 supervisor mysqld_pgid_separate=true tool_group=supervisor no_leftover_tools=true")
	w.logf("P4-A7 export_root symlink=refused export_perms=0750/0640 owner=exact")
}

func assertSymlinkedExportRefused(t *testing.T, w *worker) error {
	t.Helper()
	opts := w.options()
	symlink := filepath.Join(w.paths.root, "export-symlink")
	if _, err := os.Lstat(symlink); errors.Is(err, os.ErrNotExist) {
		must(t, os.Symlink(w.paths.export, symlink))
	}
	opts.ExportRoot = symlink
	_, err := restore.BuildPlan(t.Context(), opts, w.manifest.backup(t, 1).BackupID)
	var refusal *restore.PlanError
	if !errors.As(err, &refusal) || refusal.Code != restore.CodeUnconfigured {
		w.logf("symlinked export root error: %s", withhold(errString(err)))
		t.Fatalf("symlinked export root code = %v", err)
	}
	return err
}

// assertNoCredentialInRefusal requires a refusal diagnostic to be free of the
// generated credentials; the worker's own evidence lines are checked at emission.
func assertNoCredentialInRefusal(t *testing.T, refusal error) {
	t.Helper()
	message := errString(refusal)
	for _, key := range []string{"INTERN_P4_PASSWORD", "INTERN_P4_FTP_PASSWORD"} {
		secret := os.Getenv(key)
		if secret == "" {
			t.Fatal("fixture secret missing")
		}
		if strings.Contains(message, secret) {
			t.Fatal("a refusal leaked a credential")
		}
	}
}

// exportRestoreIDs lists the exports this suite published itself. The SIGKILL
// case seeds a foreign export directory holding only a sentinel file to prove
// recovery leaves another restore alone, and that tree is not a restore export,
// so the mode check runs over the ids the engine recorded and requires each of
// them to still be on disk.
func (w *worker) exportRestoreIDs(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(w.paths.export)
	must(t, err)
	present := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
			present[entry.Name()] = true
		}
	}
	ids := make([]string, 0, len(w.facts.prepareArgs))
	for id := range w.facts.prepareArgs {
		if !present[id] {
			t.Fatalf("the export published by restore %s is missing", id)
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		t.Fatal("no published export to inspect")
	}
	return ids
}

func randomRestoreID(t *testing.T) string {
	t.Helper()
	value := make([]byte, 16)
	_, err := rand.Read(value)
	must(t, err)
	return hex.EncodeToString(value)
}

func fileDigestOf(t *testing.T, path string) string {
	t.Helper()
	file, err := os.Open(path)
	must(t, err)
	defer func() { must(t, file.Close()) }()
	hash := sha256.New()
	_, err = io.Copy(hash, file)
	must(t, err)
	return hex.EncodeToString(hash.Sum(nil))
}

func treeDigest(t *testing.T, root string) string {
	t.Helper()
	var lines []string
	must(t, filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		digest := ""
		if entry.Type().IsRegular() {
			digest = fileDigestOf(t, path)
		}
		lines = append(lines, path+"|"+info.Mode().String()+"|"+digest)
		return nil
	}))
	sort.Strings(lines)
	hash := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(hash[:])
}

func entryNames(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	must(t, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("marker %s was not written", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitForProcess(t *testing.T, name string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if processRunning(name) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no live %s process observed", name)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForNoProcess(t *testing.T, name string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for processRunning(name) {
		if time.Now().After(deadline) {
			t.Fatalf("%s survived group teardown", name)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func processRunning(name string) bool {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !digitsOnly(entry.Name()) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "comm"))
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(raw)) == name || strings.HasPrefix(strings.TrimSpace(string(raw)), name) {
			return true
		}
	}
	return false
}

func digitsOnly(value string) bool {
	if value == "" {
		return false
	}
	for index := range len(value) {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
	}
	return true
}
