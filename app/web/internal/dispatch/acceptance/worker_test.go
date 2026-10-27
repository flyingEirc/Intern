//go:build integration

package acceptance

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/flyingEirc/Intern/app/web/internal/dispatch"
	"github.com/flyingEirc/Intern/app/web/internal/store"
	"github.com/flyingEirc/Intern/internal/storage/ftp"
)

const (
	// sseFirstEventBound is the locked promptness bound: the first SSE frame
	// must arrive within this long after the trigger request returned.
	sseFirstEventBound = 5 * time.Second
	// replicaSampleInterval is the protection sampling period while
	// replication is in progress. It must be far shorter than one FTP upload
	// so a pending or uploading replica is observed, not inferred.
	replicaSampleInterval = 10 * time.Millisecond
)

// TestW4Worker is the single W4 native entry point. It runs inside the pinned
// PXB container as the mysql uid against the real MySQL socket and Pure-FTPd
// fixture. A missing case or a skip is a failure.
func TestW4Worker(t *testing.T) {
	f := newFixture(t)
	f.topology(t)
	f.startDispatch(t)
	t.Logf("W4 worker tool_sha256=%s instance=%s uid=%d gid=%d", toolSHA256(), f.instanceID, os.Getuid(), os.Getgid())
	steps := []struct {
		name string
		run  func(*testing.T, *fixture)
	}{
		{"a1-a2-e2e", a1E2E},
		{"a2-priority", a2Priority},
		{"a3-cancel", a3Cancel},
		{"a4-sse", a4SSE},
		{"a3-sigterm", a3SIGTERM},
		{"a3-sigkill", a3SIGKILL},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			f.t = t
			step.run(t, f)
		})
	}
}

func a1E2E(t *testing.T, f *fixture) {
	t.Helper()
	// The protection sampler runs from before the first run so the
	// uploaded-but-unverified window is read from state, not assumed.
	watch := f.watchReplication(t)
	first := e2eIdempotency(t, f)
	out := f.e2eChainRuns(t, first)
	members := e2eMembers(t, f)
	f.e2eObjects(t, members)
	protection, logRows := f.e2eProtection(t, members, out)
	summary := watch.report(t)
	if summary.behind == 0 {
		t.Fatalf("no sample saw the remote point behind an unverified replica (%d samples); protection_requires_verified is unproven", summary.samples)
	}
	if summary.verified != 3 || summary.seen != 3 {
		t.Fatalf("replica samples covered %d verified of %d backups, want 3 of 3", summary.verified, summary.seen)
	}
	f.assertNoSecret(t, nil)
	t.Logf("W4-A1 e2e chain=seq:1,2,3 lsn_continuous=true replay_run_id=%s conflict=409 replicas_verified=3 protection_local=%s protection_remote=%s protection_requires_verified=true uploaded_window=observed samples=%d logs_agree=sqlite log_rows=%d lists_agree=sqlite,local,ftp",
		first, protection.Local.BackupID, protection.Remote[0].BackupID, summary.samples, logRows)
}

// e2eIdempotency proves the manual contract on the first full run: the same key
// and body replay the same run id, and the same key with another body conflicts.
func e2eIdempotency(t *testing.T, f *fixture) string {
	t.Helper()
	key := "w4-e2e-full"
	first := f.triggerRun(t, f.e2ePolicy, store.RunKindFull, key)
	replay := f.trigger(t, f.e2ePolicy, store.RunKindFull, key, http.StatusAccepted)
	var created createdRun
	must(t, json.Unmarshal(replay, &created))
	if created.RunID != first {
		t.Fatalf("replay run_id %s want %s", created.RunID, first)
	}
	conflict := f.trigger(t, f.e2ePolicy, store.RunKindIncremental, key, http.StatusConflict)
	if !bytes.Contains(conflict, []byte("onflict")) {
		t.Fatalf("conflict body %s", conflict)
	}
	return first
}

// e2eRuns is the observed outcome of the full -> incremental -> incremental
// sequence.
type e2eRuns struct {
	full, inc1, inc2 store.Run
}

