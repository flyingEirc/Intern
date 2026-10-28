package dispatch

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/flyingEirc/Intern/app/web/internal/store"
	physicalstore "github.com/flyingEirc/Intern/internal/physical/store"
	"github.com/flyingEirc/Intern/internal/replication"
)

// TestRoundRobinDoesNotStarveTheSecondReplicator is D4: a persistent cursor
// gives every replicator a turn, so a busy first slot cannot monopolize Step.
func TestRoundRobinDoesNotStarveTheSecondReplicator(t *testing.T) {
	var order []int
	step := func(i int) (bool, error) {
		order = append(order, i)
		return true, nil
	}
	next := 0
	for range 3 {
		var err error
		var did bool
		next, did, err = roundRobin(2, next, step)
		if err != nil || !did {
			t.Fatalf("round-robin step: next=%d did=%v err=%v", next, did, err)
		}
	}
	if !slices.Equal(order, []int{0, 1, 0}) {
		t.Fatalf("order = %v, want [0 1 0]", order)
	}
}

// TestStartSucceedsWhenNewestRepositoryIsFTP is D5: serve starts when the
// newest repository row is FTP. Replicators are opened from policy pairs, not
// from Repositories(limit=1).
func TestStartSucceedsWhenNewestRepositoryIsFTP(t *testing.T) {
	f := newFixture(t)
	local := f.repository("local-first")
	ftp, err := f.database.CreateRepository(t.Context(), store.RepositoryInput{
		Name: "ftp-newest", Kind: store.RepositoryFTP, TLSMode: store.TLSModePlain,
		FTP:    &store.FTPRepositoryConfig{Host: "127.0.0.1", Port: 21, BasePath: "/b", Username: "u"},
		Secret: []byte("{{SECRET_ftp_newest}}"),
	}, f.sealer, store.SystemAudit("repositories.create", store.AuditDetail{}))
	if err != nil {
		t.Fatal(err)
	}
	if ftp.ID <= local.ID {
		t.Fatalf("ftp id %s is not newer than local %s", ftp.ID, local.ID)
	}
	d := f.start(nil)
	if n := len(d.replicaSnapshot()); n != 0 {
		t.Fatalf("opened %d replicators without a replica policy", n)
	}
}

// TestReplicatorOpensForPolicyCreatedAfterStart is the post-Start policy path:
// a local+FTP pair created after Start still gets a replicator when a completed
// backup is enqueued, and the replica row reaches that replicator.
func TestReplicatorOpensForPolicyCreatedAfterStart(t *testing.T) {
	f := newFixture(t)
	useMemoryRemote(t)
	d := f.start(nil)
	local := f.repository("local-after")
	ftp := f.ftpRepository("ftp-after")
	source := f.source("source-after", testSocketPath(t))
	policy := f.replicaPolicy("policy-after", source.ID, local.ID, ftp.ID)
	if d.replicatorForPair(local.ID, ftp.ID) != nil {
		t.Fatal("replicator opened before enqueue")
	}
	backupID, chainID := completeLocalBackup(t, f.database, source.ID)
	run := insertTerminalRun(t, f.database, policy, backupID, chainID)
	d.enqueueReplication(t.Context(), run, policy, backupID)
	replicator := d.replicatorForPair(local.ID, ftp.ID)
	if replicator == nil {
		t.Fatal("missing lazy replicator")
	}
	replica, err := replicator.Replica(t.Context(), backupID)
	if err != nil {
		t.Fatal(err)
	}
	if replica.BackupID != backupID || replica.RepositoryID != ftp.ID {
		t.Fatalf("replica = %+v", replica)
	}
}

