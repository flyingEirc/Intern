//go:build integration

package acceptance

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/flyingEirc/Intern/app/web/internal/dispatch"
	"github.com/flyingEirc/Intern/app/web/internal/store"
	"github.com/flyingEirc/Intern/internal/storage/ftp"
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
	first := e2eIdempotency(t, f)
	out := f.e2eChainRuns(t, first)
	members := e2eMembers(t, f)
	f.e2eObjects(t, members)
	protection := f.e2eProtection(t, members)
	uploaded := "missed"
	if out.uploadedWindow {
		uploaded = "observed"
	}
	t.Logf("W4-A1 e2e chain=seq:1,2,3 lsn_continuous=true replay_run_id=%s conflict=409 replicas_verified=3 protection_local=%s protection_remote=%s protection_requires_verified=true uploaded_window=%s lists_agree=sqlite,local,ftp",
		first, protection.Local.BackupID, protection.Remote[0].BackupID, uploaded)
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
// sequence, including whether the uploaded-but-unverified window was seen.
type e2eRuns struct {
	full, inc1, inc2 store.Run
	uploadedWindow   bool
}

// e2eChainRuns runs full -> DML -> incremental -> DML -> incremental, verifies
// every replica, and polls the uploaded window before each verification.
func (f *fixture) e2eChainRuns(t *testing.T, first string) e2eRuns {
	t.Helper()
	out := e2eRuns{}
	out.full = f.waitRun(t, first, store.RunStatusCompleted)
	if out.full.BackupID == nil || out.full.ChainID == nil {
		t.Fatal("completed full has no backup")
	}
	out.uploadedWindow = observeUploadedNotVerified(t, f, *out.full.BackupID)
	f.waitReplica(t, *out.full.BackupID, "verified")
	out.inc1, out.uploadedWindow = f.e2eIncremental(t, "w4-e2e-inc-1", 1, out.uploadedWindow)
	out.inc2, out.uploadedWindow = f.e2eIncremental(t, "w4-e2e-inc-2", 2, out.uploadedWindow)
	f.e2eCountVerified(t)
	return out
}