// e2eChainRuns runs full -> DML -> incremental -> DML -> incremental and
// requires every replica to reach verified.
func (f *fixture) e2eChainRuns(t *testing.T, first string) e2eRuns {
	t.Helper()
	out := e2eRuns{}
	out.full = f.waitRun(t, first, store.RunStatusCompleted)
	if out.full.BackupID == nil || out.full.ChainID == nil {
		t.Fatal("completed full has no backup")
	}
	f.waitReplica(t, *out.full.BackupID, "verified")
	out.inc1 = f.e2eIncremental(t, "w4-e2e-inc-1", 1)
	out.inc2 = f.e2eIncremental(t, "w4-e2e-inc-2", 2)
	f.e2eCountVerified(t)
	return out
}

// e2eIncremental runs one DML + incremental step to completion and requires the
// verified replica.
func (f *fixture) e2eIncremental(t *testing.T, key string, n int) store.Run {
	t.Helper()
	f.dml(t, n)
	run := f.waitRun(t, f.triggerRun(t, f.e2ePolicy, store.RunKindIncremental, key), store.RunStatusCompleted)
	if run.BackupID == nil {
		t.Fatal("completed incremental has no backup")
	}
	f.waitReplica(t, *run.BackupID, "verified")
	return run
}

// e2eCountVerified requires all three backups to be verified on the FTP replica.
func (f *fixture) e2eCountVerified(t *testing.T) {
	t.Helper()
	verified := 0
	for _, replica := range f.listReplicas(t, "source_id="+f.sourceID+"&status=verified") {
		if replica.RepositoryID == f.ftpID {
			verified++
		}
	}
	if verified != 3 {
		t.Fatalf("verified replicas = %d", verified)
	}
}

// e2eMembers requires the chain to be seq 1/2/3 with continuous LSNs.
func e2eMembers(t *testing.T, f *fixture) []dispatch.BackupView {
	t.Helper()
	members := longestChain(t, f.listChains(t))
	if len(members) != 3 {
		t.Fatalf("chain members = %d want 3", len(members))
	}
	if members[0].Seq != 1 || members[1].Seq != 2 || members[2].Seq != 3 {
		t.Fatalf("seq = %d,%d,%d", members[0].Seq, members[1].Seq, members[2].Seq)
	}
	if !strings.Contains(members[0].Kind, "full") || !strings.Contains(members[1].Kind, "incremental") || !strings.Contains(members[2].Kind, "incremental") {
		t.Fatalf("kinds = %s,%s,%s", members[0].Kind, members[1].Kind, members[2].Kind)
	}
	if !lsnContinuous(members) {
		t.Fatalf("LSN gap: %+v", members)
	}
	return members
}

// e2eObjects requires the API view, the SQLite row, the local object directory
// and the FTP copy to agree on every chain member.
func (f *fixture) e2eObjects(t *testing.T, members []dispatch.BackupView) {
	t.Helper()
	remote := f.openFTP(t)
	for _, member := range members {
		f.e2eObject(t, remote, member)
	}
	if localKeys := f.listLocalObjects(t); len(localKeys) < 12 {
		t.Fatalf("local objects = %d", len(localKeys))
	}
	ftpKeys, err := remote.List(t.Context(), "")
	must(t, err)
	if len(ftpKeys) < 12 {
		t.Fatalf("ftp objects = %d", len(ftpKeys))
	}
}

// e2eObject compares one backup across the API, SQLite, the local repository and
// the FTP copy, including the replicated payload bytes.
func (f *fixture) e2eObject(t *testing.T, remote *ftp.Repository, member dispatch.BackupView) {
	t.Helper()
	view := f.getBackup(t, member.ID)
	if view.Status != "completed" || view.PayloadSHA256 == "" || view.ObjectPath == "" {
		t.Fatalf("backup view incomplete: %+v", view)
	}
	f.requireSQLiteBackup(t, member)
	localHash := f.hashLocal(t, member.ObjectPath)
	ftpHash := f.hashFTP(t, remote, member.ObjectPath)
	if localHash != member.PayloadSHA256 || ftpHash != member.PayloadSHA256 {
		t.Fatalf("payload sha mismatch backup=%s api=%s local=%s ftp=%s", member.ID, member.PayloadSHA256, localHash, ftpHash)
	}
	if !replicaVerified(view.Replicas, f.ftpID) {
		t.Fatalf("backup %s replica is not verified", member.ID)
	}
}