// TestFailedReplicatorOpenIsRetriedAndRecorded drives the open-failure rule: a
// broken remote becomes a structured replication_step event with code
// replicator_unavailable, the run stays completed, and the worker's retried open
// itself queues the backup. Nothing enqueues it a second time by hand: the
// reopen's catch-up is what makes the replica durable.
func TestFailedReplicatorOpenIsRetriedAndRecorded(t *testing.T) {
	f := newFixture(t)
	d := f.start(nil)
	local := f.repository("local-broken")
	ftp := f.ftpRepository("ftp-broken")
	source := f.source("source-broken", testSocketPath(t))
	policy := f.replicaPolicy("policy-broken", source.ID, local.ID, ftp.ID)

	// The open itself fails while the seam is broken; restoring the seam is the
	// remote becoming reachable again.
	restoreBroken := useBrokenRemote(t)
	backupID, chainID := completeLocalBackup(t, f.database, source.ID)
	run := insertTerminalRun(t, f.database, policy, backupID, chainID)
	d.enqueueReplication(t.Context(), run, policy, backupID)
	restoreBroken()
	useMemoryRemote(t)
	if d.replicatorForPair(local.ID, ftp.ID) != nil {
		t.Fatal("a failed open must not install a replicator")
	}
	assertNoQueuedReplica(t, f, backupID, ftp.ID)
	step := eventByName(t, f.events(run.ID), eventReplicationStep)
	data := asObject(t, step.Data)
	if data["status"] != "failed" || data["code"] != replicatorUnavailable || data["repository_id"] != ftp.ID {
		t.Fatalf("replication_step data = %+v, want failed/%s/%s", data, replicatorUnavailable, ftp.ID)
	}
	if final := f.run(run.ID); final.Status != store.RunStatusCompleted {
		t.Fatalf("run status = %s, the failed open must never fail the backup", final.Status)
	}

	// The worker's next cycle retries the open. The reopen runs the pair's
	// catch-up, so the backup completed while the remote was down is queued by
	// that catch-up alone. Stop covers the lazily opened replicator's handles.
	waitFor(t, "the retried open to install the replicator", func() bool {
		d.stepReplication(t.Context())
		return d.replicatorForPair(local.ID, ftp.ID) != nil
	})
	assertQueuedReplica(t, f, backupID, ftp.ID)
	if err := d.Stop(); err != nil {
		t.Fatal(err)
	}
	if d.replicaSnapshot() != nil {
		t.Fatal("Stop left replicator handles installed")
	}
}

// TestReplicaSurvivesAReplicaOutageAcrossRestart is the N1 restart variant: the
// open fails, the process restarts with the FTP repository available, and the
// pair opened during recovery's startup catch-up owns the replica row. No step
// ran and nothing enqueued by hand, so the row proves the open itself is durable.
func TestReplicaSurvivesAReplicaOutageAcrossRestart(t *testing.T) {
	f := newFixture(t)
	d := f.start(nil)
	local := f.repository("local-restart")
	ftp := f.ftpRepository("ftp-restart")
	source := f.source("source-restart", testSocketPath(t))
	policy := f.replicaPolicy("policy-restart", source.ID, local.ID, ftp.ID)
	backupID, chainID := completeLocalBackup(t, f.database, source.ID)
	run := insertTerminalRun(t, f.database, policy, backupID, chainID)

	// The remote is unreachable for the whole first process.
	restoreBroken := useBrokenRemote(t)
	d.enqueueReplication(t.Context(), run, policy, backupID)
	if d.replicatorForPair(local.ID, ftp.ID) != nil {
		t.Fatal("a failed open must not install a replicator")
	}
	assertNoQueuedReplica(t, f, backupID, ftp.ID)
	if err := d.Stop(); err != nil {
		t.Fatal(err)
	}
	restoreBroken()

	// The restart: a new Store connection over the same metadata directory and a
	// new dispatcher whose startup finds the FTP repository reachable.
	useMemoryRemote(t)
	f.reopen()
	restarted := f.start(nil)
	waitFor(t, "the restarted process to install the pair", func() bool {
		return restarted.replicatorForPair(local.ID, ftp.ID) != nil
	})
	assertQueuedReplica(t, f, backupID, ftp.ID)
}

// assertQueuedReplica requires the enqueued backup to sit in the replicas
// queue for the repository.
func assertQueuedReplica(t *testing.T, f *fixture, backupID, repositoryID string) {
	t.Helper()
	if queued := queuedReplicas(t, f, backupID, repositoryID); queued != 1 {
		t.Fatalf("queued replicas = %d, want the enqueued backup on the recovered pair", queued)
	}
}