// e2eIncremental runs one DML + incremental step to completion, observes the
// uploaded-but-unverified window and requires the verified replica.
func (f *fixture) e2eIncremental(t *testing.T, key string, n int, observed bool) (store.Run, bool) {
	t.Helper()
	f.dml(t, n)
	run := f.waitRun(t, f.triggerRun(t, f.e2ePolicy, store.RunKindIncremental, key), store.RunStatusCompleted)
	seen := observeUploadedNotVerified(t, f, *run.BackupID)
	f.waitReplica(t, *run.BackupID, "verified")
	return run, observed || seen
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
// verified one, and the run list to agree with SQLite.
func (f *fixture) e2eProtection(t *testing.T, members []dispatch.BackupView) dispatch.Protection {
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
	f.requireNoUnverifiedPoint(t, protection)
	listed := f.listRuns(t, "source_id="+f.sourceID)
	if !runsAgree(t, f, listed, []string{members[0].ID, members[1].ID, members[2].ID}) {
		t.Fatal("runs list disagrees with sqlite")
	}
	f.assertNoSecret(t, nil)
	return protection
}

// requireNoUnverifiedPoint proves the remote point is the all-verified prefix:
// no uploaded, uploading or pending replica is ever a protection point.
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

func (f *fixture) requireNoUnverifiedPoint(t *testing.T, protection dispatch.Protection) {
	t.Helper()
	for _, replica := range f.listReplicas(t, "source_id="+f.sourceID) {
		if replica.Status == "verified" {
			continue
		}
		for _, point := range protection.Remote {
			if point.BackupID == replica.BackupID {
				t.Fatalf("remote protection %s advanced on a %s replica", point.BackupID, replica.Status)
			}
		}
	}
}

func a2Priority(t *testing.T, f *fixture) {
	t.Helper()
	policy := f.createPolicy(t, "w4-priority", 8)
	f.enable(t, policy)
	stop, done, peak := f.watchRunning(t)
	full1 := f.triggerRun(t, policy, store.RunKindFull, "w4-pri-full-1")
	f.waitXtrabackup(t, full1)
	full2 := f.triggerRun(t, policy, store.RunKindFull, "w4-pri-full-2")
	inc := f.triggerRun(t, policy, store.RunKindIncremental, "w4-pri-inc")
	queued := f.listRuns(t, "status=queued")
	if !containsRun(queued, full2) || !containsRun(queued, inc) {
		t.Fatalf("expected full and incremental queued together: %+v", idsOf(queued))
	}
	if n := len(f.listRuns(t, "status=running")); n != 1 {
		t.Fatalf("running while queued = %d", n)
	}
	f.waitRun(t, full1, store.RunStatusCompleted)
	f.waitRun(t, full2, store.RunStatusCompleted)
	f.waitRun(t, inc, store.RunStatusCompleted)
	close(stop)
	<-done
	if peak() != 1 {
		t.Fatalf("running_max=%d", peak())
	}
	f.requireClaimOrder(t, full2, inc)
	t.Log("W4-A2 priority running_max=1 claim_full_before_incremental=true queued_together=true")
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
// incremental of the same policy.
func (f *fixture) requireClaimOrder(t *testing.T, fullID, incID string) {
	t.Helper()
	full, inc := f.getRun(t, fullID), f.getRun(t, incID)
	if ptrString(full.StartedAt) == "" || ptrString(inc.StartedAt) == "" {
		t.Fatal("missing started_at")
	}
	if ptrString(full.StartedAt) > ptrString(inc.StartedAt) {
		t.Fatalf("incremental claimed before full: full=%s inc=%s", ptrString(full.StartedAt), ptrString(inc.StartedAt))
	}
}

func a3Cancel(t *testing.T, f *fixture) {
	t.Helper()
	policy := f.createPolicy(t, "w4-cancel", 8)
	f.enable(t, policy)
	runID := f.triggerRun(t, policy, store.RunKindFull, "w4-cancel-full")
	f.waitXtrabackup(t, runID)
	f.cancel(t, runID)
	run := f.waitRun(t, runID, store.RunStatusCanceled)
	if run.BackupID != nil {
		t.Fatalf("canceled run still has backup_id %s", *run.BackupID)
	}
	if n := f.sqliteCount(t, `SELECT COUNT(*) FROM backups WHERE status='completed' AND run_id = ?`, runID); n != 0 {
		t.Fatal("canceled run published a completed backup")
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
	if !f.admissionQueued(policy) {
		t.Fatal("admission full was not queued")
	}
	t.Log("W4-A3 cancel canceled=true completed_backup=false staging_reclaimed=true work_reclaimed=true requires_full=true")
}

// admissionQueued reports whether one admission-triggered full is stored.
func (f *fixture) admissionQueued(policyID string) bool {
	for _, item := range f.listRuns(f.t, "policy_id="+policyID) {
		if item.Trigger == store.RunTriggerAdmission && item.Kind == store.RunKindFull {
			return true
		}
	}
	return false
}

func a4SSE(t *testing.T, f *fixture) {
	t.Helper()
	policy := f.createPolicy(t, "w4-sse", 8)
	f.enable(t, policy)
	runID := f.triggerRun(t, policy, store.RunKindFull, "w4-sse-full")
	_, body := f.operator.stream(t, "/api/v1/runs/"+runID+"/events")
	defer func() { _ = body.Close() }()
	frames, end := readSSE(t, body, 10*time.Minute)
	joined := strings.Join(frames, "")
	if end != "completed" {
		t.Fatalf("sse run did not complete: end=%s frames=%s", end, joined)
	}
	requireSSELifecycle(t, frames, joined)
	logs := f.listLogs(t, runID)
	if len(logs) == 0 {
		t.Fatal("run logs empty")
	}
	raw, err := json.Marshal(logs)
	must(t, err)
	f.assertNoSecret(t, append([]byte(joined), raw...))
	t.Log("W4-A4 sse sse_end=true sse_ordered=true secrets=clean")
}

// requireSSELifecycle requires the frozen ordered lifecycle on the wire, each
// structured frame carrying the run_logs data object.
func requireSSELifecycle(t *testing.T, frames []string, joined string) {
	t.Helper()
	if !sseOrdered(frames) {
		t.Fatalf("sse seq not ordered: %s", joined)
	}
	for _, name := range []string{"queued", "claimed", "preflight_ok", "started", "completed"} {
		if !strings.Contains(joined, `"event":"`+name+`"`) {
			t.Fatalf("sse missing %s: %s", name, joined)
		}
	}
	completed := frameFor(joined, "completed")
	if !strings.Contains(completed, "backup_id") || !strings.Contains(completed, "payload_sha256") {
		t.Fatalf("completed frame carries no payload data: %s", completed)
	}
}

func a3SIGTERM(t *testing.T, f *fixture) {
	t.Helper()
	f.startServeChild(t)
	policy := f.createPolicy(t, "w4-sigterm", 8)
	f.enable(t, policy)
	runID := f.triggerRun(t, policy, store.RunKindFull, "w4-sigterm-full")
	f.waitXtrabackup(t, runID)
	f.signalChild(t, syscall.SIGTERM)
	f.waitChild(t, 2*time.Minute)
	status := f.sqliteString(t, `SELECT status FROM runs WHERE id = ?`, runID)
	if status == store.RunStatusCompleted {
		t.Fatal("SIGTERM run completed")
	}
	if status != store.RunStatusCanceled && status != store.RunStatusFailed && status != store.RunStatusRunning {
		t.Fatalf("SIGTERM run status %s", status)
	}
	if n := f.sqliteCount(t, `SELECT COUNT(*) FROM backups WHERE status='completed' AND run_id = ?`, runID); n != 0 {
		t.Fatal("SIGTERM published a completed backup")
	}
	f.startServeChild(t)
	run := f.getRun(t, runID)
	if run.Status == store.RunStatusCompleted {
		t.Fatal("recovered SIGTERM run is completed")
	}
	if run.Status == store.RunStatusRunning {
		t.Fatal("recovered SIGTERM run still running")
	}
	next := f.triggerRun(t, policy, store.RunKindFull, "w4-sigterm-next")
	f.waitRun(t, next, store.RunStatusCompleted)
	t.Logf("W4-A3 sigterm sigterm_exit_bounded=true no_false_completed=true recovered=true status=%s", run.Status)
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
	beforeProtection := f.getProtection(t)
	f.dml(t, 9)
	incID := f.triggerRun(t, policy, store.RunKindIncremental, "w4-sigkill-inc")
	f.waitXtrabackup(t, incID)
	f.signalChild(t, syscall.SIGKILL)
	f.waitChild(t, 30*time.Second)
	status, ok := f.child.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatal("child was not SIGKILL")
	}
	f.startServeChild(t)
	f.requireKilledRunRecovered(t, incID)
	f.requirePublishedIntact(t, published, beforeHash, beforeProtection)
	next := f.triggerRun(t, policy, store.RunKindIncremental, "w4-sigkill-next")
	f.waitRun(t, next, store.RunStatusCompleted)
	t.Log("W4-A3 sigkill sigkill=SIGKILL interrupted=true pending=gone work=gone published_untouched=true protection_held=true next_backup=completed")
}

// requireKilledRunRecovered requires the SIGKILLed run to be failed/interrupted
// and its staging and work directories to be reclaimed by recovery.
func (f *fixture) requireKilledRunRecovered(t *testing.T, runID string) {
	t.Helper()
	inc := f.getRun(t, runID)
	if inc.Status != store.RunStatusFailed || errorClass(inc) != store.RunErrorInterrupted {
		t.Fatalf("killed run status=%s class=%s", inc.Status, errorClass(inc))
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
		t.Fatalf("protection advanced: before=%+v after=%+v", before.Local, after.Local)
	}
}

// observeUploadedNotVerified polls the replica of one backup while it is not
// yet verified and requires the remote protection point to stay behind it. It
// reports whether the uploaded window was actually observed; the cross-check
// against the verified prefix runs either way.
func observeUploadedNotVerified(t *testing.T, f *fixture, backupID string) bool {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	observed := false
	for time.Now().Before(deadline) {
		if f.replicaSettled(t, backupID, &observed) {
			return observed
		}
		time.Sleep(50 * time.Millisecond)
	}
	return observed
}

// replicaSettled inspects one replica row: it records an observed unverified
// window, requires the remote point to stay behind it, and reports whether the
// replica already reached verified.
func (f *fixture) replicaSettled(t *testing.T, backupID string, observed *bool) bool {
	t.Helper()
	for _, item := range f.listReplicas(t, "source_id="+f.sourceID) {
		if item.BackupID != backupID {
			continue
		}
		if item.Status == "verified" {
			return true
		}
		if item.Status != "uploaded" && item.Status != "uploading" && item.Status != "pending" {
			continue
		}
		*observed = true
		for _, point := range f.getProtection(t).Remote {
			if point.BackupID == backupID {
				t.Fatalf("remote protection advanced on a %s replica", item.Status)
			}
		}
	}
	return false
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

func runsAgree(t *testing.T, f *fixture, listed []store.Run, ids []string) bool {
	t.Helper()
	want := map[string]struct{}{}
	for _, id := range ids {
		want[id] = struct{}{}
	}
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

func readSSE(t *testing.T, body io.Reader, bound time.Duration) ([]string, string) {
	t.Helper()
	reader := bufio.NewReader(body)
	deadline := time.Now().Add(bound)
	var frames []string
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
		frames = append(frames, frame)
		if strings.HasPrefix(frame, "event: end") {
			return frames, endStatus(frame)
		}
	}
	t.Fatal("sse did not end")
	return nil, ""
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

func sseOrdered(frames []string) bool {
	prev := int64(0)
	seen := false
	for _, frame := range frames {
		if !strings.Contains(frame, "event: message") {
			continue
		}
		idx := strings.Index(frame, `"seq":`)
		if idx < 0 {
			continue
		}
		rest := frame[idx+6:]
		n := int64(0)
		for _, r := range rest {
			if r < '0' || r > '9' {
				break
			}
			n = n*10 + int64(r-'0')
		}
		if seen && n <= prev {
			return false
		}
		prev = n
		seen = true
	}
	return seen
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