// e2eProtection requires local = the newest backup, remote = the newest
// verified one, the run list to agree with SQLite on the web run IDs, and the
// paged log endpoint to equal run_logs for every run of the sequence.
func (f *fixture) e2eProtection(t *testing.T, members []dispatch.BackupView, out e2eRuns) (dispatch.Protection, int) {
	t.Helper()
	protection := f.getProtection(t)
	if protection.Local == nil || protection.Local.BackupID != members[2].ID {
		t.Fatalf("local protection = %+v want %s", protection.Local, members[2].ID)
	}
	if len(protection.Remote) != 1 || protection.Remote[0].RepositoryID != f.ftpID {
		t.Fatalf("remote protection = %+v", protection.Remote)
	}
	if protection.Remote[0].BackupID != members[2].ID {
		t.Fatalf("remote protection = %s want %s", protection.Remote[0].BackupID, members[2].ID)
	}
	listed := f.listRuns(t, "source_id="+f.sourceID)
	if !runsAgree(t, f, listed, []string{out.full.ID, out.inc1.ID, out.inc2.ID}) {
		t.Fatalf("runs list disagrees with sqlite for %s: %+v", strings.Join([]string{out.full.ID, out.inc1.ID, out.inc2.ID}, ","), idsOf(listed))
	}
	logRows := 0
	for _, id := range []string{out.full.ID, out.inc1.ID, out.inc2.ID} {
		logRows += f.requireLogsAgree(t, id)
	}
	return protection, logRows
}

// requireSQLiteBackup requires the stored backup row to match the API view.
func (f *fixture) requireSQLiteBackup(t *testing.T, member dispatch.BackupView) {
	t.Helper()
	var sqliteID, sqliteSHA, sqlitePath string
	var sqliteSeq int
	must(t, f.analysis.QueryRowContext(t.Context(),
		`SELECT id, seq, object_path, payload_sha256 FROM backups WHERE id = ?`, member.ID,
	).Scan(&sqliteID, &sqliteSeq, &sqlitePath, &sqliteSHA))
	if sqliteID != member.ID || sqliteSeq != member.Seq || sqlitePath != member.ObjectPath || sqliteSHA != member.PayloadSHA256 {
		t.Fatalf("sqlite backup mismatch id=%s seq=%d", sqliteID, sqliteSeq)
	}
}

func a2Priority(t *testing.T, f *fixture) {
	t.Helper()
	policy := f.createPolicy(t, "w4-priority", 8)
	f.enable(t, policy)
	full1 := f.triggerRun(t, policy, store.RunKindFull, "w4-pri-full-1")
	f.waitXtrabackup(t, full1)
	// The incremental is queued BEFORE the second full while the first full is
	// still running: a plain FIFO claim would now pick the incremental, so a
	// full-first claim is the only way the full can be claimed first.
	inc := f.triggerRun(t, policy, store.RunKindIncremental, "w4-pri-inc")
	full2 := f.triggerRun(t, policy, store.RunKindFull, "w4-pri-full-2")
	queued := f.listRuns(t, "status=queued")
	if !containsRun(queued, full2) || !containsRun(queued, inc) {
		t.Fatalf("expected full and incremental queued together: %+v", idsOf(queued))
	}
	if n := len(f.listRuns(t, "status=running")); n != 1 {
		t.Fatalf("running while queued = %d", n)
	}
	incAt := f.sqliteString(t, `SELECT scheduled_at FROM runs WHERE id = ?`, inc)
	fullAt := f.sqliteString(t, `SELECT scheduled_at FROM runs WHERE id = ?`, full2)
	if !parseStamp(t, incAt).Before(parseStamp(t, fullAt)) {
		t.Fatalf("the incremental must be queued first for the ordering to prove anything: inc=%s full=%s", incAt, fullAt)
	}
	stop, done, peak := f.watchRunning(t)
	f.waitRun(t, full1, store.RunStatusCompleted)
	f.waitRun(t, full2, store.RunStatusCompleted)
	f.waitRun(t, inc, store.RunStatusCompleted)
	close(stop)
	<-done
	f.requireClaimOrder(t, full2, inc)
	if concurrent := f.requireDisjointRuns(t); concurrent != 1 {
		t.Fatalf("sqlite intervals show %d concurrent runs", concurrent)
	}
	if peak() != 1 {
		t.Fatalf("running_max=%d", peak())
	}
	t.Log("W4-A2 priority running_max=1 claim_full_before_incremental=true queued_together=true order=incremental_first sqlite_intervals=disjoint")
}