// assertNoQueuedReplica requires the backup to have no replica row yet, so a
// later row proves the catch-up wrote it.
func assertNoQueuedReplica(t *testing.T, f *fixture, backupID, repositoryID string) {
	t.Helper()
	if queued := queuedReplicas(t, f, backupID, repositoryID); queued != 0 {
		t.Fatalf("queued replicas = %d, want none while the pair is uninstalled", queued)
	}
}

func queuedReplicas(t *testing.T, f *fixture, backupID, repositoryID string) int {
	t.Helper()
	var queued int
	if err := f.database.DB().QueryRowContext(f.t.Context(),
		`SELECT COUNT(*) FROM replicas WHERE backup_id = ? AND repository_id = ?`, backupID, repositoryID,
	).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	return queued
}

// useBrokenRemote makes the lazy open itself fail, which is the FTP-refused
// shape ensurePolicyReplicator must survive.
func useBrokenRemote(t *testing.T) (restore func()) {
	t.Helper()
	original := openRemote
	openRemote = func(context.Context, ftpConfig) (replication.Remote, func() error, error) {
		return nil, nil, errors.New("failing remote: connection refused")
	}
	return func() { openRemote = original }
}

func useMemoryRemote(t *testing.T) {
	t.Helper()
	original := openRemote
	openRemote = func(context.Context, ftpConfig) (replication.Remote, func() error, error) {
		return memoryRemote{}, func() error { return nil }, nil
	}
	t.Cleanup(func() { openRemote = original })
}

type memoryRemote struct{}

func (memoryRemote) Create(context.Context, string) (replication.Sink, error) {
	return nil, errors.New("memory remote: create unused")
}

func (memoryRemote) Open(context.Context, string, int64) (io.ReadCloser, error) {
	return nil, errors.New("memory remote: open unused")
}

func (memoryRemote) Size(context.Context, string) (int64, error) {
	return 0, errors.New("memory remote: size unused")
}

func (memoryRemote) CleanupPart(context.Context, string) error { return nil }

