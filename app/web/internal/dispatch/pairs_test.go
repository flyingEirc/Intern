package dispatch

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/flyingEirc/Intern/app/web/internal/store"
	physicalstore "github.com/flyingEirc/Intern/internal/physical/store"
	"github.com/flyingEirc/Intern/internal/replication"
)

// sharedFTPFixture builds the advisor's reproduction: two local repositories
// (l1, l2) and one FTP repository r that both replicate to. Each local
// repository has one policy for the same source, so r serves two pairs:
// (l1, r) and (l2, r). Both pairs are installed with the memory remote, so the
// fixture never dials a network.
type sharedFTPFixture struct {
	*fixture
	dispatcher  *Dispatcher
	l1, l2, ftp store.Repository
	source      store.Source
	policyA     store.Policy // local l1, replica ftp
	policyB     store.Policy // local l2, replica ftp
}

func newSharedFTPFixture(t *testing.T) *sharedFTPFixture {
	t.Helper()
	f := newFixture(t)
	useMemoryRemote(t)
	// The dispatcher is not started: its replication worker would step the
	// queue concurrently with the test's setup. The tests drive openPair and
	// the read paths directly, and the restart variants use recover().
	d := f.newDispatcher(time.Now)
	l1 := f.repository("shared-l1")
	l2 := f.repository("shared-l2")
	ftp := f.ftpRepository("shared-ftp")
	source := f.source("shared-source", testSocketPath(t))
	policyA := f.replicaPolicy("shared-policy-a", source.ID, l1.ID, ftp.ID)
	policyB := f.replicaPolicy("shared-policy-b", source.ID, l2.ID, ftp.ID)
	return &sharedFTPFixture{fixture: f, dispatcher: d, l1: l1, l2: l2, ftp: ftp, source: source, policyA: policyA, policyB: policyB}
}