// watchRunning samples the running-run count until stop closes and reports the
// highest observed value. Global backup concurrency is frozen at 1.
func (f *fixture) watchRunning(t *testing.T) (stop chan struct{}, done chan struct{}, peak func() int) {
	t.Helper()
	stop, done = make(chan struct{}), make(chan struct{})
	var highest int
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if n := len(f.listRuns(t, "status=running")); n > highest {
				highest = n
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	return stop, done, func() int { return highest }
}

// requireClaimOrder proves the queued full was claimed before the queued
// incremental of the same policy. started_at is compared as a parsed time, not
// as text, in both the API and SQLite.
func (f *fixture) requireClaimOrder(t *testing.T, fullID, incID string) {
	t.Helper()
	full, inc := f.getRun(t, fullID), f.getRun(t, incID)
	fullStart := parseStamp(t, ptrString(full.StartedAt))
	incStart := parseStamp(t, ptrString(inc.StartedAt))
	if !fullStart.Before(incStart) {
		t.Fatalf("incremental was claimed before full: full=%s inc=%s", fullStart, incStart)
	}
	for _, id := range []string{fullID, incID} {
		stored := parseStamp(t, f.sqliteString(t, `SELECT started_at FROM runs WHERE id = ?`, id))
		api := parseStamp(t, ptrString(map[string]*string{fullID: full.StartedAt, incID: inc.StartedAt}[id]))
		if !stored.Equal(api) {
			t.Fatalf("run %s started_at sqlite=%s api=%s", id, stored, api)
		}
	}
}

// runInterval is one stored [started_at, finished_at) window.
type runInterval struct {
	id         string
	start, end time.Time
}

// requireDisjointRuns reads every stored run interval from SQLite and requires
// that no two overlap: [started_at, finished_at) is the only authority for the
// global backup concurrency. It returns the highest observed concurrency.
func (f *fixture) requireDisjointRuns(t *testing.T) int {
	t.Helper()
	all := f.runIntervals(t)
	sort.Slice(all, func(i, j int) bool { return all[i].start.Before(all[j].start) })
	concurrent, latest := 0, time.Time{}
	for _, item := range all {
		if item.start.Before(latest) {
			t.Fatalf("runs overlap: %s starts at %s before the previous run finished at %s", item.id, item.start, latest)
		}
		if item.end.After(latest) {
			latest = item.end
		}
		if active := overlapping(all, item); active > concurrent {
			concurrent = active
		}
	}
	return concurrent
}

// runIntervals reads every started run's window. A started run without
// finished_at means the concurrency claim cannot be evaluated, so it fails.
func (f *fixture) runIntervals(t *testing.T) []runInterval {
	t.Helper()
	rows, err := f.analysis.QueryContext(t.Context(), `SELECT id, started_at, finished_at FROM runs`)
	must(t, err)
	defer func() { must(t, rows.Close()) }()
	var all []runInterval
	for rows.Next() {
		var id string
		var start, end sql.NullString
		must(t, rows.Scan(&id, &start, &end))
		if !start.Valid {
			continue
		}
		if !end.Valid {
			t.Fatalf("run %s has no finished_at", id)
		}
		all = append(all, runInterval{id: id, start: parseStamp(t, start.String), end: parseStamp(t, end.String)})
	}
	must(t, rows.Err())
	return all
}

// overlapping counts the windows that share a positive duration with one
// window, including itself.
func overlapping(all []runInterval, item runInterval) int {
	active := 0
	for _, other := range all {
		if item.start.Before(other.end) && other.start.Before(item.end) {
			active++
		}
	}
	return active
}

func a3Cancel(t *testing.T, f *fixture) {
	t.Helper()
	policy := f.createPolicy(t, "w4-cancel", 8)
	f.enable(t, policy)
	before := f.completedBackupsForPolicy(t, policy)
	runID := f.triggerRun(t, policy, store.RunKindFull, "w4-cancel-full")
	f.waitXtrabackup(t, runID)
	f.cancel(t, runID)
	run := f.waitRun(t, runID, store.RunStatusCanceled)
	if run.BackupID != nil {
		t.Fatalf("canceled run still has backup_id %s", *run.BackupID)
	}
	if n := f.completedBackupsForRun(t, runID); n != 0 {
		t.Fatalf("canceled run %s published %d completed backups", runID, n)
	}
	if after := f.completedBackupsForPolicy(t, policy); after != before {
		t.Fatalf("policy completed backups changed across the cancel: before=%d after=%d", before, after)
	}
	if during := f.completedBackupsSinceFault(t, runID); during != 0 {
		t.Fatalf("%d backups completed during the cancel window", during)
	}
	if left := f.pendingDirs(); len(left) != 0 {
		t.Fatalf("staging left behind: %s", left)
	}
	if left := f.workDirs(); len(left) != 0 {
		t.Fatalf("work dirs left behind: %s", left)
	}
	inc := f.triggerRun(t, policy, store.RunKindIncremental, "w4-cancel-inc")
	incRun := f.waitRun(t, inc, store.RunStatusFailed)
	if errorClass(incRun) != store.RunErrorRequiresFull {
		t.Fatalf("incremental error_class=%s want requires_full", errorClass(incRun))
	}
	admission, queued := f.admissionFull(policy)
	if !queued {
		t.Fatal("admission full was not queued")
	}
	// The coalesced full is real work: wait for it to reach a terminal state so
	// this scenario leaves nothing in flight for the next scenario's promptness
	// and concurrency measurements.
	f.waitTerminal(t, admission)
	t.Log("W4-A3 cancel canceled=true completed_backup=false staging_reclaimed=true work_reclaimed=true requires_full=true source_backups_unchanged=true")
}

// admissionFull returns the admission-triggered full of one policy, if stored.
func (f *fixture) admissionFull(policyID string) (string, bool) {
	for _, item := range f.listRuns(f.t, "policy_id="+policyID) {
		if item.Trigger == store.RunTriggerAdmission && item.Kind == store.RunKindFull {
			return item.ID, true
		}
	}
	return "", false
}

// waitTerminal waits for any terminal status. Unlike waitRun it accepts a
// failure, which is what a run queued as a side effect of another run's failure
// may legitimately reach.
func (f *fixture) waitTerminal(t *testing.T, id string) store.Run {
	t.Helper()
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		run := f.getRun(t, id)
		switch run.Status {
		case store.RunStatusCompleted, store.RunStatusFailed, store.RunStatusCanceled:
			return run
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("run %s did not reach a terminal status", id)
	return store.Run{}
}

func a4SSE(t *testing.T, f *fixture) {
	t.Helper()
	policy := f.createPolicy(t, "w4-sse", 8)
	f.enable(t, policy)
	runID := f.triggerRun(t, policy, store.RunKindFull, "w4-sse-full")
	triggered := time.Now()
	_, body := f.operator.stream(t, "/api/v1/runs/"+runID+"/events")
	defer func() { _ = body.Close() }()
	frames, end, first := readSSE(t, body, 10*time.Minute)
	latency := first.Sub(triggered)
	if latency > sseFirstEventBound {
		t.Fatalf("first SSE frame arrived %s after the trigger, bound %s", latency, sseFirstEventBound)
	}
	joined := strings.Join(frames, "")
	if end != "completed" {
		t.Fatalf("sse run did not complete: end=%s frames=%s", end, joined)
	}
	requireSSELifecycle(t, frames, joined)
	rows := f.requireLogsAgree(t, runID)
	logs := f.allLogs(t, runID)
	if len(logs) == 0 {
		t.Fatal("run logs empty")
	}
	raw, err := json.Marshal(logs)
	must(t, err)
	f.assertNoSecret(t, append([]byte(joined), raw...))
	t.Logf("W4-A4 sse sse_end=true sse_ordered=true secrets=clean lifecycle=queued<claimed<preflight_ok<started<completed first_event_ms=%d logs_agree=sqlite log_rows=%d",
		latency.Milliseconds(), rows)
}

// requireSSELifecycle requires the frozen ordered lifecycle on the wire: every
// lifecycle event present exactly once, its seq strictly greater than the
// previous lifecycle event's, and the completed frame carrying its payload.
func requireSSELifecycle(t *testing.T, frames []string, joined string) {
	t.Helper()
	seen := map[string]int64{}
	for _, frame := range frames {
		event, seq, ok := structuredFrame(frame)
		if !ok {
			continue
		}
		if _, duplicate := seen[event]; duplicate {
			t.Fatalf("sse repeated %s: %s", event, joined)
		}
		seen[event] = seq
	}
	previous := int64(0)
	for _, name := range []string{"queued", "claimed", "preflight_ok", "started", "completed"} {
		seq, ok := seen[name]
		if !ok {
			t.Fatalf("sse missing %s: %s", name, joined)
		}
		if seq <= previous {
			t.Fatalf("sse lifecycle out of order at %s: %v", name, seen)
		}
		previous = seq
	}
	completed := frameFor(joined, "completed")
	if !strings.Contains(completed, "backup_id") || !strings.Contains(completed, "payload_sha256") {
		t.Fatalf("completed frame carries no payload data: %s", completed)
	}
}

// structuredFrame decodes the data object of one message frame.
func structuredFrame(frame string) (string, int64, bool) {
	if !strings.Contains(frame, "event: message") {
		return "", 0, false
	}
	index := strings.Index(frame, "\ndata: ")
	if index < 0 {
		return "", 0, false
	}
	var payload struct {
		Seq   int64  `json:"seq"`
		Event string `json:"event"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(frame[index+len("\ndata: "):])), &payload); err != nil {
		return "", 0, false
	}
	if payload.Event == "" || payload.Seq == 0 {
		return "", 0, false
	}
	return payload.Event, payload.Seq, true
}

func a3SIGTERM(t *testing.T, f *fixture) {
	t.Helper()
	f.startServeChild(t)
	policy := f.createPolicy(t, "w4-sigterm", 8)
	f.enable(t, policy)
	before := f.completedBackupsForPolicy(t, policy)
	runID := f.triggerRun(t, policy, store.RunKindFull, "w4-sigterm-full")
	f.waitXtrabackup(t, runID)
	f.signalChild(t, syscall.SIGTERM)
	f.waitChild(t, 2*time.Minute)
	// The exit code belongs to the child that received SIGTERM; a restart
	// replaces f.child, so read it before the recovery start.
	exit := f.childExitCode(t)
	if exit != 0 {
		t.Fatalf("serve child exited %d: the bounded drain reported an error", exit)
	}
	// The drain must record a terminal outcome for the interrupted run. A row
	// still `running` after the child exited means the drain silently dropped
	// the outcome, which the contract does not allow.
	status := f.sqliteString(t, `SELECT status FROM runs WHERE id = ?`, runID)
	if status != store.RunStatusCanceled && status != store.RunStatusFailed {
		t.Fatalf("SIGTERM left run %s as %s, want canceled or failed", runID, status)
	}
	if n := f.completedBackupsForRun(t, runID); n != 0 {
		t.Fatalf("SIGTERM published %d completed backups", n)
	}
	if after := f.completedBackupsForPolicy(t, policy); after != before {
		t.Fatalf("policy completed backups changed across SIGTERM: before=%d after=%d", before, after)
	}
	if during := f.completedBackupsSinceFault(t, runID); during != 0 {
		t.Fatalf("%d backups completed during the SIGTERM window", during)
	}
	f.startServeChild(t)
	recovered := f.sqliteString(t, `SELECT status FROM runs WHERE id = ?`, runID)
	if recovered != store.RunStatusCanceled && recovered != store.RunStatusFailed {
		t.Fatalf("recovered SIGTERM run is %s", recovered)
	}
	next := f.triggerRun(t, policy, store.RunKindFull, "w4-sigterm-next")
	f.waitRun(t, next, store.RunStatusCompleted)
	f.assertNoSecret(t, nil)
	t.Logf("W4-A3 sigterm sigterm_exit_bounded=true no_false_completed=true recovered=true status=%s exit=%d", status, exit)
}

func a3SIGKILL(t *testing.T, f *fixture) {
	t.Helper()
	f.startServeChild(t)
	policy := f.createPolicy(t, "w4-sigkill", 8)
	f.enable(t, policy)
	fullID := f.triggerRun(t, policy, store.RunKindFull, "w4-sigkill-full")
	full := f.waitRun(t, fullID, store.RunStatusCompleted)
	if full.BackupID == nil {
		t.Fatal("SIGKILL setup full has no backup")
	}
	published := f.getBackup(t, *full.BackupID)
	beforeHash := f.hashLocal(t, published.ObjectPath)
	// Settle replication first so the hold is measured against a fixed pair of
	// protection points rather than against work still in flight.
	f.waitAllReplicasVerified(t)
	beforeProtection := f.getProtection(t)
	if len(beforeProtection.Remote) != 1 {
		t.Fatalf("before SIGKILL remote protection = %+v, the remote hold would be unproven", beforeProtection.Remote)
	}
	f.dml(t, 9)
	incID := f.triggerRun(t, policy, store.RunKindIncremental, "w4-sigkill-inc")
	f.waitXtrabackup(t, incID)
	f.signalChild(t, syscall.SIGKILL)
	f.waitChild(t, 30*time.Second)
	killed, ok := f.child.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !killed.Signaled() || killed.Signal() != syscall.SIGKILL {
		t.Fatal("child was not SIGKILL")
	}
	f.startServeChild(t)
	f.requireKilledRunRecovered(t, incID)
	f.requirePublishedIntact(t, published, beforeHash, beforeProtection)
	f.requireProtectionHeld(t, beforeProtection)
	f.assertNoSecret(t, nil)
	next := f.triggerRun(t, policy, store.RunKindIncremental, "w4-sigkill-next")
	f.waitRun(t, next, store.RunStatusCompleted)
	t.Log("W4-A3 sigkill sigkill=SIGKILL interrupted=true pending=gone work=gone published_untouched=true protection_held=true next_backup=completed remote_held=true sqlite_interrupted=true")
}

// requireKilledRunRecovered requires the SIGKILLed run to be failed/interrupted
// in both the API and SQLite, and its staging and work directories to be
// reclaimed by recovery.
func (f *fixture) requireKilledRunRecovered(t *testing.T, runID string) {
	t.Helper()
	inc := f.getRun(t, runID)
	if inc.Status != store.RunStatusFailed || errorClass(inc) != store.RunErrorInterrupted {
		t.Fatalf("killed run api status=%s class=%s", inc.Status, errorClass(inc))
	}
	var status, class string
	must(t, f.analysis.QueryRowContext(t.Context(),
		`SELECT status, COALESCE(error_class, '') FROM runs WHERE id = ?`, runID).Scan(&status, &class))
	if status != store.RunStatusFailed || class != store.RunErrorInterrupted {
		t.Fatalf("killed run sqlite status=%s class=%s", status, class)
	}
	if left := f.pendingDirs(); len(left) != 0 {
		t.Fatalf("pending after recovery: %s", left)
	}
	if left := f.workDirs(); len(left) != 0 {
		t.Fatalf("work after recovery: %s", left)
	}
}

// requirePublishedIntact proves recovery never touched a published object and
// never advanced the local protection point.
func (f *fixture) requirePublishedIntact(t *testing.T, published dispatch.BackupView, beforeHash string, before dispatch.Protection) {
	t.Helper()
	afterHash := f.hashLocal(t, published.ObjectPath)
	if afterHash != beforeHash || afterHash != published.PayloadSHA256 {
		t.Fatal("published object changed")
	}
	after := f.getProtection(t)
	if after.Local == nil || before.Local == nil || after.Local.BackupID != before.Local.BackupID {
		t.Fatalf("local protection advanced: before=%+v after=%+v", before.Local, after.Local)
	}
}

// requireProtectionHeld requires both protection points to be unchanged: the
// local point and every remote point per repository.
func (f *fixture) requireProtectionHeld(t *testing.T, before dispatch.Protection) {
	t.Helper()
	after := f.getProtection(t)
	if len(after.Remote) != len(before.Remote) {
		t.Fatalf("remote protection points changed: before=%+v after=%+v", before.Remote, after.Remote)
	}
	for i := range before.Remote {
		if after.Remote[i] != before.Remote[i] {
			t.Fatalf("remote protection advanced: before=%+v after=%+v", before.Remote[i], after.Remote[i])
		}
	}
}

func replicaVerified(replicas []dispatch.ReplicaView, repositoryID string) bool {
	for _, replica := range replicas {
		if replica.RepositoryID == repositoryID && replica.Status == "verified" {
			return true
		}
	}
	return false
}

func longestChain(t *testing.T, chains []dispatch.ChainView) []dispatch.BackupView {
	t.Helper()
	var best []dispatch.BackupView
	for _, chain := range chains {
		if len(chain.Members) > len(best) {
			best = chain.Members
		}
	}
	if len(best) == 0 {
		t.Fatal("no chain members")
	}
	return best
}

func lsnContinuous(members []dispatch.BackupView) bool {
	var prev uint64
	for i, member := range members {
		to, err := strconv.ParseUint(member.ToLSN, 10, 64)
		if err != nil || to == 0 {
			return false
		}
		if i > 0 {
			from, err := strconv.ParseUint(member.FromLSN, 10, 64)
			if err != nil || from != prev {
				return false
			}
			if to <= prev {
				return false
			}
		}
		prev = to
	}
	return true
}

// runsAgree requires every listed run ID to be the web run ID of a completed
// SQLite row and to appear as completed in the API list.
func runsAgree(t *testing.T, f *fixture, listed []store.Run, ids []string) bool {
	t.Helper()
	for _, id := range ids {
		var status string
		if err := f.analysis.QueryRowContext(t.Context(), `SELECT status FROM runs WHERE id = ?`, id).Scan(&status); err != nil {
			return false
		}
		if status != store.RunStatusCompleted {
			return false
		}
		found := false
		for _, run := range listed {
			if run.ID == id && run.Status == store.RunStatusCompleted {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func containsRun(items []store.Run, id string) bool {
	for _, item := range items {
		if item.ID == id {
			return true
		}
	}
	return false
}

func idsOf(items []store.Run) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.ID)
	}
	return out
}

// readSSE reads frames until the end event, returning the frames, the terminal
// status and the arrival time of the first frame.
func readSSE(t *testing.T, body io.Reader, bound time.Duration) ([]string, string, time.Time) {
	t.Helper()
	reader := bufio.NewReader(body)
	deadline := time.Now().Add(bound)
	var frames []string
	var first time.Time
	for time.Now().Before(deadline) {
		frame, err := readFrame(reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				t.Fatal("sse closed before end")
			}
			t.Fatal(err)
		}
		if strings.HasPrefix(frame, ":") {
			continue
		}
		if first.IsZero() {
			first = time.Now()
		}
		frames = append(frames, frame)
		if strings.HasPrefix(frame, "event: end") {
			return frames, endStatus(frame), first
		}
	}
	t.Fatal("sse did not end")
	return nil, "", first
}

// endStatus reads the terminal status out of the single end frame.
func endStatus(frame string) string {
	for _, line := range strings.Split(frame, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "data:"); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func readFrame(reader *bufio.Reader) (string, error) {
	var b strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return b.String(), err
		}
		b.WriteString(line)
		if line == "\n" || line == "\r\n" {
			return b.String(), nil
		}
	}
}

// frameFor returns the first message frame of one structured event.
func frameFor(joined, event string) string {
	index := strings.Index(joined, `"event":"`+event+`"`)
	if index < 0 {
		return ""
	}
	start := strings.LastIndex(joined[:index], "event: message")
	if start < 0 {
		start = index
	}
	end := strings.Index(joined[index:], "\n\n")
	if end < 0 {
		return joined[start:]
	}
	return joined[start : index+end]
}

// requireLogsAgree requires the whole paged log endpoint of one run to equal
// its run_logs rows on seq, event, level and data. The log is append-only, so
// a page read that raced a concurrent event is retried until it settles.
func (f *fixture) requireLogsAgree(t *testing.T, runID string) int {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var listed, stored []store.RunLog
	for time.Now().Before(deadline) {
		stored = f.sqliteLogs(t, runID)
		listed = f.allLogs(t, runID)
		if len(listed) == len(stored) && sameLogs(listed, stored) {
			return len(listed)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("run %s logs disagree: api=%s sqlite=%s", runID, describeLogs(listed), describeLogs(stored))
	return 0
}

func sameLogs(left, right []store.RunLog) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i].RunID != right[i].RunID || left[i].Seq != right[i].Seq ||
			left[i].TS != right[i].TS || left[i].Level != right[i].Level ||
			left[i].Event != right[i].Event || left[i].Message != right[i].Message {
			return false
		}
		if !reflect.DeepEqual(left[i].Data, right[i].Data) {
			return false
		}
	}
	return true
}

func describeLogs(logs []store.RunLog) string {
	parts := make([]string, 0, len(logs))
	for _, entry := range logs {
		parts = append(parts, fmt.Sprintf("%d:%s", entry.Seq, entry.Event))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func toolSHA256() string {
	file, err := os.Open(xtrabackupBin)
	if err != nil {
		return ""
	}
	defer func() { _ = file.Close() }()
	sum := sha256.New()
	_, _ = io.Copy(sum, file)
	return hex.EncodeToString(sum.Sum(nil))
}