func completeLocalBackup(t *testing.T, database *store.Store, sourceID string) (backupID, chainID string) {
	t.Helper()
	physical := physicalstore.New(database.DB())
	plan, err := physical.Plan(t.Context(), physicalstore.PlanRequest{
		SourceID: sourceID, ServerUUID: "server-uuid", ServerVersion: "8.4.11",
		ToolVersion: "8.4.0-7", Kind: "full", MaxChainLength: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	backupID = "backup-lazy-repl"
	if err = physical.Begin(t.Context(), physicalstore.BeginRequest{
		Plan: plan, SourceID: sourceID, ServerUUID: "server-uuid", ServerVersion: "8.4.11",
		ToolVersion: "8.4.0-7", InstanceID: database.InstanceID(), RunID: backupID, BackupID: backupID,
		ObjectPath:   database.InstanceID() + "/" + sourceID + "/" + plan.ChainID + "/" + backupID,
		StagingToken: backupID, WorkDir: filepath.Join(t.TempDir(), "work-"+backupID),
		KeyID: "key", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err = physical.Complete(t.Context(), physicalstore.Completion{
		BackupID: backupID, FromLSN: 0, ToLSN: 100, LastLSN: 100,
		CheckpointsRaw: []byte("backup_type = full-backuped\nfrom_lsn = 0\nto_lsn = 100\nlast_lsn = 100\n"),
		InfoRaw:        []byte("tool_name = xtrabackup\n"), PayloadSize: 512, PayloadSHA256: "payload",
		MetadataSize: 128, MetadataSHA256: "metadata", CompletedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	return backupID, plan.ChainID
}

func insertTerminalRun(t *testing.T, database *store.Store, policy store.Policy, backupID, chainID string) store.Run {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	now := store.ScheduleStamp(time.Now())
	_, err = database.DB().ExecContext(t.Context(), `
INSERT INTO runs (id, policy_id, source_id, kind, "trigger", idempotency_key, request_hash, status, scheduled_at, created_at, started_at, finished_at, backup_id, chain_id, payload_size, local_repository_id, replica_repository_id)
VALUES (?, ?, ?, 'full', 'manual', ?, 'lazy-repl', 'completed', ?, ?, ?, ?, ?, ?, 512, ?, ?)`,
		id.String(), policy.ID, policy.SourceID, "lazy-repl:"+id.String(), now, now, now, now, backupID, chainID,
		policy.LocalRepositoryID, policy.ReplicaRepositoryID)
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.Run(t.Context(), id.String())
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// TestKEKInsideRepositoryFailsTheRun is D6: moving the KEK into the policy's
// local repository after the row exists fails the run with preflight /
// kek_inside_repository before chain.Run.
func TestKEKInsideRepositoryFailsTheRun(t *testing.T) {
	q := newQueueFixture(t)
	inside := filepath.Join(q.repository.Local.Root, "kek.json")
	payload, err := os.ReadFile(q.keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inside, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	q.runtime.KeyFile = inside
	run := q.queued("kek-inside", store.RunKindFull, store.RunTriggerManual, time.Now().UTC())
	q.start(nil)

	final := q.waitForRunState(run.ID, store.RunStatusFailed, store.RunErrorPreflight)
	if final.ErrorCode == nil || *final.ErrorCode != "kek_inside_repository" {
		t.Fatalf("error code = %v, want kek_inside_repository", final.ErrorCode)
	}
	failed := eventByName(t, q.events(run.ID), eventPreflightFailed)
	data := asObject(t, failed.Data)
	if data["check"] != "kek" || data["code"] != "kek_inside_repository" {
		t.Fatalf("preflight_failed data = %+v", data)
	}
	assertNoSecrets(t, q, run.ID)
}

// TestAdmissionCoalescesTwoRequiresFullFailuresToOneFull is D7: two
// RequiresFull failures queue exactly one admission full for the source.
func TestAdmissionCoalescesTwoRequiresFullFailuresToOneFull(t *testing.T) {
	q := newQueueFixture(t)
	d := q.newDispatcher(time.Now)
	inc := q.queued("inc-requires-full", store.RunKindIncremental, store.RunTriggerCron, time.Now().UTC())
	if _, ok, err := q.database.ClaimRun(t.Context()); err != nil || !ok {
		t.Fatalf("claim: %v ok=%v", err, ok)
	}
	admission := &physicalstore.AdmissionError{Code: "requires_full", RequiresFull: true}
	if err := d.admissionFailure(t.Context(), inc, q.policy, admission); err != nil {
		t.Fatal(err)
	}
	d.queueAdmissionFull(t.Context(), inc, q.policy)
	if n := countAdmissionFulls(q); n != 1 {
		t.Fatalf("admission fulls = %d, want 1", n)
	}
	assertRequiresFull(t, q.run(inc.ID))
}

// TestAdmissionFullAfterAFailedAdmissionFull is N3: an admission full that fails
// before a chain exists must not block the next one. The key is the contract's
// per-attempt policy:<id>:full:admission:<scheduled_at>, so the second
// RequiresFull queues a fresh full instead of colliding with the failed row.
func TestAdmissionFullAfterAFailedAdmissionFull(t *testing.T) {
	q := newQueueFixture(t)
	d := q.newDispatcher(time.Now)
	admission := &physicalstore.AdmissionError{Code: "requires_full", RequiresFull: true}

	first := refusedIncremental(t, q, d, admission, "inc-first")
	if len(first) != 1 {
		t.Fatalf("admission fulls = %d, want 1", len(first))
	}
	// The admission full fails at preflight, exactly as a broken source does, and
	// releases the source's single full slot.
	claimRun(t, q, "the admission full")
	if err := d.failRun(t.Context(), first[0], store.RunErrorPreflight, "source_unreachable", errors.New("preflight")); err != nil {
		t.Fatal(err)
	}

	refusedIncremental(t, q, d, admission, "inc-second")
	if n := len(admissionFulls(q)); n != 2 {
		t.Fatalf("admission fulls = %d, want a new full after the failed one", n)
	}
	if active, err := q.database.HasActiveFull(t.Context(), q.source.ID); err != nil || !active {
		t.Fatalf("active full = %v, %v; want the new admission full queued", active, err)
	}
}

// refusedIncremental claims one queued incremental, refuses it the way the
// physical admission does, and returns the admission fulls the refusal queued.
func refusedIncremental(t *testing.T, q *queueFixture, d *Dispatcher,
	admission *physicalstore.AdmissionError, name string,
) []store.Run {
	t.Helper()
	inc := q.queued(name, store.RunKindIncremental, store.RunTriggerCron, time.Now().UTC())
	claimRun(t, q, name)
	if err := d.admissionFailure(t.Context(), inc, q.policy, admission); err != nil {
		t.Fatal(err)
	}
	return admissionFulls(q)
}

// claimRun claims the next queued run and requires the claim to succeed.
func claimRun(t *testing.T, q *queueFixture, what string) {
	t.Helper()
	if _, ok, err := q.database.ClaimRun(t.Context()); err != nil || !ok {
		t.Fatalf("claim %s: %v ok=%v", what, err, ok)
	}
}

// admissionFulls lists the admission-triggered fulls of the fixture policy,
// newest first.
func admissionFulls(q *queueFixture) []store.Run {
	fulls := make([]store.Run, 0, 2)
	for _, run := range q.runs(q.policy.ID) {
		if run.Kind == store.RunKindFull && run.Trigger == store.RunTriggerAdmission {
			fulls = append(fulls, run)
		}
	}
	return fulls
}

// TestStructuredRunEventsHaveNoSecrets is D1: lifecycle events carry a fixed
// redacted message plus a data object, and never contain secrets.
func TestStructuredRunEventsHaveNoSecrets(t *testing.T) {
	q := newQueueFixture(t)
	run := q.queued("structured-events", store.RunKindFull, store.RunTriggerManual, time.Now().UTC())
	q.start(nil)
	final := q.waitForRunState(run.ID, store.RunStatusFailed, store.RunErrorPreflight)
	if final.ErrorCode == nil {
		t.Fatal("missing error code")
	}
	logs := q.events(run.ID)
	claimed := eventByName(t, logs, eventClaimed)
	if claimed.Message != eventMessage(eventClaimed) {
		t.Fatalf("claimed message = %q", claimed.Message)
	}
	preflight := eventByName(t, logs, eventPreflightFailed)
	data := asObject(t, preflight.Data)
	if data["check"] == "" || data["code"] == "" {
		t.Fatalf("preflight_failed data = %+v, want check and code", data)
	}
	failed := eventByName(t, logs, eventFailed)
	failedData := asObject(t, failed.Data)
	if failedData["error_class"] != store.RunErrorPreflight || failedData["code"] != *final.ErrorCode {
		t.Fatalf("failed data = %+v", failedData)
	}
	assertNoSecrets(t, q, run.ID)
}

func countAdmissionFulls(q *queueFixture) int {
	n := 0
	for _, run := range q.runs(q.policy.ID) {
		if run.Kind == store.RunKindFull && run.Trigger == store.RunTriggerAdmission {
			n++
		}
	}
	return n
}

func assertRequiresFull(t *testing.T, run store.Run) {
	t.Helper()
	if run.Status != store.RunStatusFailed {
		t.Fatalf("incremental status = %s, want failed", run.Status)
	}
	if run.ErrorClass == nil || *run.ErrorClass != store.RunErrorRequiresFull {
		t.Fatalf("incremental class = %v, want requires_full", run.ErrorClass)
	}
}

func eventByName(t *testing.T, logs []store.RunLog, name string) store.RunLog {
	t.Helper()
	for _, row := range logs {
		if row.Event == name {
			return row
		}
	}
	t.Fatalf("missing event %s in %v", name, eventNamesOf(logs))
	return store.RunLog{}
}

func eventNamesOf(logs []store.RunLog) []string {
	names := make([]string, 0, len(logs))
	for _, row := range logs {
		names = append(names, row.Event)
	}
	return names
}

func asObject(t *testing.T, data any) map[string]any {
	t.Helper()
	object, ok := data.(map[string]any)
	if !ok {
		t.Fatalf("data = %T %+v, want object", data, data)
	}
	return object
}

func assertNoSecrets(t *testing.T, q *queueFixture, runID string) {
	t.Helper()
	raw, err := json.Marshal(q.events(runID))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, secret := range []string{"{{SECRET_dispatch_source}}", q.keyFile} {
		if strings.Contains(body, secret) {
			t.Fatalf("secret %q leaked in run events", secret)
		}
	}
}