// install opens both pairs of the shared FTP repository exactly as startup
// recovery does, including each pair's catch-up.
func (s *sharedFTPFixture) install(t *testing.T) {
	t.Helper()
	if err := s.dispatcher.openPair(t.Context(), s.l1.ID, s.ftp.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.dispatcher.openPair(t.Context(), s.l2.ID, s.ftp.ID); err != nil {
		t.Fatal(err)
	}
}

// completeForPolicy publishes one completed local backup whose producing run
// names policy, so PairBackups can scope it. Each backup needs a distinct id,
// because the physical backups table keys on it.
func (s *sharedFTPFixture) completeForPolicy(t *testing.T, policy store.Policy, backupID string) (backupIDResult, chainID string) {
	t.Helper()
	physical := physicalstore.New(s.database.DB())
	plan, err := physical.Plan(t.Context(), physicalstore.PlanRequest{
		SourceID: s.source.ID, ServerUUID: "server-uuid", ServerVersion: "8.4.11",
		ToolVersion: "8.4.0-7", Kind: "full", MaxChainLength: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = physical.Begin(t.Context(), physicalstore.BeginRequest{
		Plan: plan, SourceID: s.source.ID, ServerUUID: "server-uuid", ServerVersion: "8.4.11",
		ToolVersion: "8.4.0-7", InstanceID: s.database.InstanceID(), RunID: backupID, BackupID: backupID,
		ObjectPath:   s.database.InstanceID() + "/" + s.source.ID + "/" + plan.ChainID + "/" + backupID,
		StagingToken: backupID, WorkDir: s.workRoot() + "/run-" + backupID,
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
	run := insertTerminalRun(t, s.database, policy, backupID, plan.ChainID)
	return backupID, run.ID
}

// TestSharedFTPRepositoryKeepsBothPairsAnswers shows the F1 fan-out: every API
// that answers for one FTP repository must visit both pairs sharing it, and
// restart recovery must reclaim both pairs. This is the advisor's table, one
// assertion group per call, with both pairs' replicas verified.
func TestSharedFTPRepositoryKeepsBothPairsAnswers(t *testing.T) {
	s := newSharedFTPFixture(t)
	backupA, _ := s.completeForPolicy(t, s.policyA, "acc-b1")
	backupB, _ := s.completeForPolicy(t, s.policyB, "acc-b2")
	s.install(t)
	d := s.dispatcher

	// Each pair's catch-up binds exactly its own backup.
	if d.replicatorForPair(s.l1.ID, s.ftp.ID) == nil || d.replicatorForPair(s.l2.ID, s.ftp.ID) == nil {
		t.Fatal("the two pairs of the shared FTP repository were not installed")
	}
	assertQueuedReplica(t, s.fixture, backupA, s.ftp.ID)
	assertQueuedReplica(t, s.fixture, backupB, s.ftp.ID)
	assertNoQueuedReplica(t, s.fixture, backupA, "unrelated-ftp")

	// Record both replicas verified, exactly as a completed copy looks, so the
	// read paths below have real state to union.
	markReplicaVerified(t, s.database, backupA, s.ftp.ID, s.l1.ID)
	markReplicaVerified(t, s.database, backupB, s.ftp.ID, s.l2.ID)

	assertSharedProtection(t, s, backupA)
	assertSharedReplicaList(t, s)
	assertSharedBackupViews(t, s, backupA, backupB)
	assertSharedRetry(t, s, backupA, backupB)
	assertSharedRecovery(t, s, backupA, backupB)
}

// assertSharedProtection requires the remote point to be visible through both
// pairs. Before the fix the loop deduplicated on the repository and used
// whichever pair came first, hiding the other pair's verified replica.
func assertSharedProtection(t *testing.T, s *sharedFTPFixture, earliest string) {
	t.Helper()
	protection, err := s.dispatcher.Protection(t.Context(), s.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if protection.Local == nil {
		t.Fatal("protection lost the local point")
	}
	if len(protection.Remote) != 1 {
		t.Fatalf("remote points = %+v, want one for the shared repository", protection.Remote)
	}
	if protection.Remote[0].RepositoryID != s.ftp.ID || protection.Remote[0].BackupID != earliest {
		t.Fatalf("remote point = %+v, want the earliest verified backup %s", protection.Remote[0], earliest)
	}
}

// assertSharedReplicaList requires the union over both pairs: two rows, not the
// one row a single arbitrary pair's queue would report.
func assertSharedReplicaList(t *testing.T, s *sharedFTPFixture) {
	t.Helper()
	for _, filter := range []ReplicaFilter{{}, {SourceID: s.source.ID}} {
		views, err := s.dispatcher.Replicas(t.Context(), filter)
		if err != nil {
			t.Fatal(err)
		}
		if len(views) != 2 {
			t.Fatalf("replica views for %+v = %+v, want both pairs' rows", filter, views)
		}
		for _, view := range views {
			if view.Status != "verified" {
				t.Fatalf("replica view = %+v, want both verified", view)
			}
		}
	}
}

// assertSharedBackupViews requires each backup to show its own replica row.
func assertSharedBackupViews(t *testing.T, s *sharedFTPFixture, backupA, backupB string) {
	t.Helper()
	for _, backupID := range []string{backupA, backupB} {
		view, err := s.dispatcher.BackupView(t.Context(), backupID)
		if err != nil {
			t.Fatal(err)
		}
		if len(view.Replicas) != 1 {
			t.Fatalf("backup %s replicas = %+v, want one row", backupID, view.Replicas)
		}
		if view.Replicas[0].RepositoryID != s.ftp.ID {
			t.Fatalf("backup %s replica repository = %s", backupID, view.Replicas[0].RepositoryID)
		}
	}
}

// assertSharedRetry requires ReplicaRetry to resolve the backup's own pair
// instead of answering not-found for the second pair's backup.
func assertSharedRetry(t *testing.T, s *sharedFTPFixture, backupA, backupB string) {
	t.Helper()
	d := s.dispatcher
	if err := d.ReplicaRetry(t.Context(), backupB, s.ftp.ID); err != nil {
		t.Fatalf("retry of the second pair's backup: %v", err)
	}
	replicator := d.replicatorForPair(s.l2.ID, s.ftp.ID)
	replica, err := replicator.Replica(t.Context(), backupB)
	if err != nil {
		t.Fatal(err)
	}
	if replica.Status != "pending" {
		t.Fatalf("retried replica status = %s, want pending", replica.Status)
	}
	if _, err := replicator.Replica(t.Context(), backupA); err == nil {
		t.Fatal("the l2 replicator must not see the l1 pair's row")
	}
	if err := d.ReplicaRetry(t.Context(), backupA, s.ftp.ID); err != nil {
		t.Fatalf("retry of the first pair's backup: %v", err)
	}
	if err := d.ReplicaRetry(t.Context(), "missing-backup", s.ftp.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("retry of an unknown backup = %v, want not found", err)
	}
}

// assertSharedRecovery requires startup recovery to reclaim both pairs: the
// second pair's interrupted upload returns to pending and its temporary part is
// cleaned by its own replicator.
func assertSharedRecovery(t *testing.T, s *sharedFTPFixture, backupA, backupB string) {
	t.Helper()
	uploadingReplica(t, s.database, backupA, s.ftp.ID, s.l1.ID)
	uploadingReplica(t, s.database, backupB, s.ftp.ID, s.l2.ID)
	partID := insertReplicaPart(t, s.database, backupB, s.ftp.ID, "part/acc-b2-temp")
	if err := s.dispatcher.recover(t.Context()); err != nil {
		t.Fatalf("recovery over both pairs: %v", err)
	}
	for _, pair := range []struct {
		localID, backupID string
	}{{s.l1.ID, backupA}, {s.l2.ID, backupB}} {
		replica, err := s.dispatcher.replicatorForPair(pair.localID, s.ftp.ID).Replica(t.Context(), pair.backupID)
		if err != nil || replica.Status != "pending" {
			t.Fatalf("replica of pair %s after recovery = %+v, %v; want pending", pair.localID, replica, err)
		}
	}
	assertReplicaPartGone(t, s.database, partID)
}

// TestSharedFTPCatchUpBindsOnlyItsOwnBackups is the advisor's F2 reproduction:
// source s with pA (local l1, no replica) and pB (local l2, FTP). pA's backup
// is never bound to the l2 pair, and pB's own backup reaches the replicator and
// keeps the FIFO moving.
func TestSharedFTPCatchUpBindsOnlyItsOwnBackups(t *testing.T) {
	s := newSharedFTPFixture(t)
	// pA has no replica: delete the l1 policy's replica binding so only the
	// plain (l1, no replica) policy remains.
	if _, err := s.database.DB().ExecContext(t.Context(),
		`UPDATE policies SET replica_repository_id = NULL WHERE id = ?`, s.policyA.ID); err != nil {
		t.Fatal(err)
	}

	backupA, _ := s.completeForPolicy(t, s.policyA, "acc2-bA") // produced on l1
	backupB, _ := s.completeForPolicy(t, s.policyB, "acc2-bB") // produced on l2, FTP

	d := s.dispatcher
	if err := d.openPair(t.Context(), s.l2.ID, s.ftp.ID); err != nil {
		t.Fatal(err)
	}
	assertQueuedReplica(t, s.fixture, backupB, s.ftp.ID)
	assertNoQueuedReplica(t, s.fixture, backupA, s.ftp.ID)

	// The l2 pair's FIFO keeps stepping its own backup: no mis-bound row for
	// the l1 backup can block it. The memory remote cannot copy, so the attempt
	// itself may fail; what matters is which backup it named.
	replicator := d.replicatorForPair(s.l2.ID, s.ftp.ID)
	stepped, attempted, _ := replicator.StepItem(t.Context())
	if !attempted {
		t.Fatal("the pair's own backup was never stepped")
	}
	if stepped.BackupID != backupB {
		t.Fatalf("stepped backup = %s, want the pair's own backup %s", stepped.BackupID, backupB)
	}

	// The F2 failure mode, stated positively: the l1 backup is not bound to the
	// l2 pair, so it cannot be stepped, failed or verified through it.
	if _, err := replicator.Replica(t.Context(), backupA); err == nil {
		t.Fatal("the l1 pair's backup leaked into the l2 pair's queue")
	}
}

// TestPeriodicCatchUpRetriesAFailedEnqueue is F3: a transient Enqueue failure at
// completion is retried by the replication worker's periodic catch-up, without
// a restart and without waiting longer than the catch-up interval.
func TestPeriodicCatchUpRetriesAFailedEnqueue(t *testing.T) {
	f := newFixture(t)
	useMemoryRemote(t)
	local := f.repository("periodic-local")
	ftp := f.ftpRepository("periodic-ftp")
	source := f.source("periodic-source", testSocketPath(t))
	policy := f.replicaPolicy("periodic-policy", source.ID, local.ID, ftp.ID)

	// The clock is controlled, so the test decides when the 30s interval has
	// elapsed instead of waiting.
	base := time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC)
	now := base
	d := f.newDispatcher(func() time.Time { return now })
	if err := d.openPair(t.Context(), local.ID, ftp.ID); err != nil {
		t.Fatal(err)
	}

	backupID, _ := completeLocalBackup(t, f.database, source.ID)
	run := insertTerminalRun(t, f.database, policy, backupID, "chain-periodic")

	// The pair is installed first, so the enqueue at completion can fail once
	// and the worker's periodic catch-up is the only retry path.
	replicator := d.replicatorForPair(local.ID, ftp.ID)
	if replicator == nil {
		t.Fatal("pair was not installed")
	}
	// Block the enqueue with an unacceptable backup state, as a transient
	// failure of the enqueue write does, then restore it.
	blockEnqueue(t, f.database, backupID)
	d.enqueueReplication(t.Context(), run, policy, backupID)
	assertNoQueuedReplica(t, f, backupID, ftp.ID)

	// Before the interval elapses the worker must not retry.
	now = now.Add(catchUpInterval - time.Second)
	d.stepReplication(t.Context())
	assertNoQueuedReplica(t, f, backupID, ftp.ID)

	// After the interval the worker's periodic catch-up queues the backup.
	unblockEnqueue(t, f.database, backupID)
	now = now.Add(time.Second)
	d.stepReplication(t.Context())
	assertQueuedReplica(t, f, backupID, ftp.ID)
}

// TestPeriodicCatchUpRateLimitsHealthyPairs requires the 30s floor: a healthy
// pair is not re-scanned on every worker cycle.
func TestPeriodicCatchUpRateLimitsHealthyPairs(t *testing.T) {
	f := newFixture(t)
	useMemoryRemote(t)
	local := f.repository("rate-local")
	ftp := f.ftpRepository("rate-ftp")
	source := f.source("rate-source", testSocketPath(t))
	policy := f.replicaPolicy("rate-policy", source.ID, local.ID, ftp.ID)

	base := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	now := base
	d := f.newDispatcher(func() time.Time { return now })
	if err := d.openPair(t.Context(), local.ID, ftp.ID); err != nil {
		t.Fatal(err)
	}
	// Keep the enqueue permanently impossible, so a scan would log a warning;
	// the rate-limit proof is that the scan does not even run.
	backupID, _ := completeLocalBackup(t, f.database, source.ID)
	run := insertTerminalRun(t, f.database, policy, backupID, "chain-rate")
	blockEnqueue(t, f.database, backupID)
	d.enqueueReplication(t.Context(), run, policy, backupID)

	now = now.Add(catchUpInterval / 2)
	d.stepReplication(t.Context())
	assertNoQueuedReplica(t, f, backupID, ftp.ID)
	now = now.Add(time.Millisecond)
	d.stepReplication(t.Context())
	assertNoQueuedReplica(t, f, backupID, ftp.ID)
}

// markReplicaVerified records a verified replica row for one pair, the state a
// finished copy has, so the read paths have real rows to union.
func markReplicaVerified(t *testing.T, database *store.Store, backupID, repositoryID, localRepositoryID string) {
	t.Helper()
	upsertReplica(t, database, backupID, repositoryID, localRepositoryID, "verified")
}

// upsertReplica writes one pair-scoped replica row with the given status.
func upsertReplica(t *testing.T, database *store.Store, backupID, repositoryID, localRepositoryID, status string) {
	t.Helper()
	stamp := time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z")
	_, err := database.DB().ExecContext(t.Context(), `INSERT INTO replicas
(backup_id, repository_id, local_repository_id, idem_key, status, attempts, next_attempt_at, uploaded_at, verified_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?)
ON CONFLICT(backup_id, repository_id) DO UPDATE SET
  status = excluded.status, verified_at = excluded.verified_at, local_repository_id = excluded.local_repository_id`,
		backupID, repositoryID, localRepositoryID, "replicate:"+backupID+":"+repositoryID,
		status, stamp, stamp, stamp, stamp, stamp)
	if err != nil {
		t.Fatal(err)
	}
}

// uploadingReplica marks one pair's replica as uploading, the state startup
// recovery resets to pending.
func uploadingReplica(t *testing.T, database *store.Store, backupID, repositoryID, localRepositoryID string) {
	t.Helper()
	stamp := time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z")
	_, err := database.DB().ExecContext(t.Context(), `UPDATE replicas
SET status = 'uploading', next_attempt_at = ?, updated_at = ?
WHERE backup_id = ? AND repository_id = ? AND local_repository_id = ?`,
		stamp, stamp, backupID, repositoryID, localRepositoryID)
	if err != nil {
		t.Fatal(err)
	}
}

// insertReplicaPart records one temporary part of one pair, the cleanup
// obligation startup recovery retries.
func insertReplicaPart(t *testing.T, database *store.Store, backupID, repositoryID, part string) int64 {
	t.Helper()
	result, err := database.DB().ExecContext(t.Context(), `INSERT INTO replica_parts
(backup_id, repository_id, member, part, created_at) VALUES (?, ?, 'payload.bin', ?, ?)`,
		backupID, repositoryID, part, time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z"))
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// assertReplicaPartGone requires the pair's recovery to have cleaned its part.
func assertReplicaPartGone(t *testing.T, database *store.Store, id int64) {
	t.Helper()
	var count int
	if err := database.DB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM replica_parts WHERE id = ?`, id).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("the second pair's temporary part was never cleaned")
	}
}

// blockEnqueue makes one backup unacceptable to Enqueue (not completed), which
// stands in for a transient failure of the enqueue write.
func blockEnqueue(t *testing.T, database *store.Store, backupID string) {
	t.Helper()
	if _, err := database.DB().ExecContext(t.Context(),
		`UPDATE backups SET status = 'running' WHERE id = ?`, backupID); err != nil {
		t.Fatal(err)
	}
}

// unblockEnqueue restores the completed state the enqueue needs.
func unblockEnqueue(t *testing.T, database *store.Store, backupID string) {
	t.Helper()
	if _, err := database.DB().ExecContext(t.Context(),
		`UPDATE backups SET status = 'completed' WHERE id = ?`, backupID); err != nil {
		t.Fatal(err)
	}
}

var (
	_ replication.Remote = memoryRemote{}
	_ io.Reader          = io.Reader(nil)
	_ context.Context    = nil
)
