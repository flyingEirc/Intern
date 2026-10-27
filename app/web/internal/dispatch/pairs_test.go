package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/flyingEirc/Intern/app/web/internal/resources"
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

// completeForPolicy publishes one completed full backup for one policy at the
// current time, so the run's claim snapshot names that policy's pair.
func (s *sharedFTPFixture) completeForPolicy(t *testing.T, policy store.Policy, backupID string) (backupIDOut, runID string) {
	t.Helper()
	return s.publishMember(t, policy, backupID, "full", time.Now())
}

// publishMember publishes one completed physical member for one policy at an
// explicit chain position, and records the completed run that produced it. A
// full starts a new chain at at; an incremental extends the source's latest
// chain. Each backup needs a distinct id, because the physical backups table
// keys on it.
func (s *sharedFTPFixture) publishMember(t *testing.T, policy store.Policy, backupID, kind string, at time.Time) (backupIDOut, runID string) {
	t.Helper()
	physical := physicalstore.New(s.database.DB())
	plan, err := physical.Plan(t.Context(), physicalstore.PlanRequest{
		SourceID: s.source.ID, ServerUUID: "server-uuid", ServerVersion: "8.4.11",
		ToolVersion: "8.4.0-7", Kind: kind, MaxChainLength: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = physical.Begin(t.Context(), physicalstore.BeginRequest{
		Plan: plan, SourceID: s.source.ID, ServerUUID: "server-uuid", ServerVersion: "8.4.11",
		ToolVersion: "8.4.0-7", InstanceID: s.database.InstanceID(), RunID: backupID, BackupID: backupID,
		ObjectPath:   s.database.InstanceID() + "/" + s.source.ID + "/" + plan.ChainID + "/" + backupID,
		StagingToken: backupID, WorkDir: s.workRoot() + "/run-" + backupID,
		KeyID: "key", CreatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
	var from uint64
	if plan.Parent != nil {
		from = plan.Parent.ToLSN
	}
	to := from + 100
	checkpoints := []byte("backup_type = full-backuped\nfrom_lsn = 0\nto_lsn = 100\nlast_lsn = 100\n")
	if plan.Parent != nil {
		checkpoints = []byte(fmt.Sprintf("backup_type = incremental\nfrom_lsn = %d\nto_lsn = %d\nlast_lsn = %d\n", from, to, to))
	}
	if err = physical.Complete(t.Context(), physicalstore.Completion{
		BackupID: backupID, FromLSN: from, ToLSN: to, LastLSN: to,
		CheckpointsRaw: checkpoints,
		InfoRaw:        []byte("tool_name = xtrabackup\n"), PayloadSize: 512, PayloadSHA256: "payload",
		MetadataSize: 128, MetadataSHA256: "metadata", CompletedAt: at,
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

	// The repository's point is the last item of the union's all-verified
	// prefix, which is the second pair's later chain, not the earliest pair
	// point: a pair that stops producing cannot pin the repository.
	assertSharedProtection(t, s, backupB)
	assertSharedReplicaList(t, s)
	assertSharedBackupViews(t, s, backupA, backupB)
	assertSharedRetry(t, s, backupA, backupB)
	assertSharedRecovery(t, s, backupA, backupB)
}

// assertSharedProtection requires the remote point to be visible through both
// pairs and to be the union's last verified row. Before the fix the loop
// deduplicated on the repository and compared proof times, so whichever pair
// proved first won and the other pair's later chain was hidden.
func assertSharedProtection(t *testing.T, s *sharedFTPFixture, want string) {
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
	if protection.Remote[0].RepositoryID != s.ftp.ID || protection.Remote[0].BackupID != want {
		t.Fatalf("remote point = %+v, want the union's last verified backup %s", protection.Remote[0], want)
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

// sharedRemotePoint returns the one remote point the shared FTP repository
// reports, or nil when the repository has proven nothing for the source.
func (s *sharedFTPFixture) remotePoint(t *testing.T) *PointView {
	t.Helper()
	protection, err := s.dispatcher.Protection(t.Context(), s.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(protection.Remote) > 1 {
		t.Fatalf("remote points = %+v, want at most one for the shared repository", protection.Remote)
	}
	if len(protection.Remote) == 0 {
		return nil
	}
	return &protection.Remote[0]
}

// requireRemotePoint requires the shared repository's point to be one backup.
func requireRemotePoint(t *testing.T, s *sharedFTPFixture, want string) {
	t.Helper()
	point := s.remotePoint(t)
	if point == nil {
		t.Fatalf("the repository reports no remote point, want %s", want)
	}
	if point.RepositoryID != s.ftp.ID || point.BackupID != want {
		t.Fatalf("remote point = %+v, want %s on the shared repository", *point, want)
	}
}

// requireNoRemotePoint requires the shared repository to report no point: the
// first unproven row of the union bounds the prefix before any verified row.
func requireNoRemotePoint(t *testing.T, s *sharedFTPFixture) {
	t.Helper()
	if point := s.remotePoint(t); point != nil {
		t.Fatalf("remote point = %+v, want none: the union's first unproven row bounds the prefix", *point)
	}
}

// TestProtectionPointFollowsChainPositionNotProofTime is the advisor's P1: a
// pair that stops producing must not pin the point. The first pair proves one
// backup first, the second pair proves two later backups, and the repository's
// point is the last verified row of the union ordered by chain position.
func TestProtectionPointFollowsChainPositionNotProofTime(t *testing.T) {
	s := newSharedFTPFixture(t)
	s.install(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pinned, _ := s.publishMember(t, s.policyA, "p1-pinned", "full", base)
	first, _ := s.publishMember(t, s.policyB, "p1-first", "full", base.Add(time.Hour))
	last, _ := s.publishMember(t, s.policyB, "p1-last", "full", base.Add(2*time.Hour))
	// Proof order follows chain order, but nothing may be inferred from it.
	markReplicaVerifiedAt(t, s.database, pinned, s.ftp.ID, s.l1.ID, base.Add(24*time.Hour))
	markReplicaVerifiedAt(t, s.database, first, s.ftp.ID, s.l2.ID, base.Add(48*time.Hour))
	markReplicaVerifiedAt(t, s.database, last, s.ftp.ID, s.l2.ID, base.Add(72*time.Hour))

	requireRemotePoint(t, s, last)
}

// TestProtectionPointNeverComparesVerifiedAt is the advisor's P2: the two
// proofs are inverted against the data order. The repository protects the
// source up to its newest proven backup, so the point is the later chain even
// though the earlier chain was proven last.
func TestProtectionPointNeverComparesVerifiedAt(t *testing.T) {
	s := newSharedFTPFixture(t)
	s.install(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	older, _ := s.publishMember(t, s.policyA, "p2-older", "full", base)
	newer, _ := s.publishMember(t, s.policyB, "p2-newer", "full", base.Add(time.Hour))
	markReplicaVerifiedAt(t, s.database, older, s.ftp.ID, s.l1.ID, base.Add(9*24*time.Hour))
	markReplicaVerifiedAt(t, s.database, newer, s.ftp.ID, s.l2.ID, base.Add(4*24*time.Hour))

	requireRemotePoint(t, s, newer)
}

// TestProtectionPointStopsAtAnUnprovenPredecessor is the advisor's P3: a pair
// that proved nothing is not ignored when its backup precedes the other pair's
// verified one in chain order. The union's prefix ends there, so the repository
// claims nothing rather than a point it cannot prove contiguously.
func TestProtectionPointStopsAtAnUnprovenPredecessor(t *testing.T) {
	s := newSharedFTPFixture(t)
	s.install(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pending, _ := s.publishMember(t, s.policyA, "p3-pending", "full", base)
	verified, _ := s.publishMember(t, s.policyB, "p3-verified", "full", base.Add(time.Hour))
	upsertReplica(t, s.database, pending, s.ftp.ID, s.l1.ID, "pending")
	markReplicaVerifiedAt(t, s.database, verified, s.ftp.ID, s.l2.ID, base.Add(2*time.Hour))

	requireNoRemotePoint(t, s)
}

// TestProtectionPointNeverDependsOnAnUnprovenParent is the advisor's P6: chains
// are per source, so an incremental on one pair can extend a full on the other.
// The incremental is verified, its parent full is still pending on the same
// repository, and the repository therefore reports no point: the reported point
// would otherwise depend on a backup that is not verified there.
func TestProtectionPointNeverDependsOnAnUnprovenParent(t *testing.T) {
	s := newSharedFTPFixture(t)
	s.install(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	parent, _ := s.publishMember(t, s.policyA, "p6-full", "full", base)
	incremental, _ := s.publishMember(t, s.policyB, "p6-inc", "incremental", base.Add(time.Hour))
	upsertReplica(t, s.database, parent, s.ftp.ID, s.l1.ID, "pending")
	markReplicaVerifiedAt(t, s.database, incremental, s.ftp.ID, s.l2.ID, base.Add(2*time.Hour))

	requireNoRemotePoint(t, s)
}

// TestSharedFTPCatchUpBindsOnlyItsOwnBackups is the advisor's F2 reproduction:
// source s with pA (local l1, no replica) and pB (local l2, FTP). pA's backup
// is never bound to the l2 pair, and pB's own backup reaches the replicator and
// keeps the FIFO moving.
func TestSharedFTPCatchUpBindsOnlyItsOwnBackups(t *testing.T) {
	s := newSharedFTPFixture(t)
	// pA has no replica: drop the l1 policy's replica binding so only the
	// plain (l1, no replica) policy remains.
	plain := s.updatePolicyPair(t, s.policyA, s.l1.ID, nil)

	backupA, _ := s.completeForPolicy(t, plain, "acc2-bA")     // produced on l1
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

// updatePolicyPair replaces one policy's repositories through the same store
// call the PUT handler makes, so the tests exercise the real edit path.
func (s *sharedFTPFixture) updatePolicyPair(t *testing.T, policy store.Policy, localID string, replicaID *string) store.Policy {
	t.Helper()
	updated, err := s.database.UpdatePolicy(t.Context(), policy.ID, store.PolicyInput{
		Name: policy.Name, SourceID: policy.SourceID, LocalRepositoryID: localID,
		ReplicaRepositoryID: replicaID, FullCron: policy.FullCron,
		IncrementalCron: policy.IncrementalCron, Timezone: policy.Timezone,
		MaxChainLength: policy.MaxChainLength, Parallel: policy.Parallel,
		Throttle: policy.Throttle, ZstdLevel: policy.ZstdLevel,
		PageTracking: policy.PageTracking, Enabled: policy.Enabled,
	}, store.SystemAudit("policies.update", store.AuditDetail{}))
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

// TestClaimSnapshotsThePolicyPair requires the claim itself to record the
// (local, replica) pair it ran with: the snapshot is what every later
// replication read resolves, so a claim that recorded nothing would leave the
// backup unbound after any policy edit.
func TestClaimSnapshotsThePolicyPair(t *testing.T) {
	s := newSharedFTPFixture(t)
	first := s.queuedRun(t, s.policyB, "claim-snapshot-1")
	claimed, ok := s.claimRun(t)
	if !ok || claimed.ID != first.ID {
		t.Fatalf("claim = %+v %v, want %s", claimed, ok, first.ID)
	}
	requirePinnedPair(t, s, claimed, s.l2.ID, s.ftp.ID)
	// Finish the run so the second claim is possible.
	if err := s.database.FinishRunFailed(t.Context(), claimed.ID, store.RunErrorBackupFailed, "finished",
		store.SystemAudit("runs.failed", store.AuditDetail{}), nil); err != nil {
		t.Fatal(err)
	}

	moved := s.updatePolicyPair(t, s.policyB, s.l1.ID, &s.ftp.ID)
	second := s.queuedRun(t, moved, "claim-snapshot-2")
	reclaimed, ok := s.claimRun(t)
	if !ok || reclaimed.ID != second.ID {
		t.Fatalf("second claim = %+v %v, want %s", reclaimed, ok, second.ID)
	}
	requirePinnedPair(t, s, reclaimed, s.l1.ID, s.ftp.ID)
}

// queuedRun creates one queued run of a policy.
func (s *sharedFTPFixture) queuedRun(t *testing.T, policy store.Policy, key string) store.Run {
	t.Helper()
	created, err := s.database.CreateRun(t.Context(), store.RunInput{
		PolicyID: policy.ID, SourceID: policy.SourceID, Kind: store.RunKindFull,
		Trigger: store.RunTriggerManual, IdempotencyKey: key, RequestHash: "claim-snapshot",
		ScheduledAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return created
}

// claimRun claims the next queued run.
func (s *sharedFTPFixture) claimRun(t *testing.T) (store.Run, bool) {
	t.Helper()
	run, claimed, err := s.database.ClaimRun(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return run, claimed
}

// requirePinnedPair requires the run to carry the given claimed pair.
func requirePinnedPair(t *testing.T, s *sharedFTPFixture, run store.Run, localID, replicaID string) {
	t.Helper()
	stored, err := s.database.Run(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.LocalRepositoryID == nil || *stored.LocalRepositoryID != localID {
		t.Fatalf("run %s local repository = %v, want %s", run.ID, stored.LocalRepositoryID, localID)
	}
	if stored.ReplicaRepositoryID == nil || *stored.ReplicaRepositoryID != replicaID {
		t.Fatalf("run %s replica repository = %v, want %s", run.ID, stored.ReplicaRepositoryID, replicaID)
	}
}

// recordingPreflight records the local repository the run asked it to check,
// then fails so the run stops before chain.Run.
type recordingPreflight struct{ seen chan string }

func (p recordingPreflight) PreflightSource(_ context.Context, _, localRepositoryID string) (resources.PreflightOutcome, error) {
	p.seen <- localRepositoryID
	return resources.PreflightOutcome{
		Failure: &resources.Failure{Check: "tool", Code: "tool_missing", Message: "probe"},
	}, nil
}

// TestRunUsesTheClaimedPairAfterAPolicyEdit requires the run itself — not just
// the replication reads — to work from the pair the claim snapshotted. The
// policy moves between the claim and the run, and the preflight must still be
// told the repository the run was claimed on.
func TestRunUsesTheClaimedPairAfterAPolicyEdit(t *testing.T) {
	s := newSharedFTPFixture(t)
	run := s.queuedRun(t, s.policyA, "pinned-run")
	claimed, ok := s.claimRun(t)
	if !ok || claimed.ID != run.ID {
		t.Fatalf("claim = %+v %v, want %s", claimed, ok, run.ID)
	}
	requirePinnedPair(t, s, claimed, s.l1.ID, s.ftp.ID)

	// The PUT lands between the claim and the run.
	s.updatePolicyPair(t, s.policyA, s.l2.ID, &s.ftp.ID)

	probe := recordingPreflight{seen: make(chan string, 1)}
	// The fixture's dispatcher is not started: its recovery must not touch the
	// already-claimed run, so only the injected preflight is replaced.
	s.dispatcher.config.Preflight = probe
	if err := s.dispatcher.executeRun(t.Context(), claimed); err != nil {
		t.Fatal(err)
	}
	select {
	case seen := <-probe.seen:
		if seen != s.l1.ID {
			t.Fatalf("the preflight checked repository %s, want the claimed %s", seen, s.l1.ID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the run never reached preflight")
	}
	final := s.run(claimed.ID)
	if final.Status != store.RunStatusFailed || final.ErrorClass == nil || *final.ErrorClass != store.RunErrorPreflight {
		t.Fatalf("run %+v, want a preflight failure from the recorded probe", final)
	}
}

// TestPolicyEditNeverRebindsAnExistingBackup is the advisor's P4: a PUT that
// moves the policy's repositories must not bind the backup it already produced
// to the new pair. The old backup stays on the pair its run was claimed with,
// and only the new pair's own backup replicates.
func TestPolicyEditNeverRebindsAnExistingBackup(t *testing.T) {
	s := newSharedFTPFixture(t)
	// The policy starts on l1 with no replica repository, exactly as P4 begins.
	plain := s.updatePolicyPair(t, s.policyA, s.l1.ID, nil)
	oldBackup, _ := s.completeForPolicy(t, plain, "p4-old")

	// The PUT: the same store call the handler makes.
	moved := s.updatePolicyPair(t, plain, s.l2.ID, &s.ftp.ID)
	newBackup, _ := s.completeForPolicy(t, moved, "p4-new")

	if err := s.dispatcher.openPair(t.Context(), s.l2.ID, s.ftp.ID); err != nil {
		t.Fatal(err)
	}
	assertNoQueuedReplica(t, s.fixture, oldBackup, s.ftp.ID)
	assertQueuedReplica(t, s.fixture, newBackup, s.ftp.ID)

	// The old backup cannot be stepped, failed or verified through the new pair.
	replicator := s.dispatcher.replicatorForPair(s.l2.ID, s.ftp.ID)
	if _, err := replicator.Replica(t.Context(), oldBackup); err == nil {
		t.Fatal("the policy edit rebound an existing backup to the new pair")
	}
	if stepped, attempted, _ := replicator.StepItem(t.Context()); attempted && stepped.BackupID == oldBackup {
		t.Fatalf("the new pair stepped the old backup %s", oldBackup)
	}
}

// TestReplicaRetryFindsAnOldBackupAfterAPolicyEdit requires the manual retry to
// resolve the pair the backup was claimed with, not the policy's current one.
// The pair set a restarted process installs still contains it, because the
// completed run's snapshot names it.
func TestReplicaRetryFindsAnOldBackupAfterAPolicyEdit(t *testing.T) {
	s := newSharedFTPFixture(t)
	oldBackup, _ := s.completeForPolicy(t, s.policyA, "retry-old")
	s.install(t)
	assertQueuedReplica(t, s.fixture, oldBackup, s.ftp.ID)

	// The PUT moves the policy to the other local repository.
	s.updatePolicyPair(t, s.policyA, s.l2.ID, &s.ftp.ID)

	pairs, pairErr := s.database.ReplicationPairs(t.Context())
	if pairErr != nil {
		t.Fatal(pairErr)
	}
	restarted := s.newDispatcher(time.Now)
	for _, pair := range pairs {
		if openErr := restarted.openPair(t.Context(), pair.LocalRepositoryID, pair.ReplicaRepositoryID); openErr != nil {
			t.Fatal(openErr)
		}
	}
	owner := restarted.replicatorForPair(s.l1.ID, s.ftp.ID)
	if owner == nil {
		t.Fatal("the pair the old run was claimed with is no longer installed")
	}
	if retryErr := restarted.ReplicaRetry(t.Context(), oldBackup, s.ftp.ID); retryErr != nil {
		t.Fatalf("retry of a backup produced before the policy edit: %v", retryErr)
	}
	replica, replicaErr := owner.Replica(t.Context(), oldBackup)
	if replicaErr != nil || replica.Status != "pending" {
		t.Fatalf("retried replica = %+v, %v; want pending on its own pair", replica, replicaErr)
	}
	if _, err := restarted.replicatorForPair(s.l2.ID, s.ftp.ID).Replica(t.Context(), oldBackup); err == nil {
		t.Fatal("the policy edit rebound the old backup to the new pair")
	}
}

// TestCatchUpSkipsPrunedBackups requires the catch-up to select only backups
// that are still locally present: a pruned backup has nothing to copy, so it
// never enters the queue and never warns on every scan.
func TestCatchUpSkipsPrunedBackups(t *testing.T) {
	f := newFixture(t)
	useMemoryRemote(t)
	local := f.repository("pruned-local")
	ftp := f.ftpRepository("pruned-ftp")
	source := f.source("pruned-source", testSocketPath(t))
	policy := f.replicaPolicy("pruned-policy", source.ID, local.ID, ftp.ID)
	backupID, chainID := completeLocalBackup(t, f.database, source.ID)
	insertTerminalRun(t, f.database, policy, backupID, chainID)
	// The chain was pruned after its copy: the row stays, its payload is gone.
	if _, err := f.database.DB().ExecContext(t.Context(),
		`UPDATE backups SET local_present = 0 WHERE id = ?`, backupID); err != nil {
		t.Fatal(err)
	}

	d := f.newDispatcher(time.Now)
	if err := d.openPair(t.Context(), local.ID, ftp.ID); err != nil {
		t.Fatal(err)
	}
	assertNoQueuedReplica(t, f, backupID, ftp.ID)
	if logs := f.logs.String(); strings.Contains(logs, "replication catch-up skipped a backup") {
		t.Fatalf("the catch-up warned about a pruned backup: %s", logs)
	}
}

// TestPeriodicCatchUpUsesTheDispatcherClock proves the 30s floor on the
// dispatcher's clock with an enqueue that is not blocked: a backup completed
// after the pair was installed is queued by the periodic catch-up only once the
// controlled clock passes the interval. A catch-up that always runs would queue
// it early, and one that read the real clock would never agree with the
// recorded mark.
func TestPeriodicCatchUpUsesTheDispatcherClock(t *testing.T) {
	f := newFixture(t)
	useMemoryRemote(t)
	local := f.repository("clock-local")
	ftp := f.ftpRepository("clock-ftp")
	source := f.source("clock-source", testSocketPath(t))
	policy := f.replicaPolicy("clock-policy", source.ID, local.ID, ftp.ID)

	base := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	now := base
	d := f.newDispatcher(func() time.Time { return now })
	// The pair is installed with nothing to queue, so its catch-up mark is base.
	if err := d.openPair(t.Context(), local.ID, ftp.ID); err != nil {
		t.Fatal(err)
	}
	backupID, _ := completeLocalBackup(t, f.database, source.ID)
	insertTerminalRun(t, f.database, policy, backupID, "chain-clock")
	// No enqueue at completion: the periodic catch-up is the only path, and
	// nothing is blocked, so a row proves the scan really ran.
	assertNoQueuedReplica(t, f, backupID, ftp.ID)

	now = base.Add(catchUpInterval - time.Second)
	d.stepReplication(t.Context())
	assertNoQueuedReplica(t, f, backupID, ftp.ID)

	now = base.Add(catchUpInterval)
	d.stepReplication(t.Context())
	assertQueuedReplica(t, f, backupID, ftp.ID)
}

// TestPeriodicCatchUpRateLimitsHealthyPairs requires the 30s floor: a healthy
// pair is not re-scanned on every worker cycle. The blocked enqueue makes the
// assertion weak on its own — a scan would warn and still leave no row — so the
// floor itself is proven by TestPeriodicCatchUpUsesTheDispatcherClock; this test
// pins that a pair installed at Start is not re-scanned inside the interval.
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

// markReplicaVerifiedAt records a verified replica row with an explicit proof
// time, so a probe can invert proof order against chain order.
func markReplicaVerifiedAt(t *testing.T, database *store.Store, backupID, repositoryID, localRepositoryID string, verifiedAt time.Time) {
	t.Helper()
	upsertReplicaAt(t, database, backupID, repositoryID, localRepositoryID, "verified", verifiedAt)
}

// upsertReplica writes one pair-scoped replica row with the given status.
func upsertReplica(t *testing.T, database *store.Store, backupID, repositoryID, localRepositoryID, status string) {
	t.Helper()
	upsertReplicaAt(t, database, backupID, repositoryID, localRepositoryID, status, time.Now())
}

// upsertReplicaAt writes one pair-scoped replica row; only a verified row keeps
// the proof time it is given.
func upsertReplicaAt(t *testing.T, database *store.Store, backupID, repositoryID, localRepositoryID, status string, verifiedAt time.Time) {
	t.Helper()
	stamp := time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z")
	var verified any
	if status == "verified" {
		verified = verifiedAt.UTC().Format("2006-01-02T15:04:05.000000000Z")
	}
	_, err := database.DB().ExecContext(t.Context(), `INSERT INTO replicas
(backup_id, repository_id, local_repository_id, idem_key, status, attempts, next_attempt_at, uploaded_at, verified_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?)
ON CONFLICT(backup_id, repository_id) DO UPDATE SET
  status = excluded.status, verified_at = excluded.verified_at, local_repository_id = excluded.local_repository_id`,
		backupID, repositoryID, localRepositoryID, "replicate:"+backupID+":"+repositoryID,
		status, stamp, stamp, verified, stamp, stamp)
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

// TestReplicaAddedLaterQueuesTheChainBaseFirst is the advisor's P7a: one policy
// produced its full before any replica existed, and the PUT that added the
// replica must not leave the chain base unqueued on the repository. The
// catch-up pulls the qualifying predecessors first, so the full is enqueued
// before the incremental, and the repository's point is only correct once both
// members are verified there.
func TestReplicaAddedLaterQueuesTheChainBaseFirst(t *testing.T) {
	s := newSharedFTPFixture(t)
	plain := s.updatePolicyPair(t, s.policyA, s.l1.ID, nil) // (l1, no replica)
	base := time.Date(2026, 1, 7, 8, 0, 0, 0, time.UTC)
	full, _ := s.publishMember(t, plain, "p7-full", "full", base)
	paired := s.updatePolicyPair(t, plain, s.l1.ID, &s.ftp.ID)
	incremental, _ := s.publishMember(t, paired, "p7-inc", "incremental", base.Add(time.Hour))

	// Installing the pair runs the catch-up. Only the incremental's run was
	// claimed with the pair, so the predecessor pull is what queues the full.
	if err := s.dispatcher.openPair(t.Context(), s.l1.ID, s.ftp.ID); err != nil {
		t.Fatal(err)
	}
	assertQueuedReplica(t, s.fixture, full, s.ftp.ID)
	assertQueuedReplica(t, s.fixture, incremental, s.ftp.ID)

	// FIFO follows chain position: the first eligible item of the pair's queue
	// is the chain base, not the incremental that depends on it.
	replicator := s.dispatcher.replicatorForPair(s.l1.ID, s.ftp.ID)
	// The memory remote cannot copy, so the attempt itself may fail; what
	// matters is which backup it named.
	stepped, attempted, _ := replicator.StepItem(t.Context())
	if !attempted {
		t.Fatal("the pair's queue stepped nothing after the catch-up")
	}
	if stepped.BackupID != full {
		t.Fatalf("first stepped backup = %s, want the chain base %s", stepped.BackupID, full)
	}

	// The point is correct only after both members are verified: nothing is
	// proven right after the catch-up, the verified base is the point, and
	// only the whole verified chain moves it to the incremental.
	markReplicaVerifiedAt(t, s.database, full, s.ftp.ID, s.l1.ID, base.Add(2*time.Hour))
	requireRemotePoint(t, s, full)
	markReplicaVerifiedAt(t, s.database, incremental, s.ftp.ID, s.l1.ID, base.Add(3*time.Hour))
	requireRemotePoint(t, s, incremental)
}

// TestCrossLocalChainNeverClaimsTheOtherLocalBase is the advisor's P7b: policy
// pA (l1, no replica) produced the chain's full and policy pB (l2, r) produced
// an incremental of that chain. The incremental is pB's own backup, but its
// chain base lives on another local repository and never reaches r through the
// l2 pair, so r's point must never name the incremental.
func TestCrossLocalChainNeverClaimsTheOtherLocalBase(t *testing.T) {
	s := newSharedFTPFixture(t)
	plain := s.updatePolicyPair(t, s.policyA, s.l1.ID, nil)
	base := time.Date(2026, 1, 7, 9, 0, 0, 0, time.UTC)
	full, _ := s.publishMember(t, plain, "p7b-full", "full", base)
	incremental, _ := s.publishMember(t, s.policyB, "p7b-inc", "incremental", base.Add(time.Hour))

	if err := s.dispatcher.openPair(t.Context(), s.l2.ID, s.ftp.ID); err != nil {
		t.Fatal(err)
	}
	assertQueuedReplica(t, s.fixture, incremental, s.ftp.ID)
	assertNoQueuedReplica(t, s.fixture, full, s.ftp.ID)

	// The base has no row on the repository, so the chain is not on it from
	// seq 1: the verified incremental contributes nothing.
	markReplicaVerifiedAt(t, s.database, incremental, s.ftp.ID, s.l2.ID, base.Add(2*time.Hour))
	requireNoRemotePoint(t, s)
}

// TestProtectionPointAdvancesPastAGappedChain is the advisor's Z1 single-policy
// reproduction: one policy produced the chain's full on l1 with no replica
// repository, a PUT moved it to (l2, r), and the same source then produced an
// incremental of that chain plus a complete new chain. The first chain is
// permanently incomplete on r - its base was produced elsewhere and no pull can
// bring it - so the gap must drop only that chain, and the verified chains after
// it must still reach the point. Before the fix the gap ended the whole scan, so
// the repository reported no point at all, forever.
func TestProtectionPointAdvancesPastAGappedChain(t *testing.T) {
	s := newSharedFTPFixture(t)
	plain := s.updatePolicyPair(t, s.policyA, s.l1.ID, nil)
	base := time.Date(2026, 1, 9, 8, 0, 0, 0, time.UTC)
	gapBase, _ := s.publishMember(t, plain, "z1-full", "full", base)
	paired := s.updatePolicyPair(t, plain, s.l2.ID, &s.ftp.ID)
	gapped, _ := s.publishMember(t, paired, "z1-inc", "incremental", base.Add(time.Hour))
	second, _ := s.publishMember(t, paired, "z1-next-0", "full", base.Add(2*time.Hour))
	third, _ := s.publishMember(t, paired, "z1-next-1", "incremental", base.Add(3*time.Hour))
	fourth, _ := s.publishMember(t, paired, "z1-next-2", "full", base.Add(4*time.Hour))

	if err := s.dispatcher.openPair(t.Context(), s.l2.ID, s.ftp.ID); err != nil {
		t.Fatal(err)
	}
	// The other local repository's full never reaches this pair, which is the
	// gap; everything the pair produced is queued.
	assertNoQueuedReplica(t, s.fixture, gapBase, s.ftp.ID)
	for _, backupID := range []string{gapped, second, third, fourth} {
		assertQueuedReplica(t, s.fixture, backupID, s.ftp.ID)
	}

	// The gapped chain's member is verified first: it proves nothing on its own,
	// and it must not be claimed as the point either.
	markReplicaVerifiedAt(t, s.database, gapped, s.ftp.ID, s.l2.ID, base.Add(5*time.Hour))
	requireNoRemotePoint(t, s)

	// Each complete chain then moves the point to its own latest member, and the
	// gapped chain never comes back into it.
	markReplicaVerifiedAt(t, s.database, second, s.ftp.ID, s.l2.ID, base.Add(6*time.Hour))
	requireRemotePoint(t, s, second)
	markReplicaVerifiedAt(t, s.database, third, s.ftp.ID, s.l2.ID, base.Add(7*time.Hour))
	requireRemotePoint(t, s, third)
	markReplicaVerifiedAt(t, s.database, fourth, s.ftp.ID, s.l2.ID, base.Add(8*time.Hour))
	requireRemotePoint(t, s, fourth)

	// The pair's own point agrees with the repository-wide union.
	pair, err := s.dispatcher.replicatorForPair(s.l2.ID, s.ftp.ID).RemotePoint(t.Context(), s.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pair == nil || pair.BackupID != fourth {
		t.Fatalf("pair point = %+v, want %s", pair, fourth)
	}
}

// TestProtectionPointOfAP7bChainFollowedByANewChain is the advisor's Z1 second
// probe: the P7b shape (a cross-local chain whose base is unprovable on the
// repository) followed by a brand-new complete chain. The new chain must become
// the point; P7b's gap must never be claimed.
func TestProtectionPointOfAP7bChainFollowedByANewChain(t *testing.T) {
	s := newSharedFTPFixture(t)
	plain := s.updatePolicyPair(t, s.policyA, s.l1.ID, nil)
	base := time.Date(2026, 1, 9, 9, 0, 0, 0, time.UTC)
	foreignBase, _ := s.publishMember(t, plain, "z1b-full", "full", base)
	// policyB is (l2, r): its incremental extends the chain foreignBase started.
	orphaned, _ := s.publishMember(t, s.policyB, "z1b-inc", "incremental", base.Add(time.Hour))
	ownBase, _ := s.publishMember(t, s.policyB, "z1b-new-full", "full", base.Add(2*time.Hour))
	ownInc, _ := s.publishMember(t, s.policyB, "z1b-new-inc", "incremental", base.Add(3*time.Hour))

	if err := s.dispatcher.openPair(t.Context(), s.l2.ID, s.ftp.ID); err != nil {
		t.Fatal(err)
	}
	assertNoQueuedReplica(t, s.fixture, foreignBase, s.ftp.ID)

	// P7b alone: the cross-local chain contributes nothing, so the repository has
	// no point at all.
	markReplicaVerifiedAt(t, s.database, orphaned, s.ftp.ID, s.l2.ID, base.Add(4*time.Hour))
	requireNoRemotePoint(t, s)

	// The new chain is on the repository from seq 1 and verified: it is the
	// point, and its last member wins as each member is proven.
	markReplicaVerifiedAt(t, s.database, ownBase, s.ftp.ID, s.l2.ID, base.Add(5*time.Hour))
	requireRemotePoint(t, s, ownBase)
	markReplicaVerifiedAt(t, s.database, ownInc, s.ftp.ID, s.l2.ID, base.Add(6*time.Hour))
	requireRemotePoint(t, s, ownInc)

	point := s.remotePoint(t)
	if point != nil && point.ChainID == chainOf(t, s, orphaned) {
		t.Fatalf("remote point = %+v, must never be claimed from the gapped chain", *point)
	}
}

// chainOf reads the chain id one backup belongs to, so a test can name the chain
// a point must not come from.
func chainOf(t *testing.T, s *sharedFTPFixture, backupID string) string {
	t.Helper()
	var chainID string
	if err := s.database.DB().QueryRowContext(t.Context(),
		`SELECT chain_id FROM backups WHERE id = ?`, backupID).Scan(&chainID); err != nil {
		t.Fatal(err)
	}
	return chainID
}

// TestReplicationPairsDropsAPairWhoseRepositoryIsGone pins the repository
// existence check inside ReplicationPairs. Deleting a repository is refused
// while runs reference it, so the state the check defends against — a completed
// run whose snapshot names a repository row that no longer exists — is reached
// directly in the database, the way pre-upgrade rows or an out-of-band deletion
// leave it.
func TestReplicationPairsDropsAPairWhoseRepositoryIsGone(t *testing.T) {
	s := newSharedFTPFixture(t)
	s.completeForPolicy(t, s.policyA, "mn-b1") // run snapshot (l1, ftp)
	s.completeForPolicy(t, s.policyB, "mn-b2") // run snapshot (l2, ftp)
	// Move both policies off the repositories that are about to disappear,
	// through the real edit path, so the repository rows can be removed while
	// the run snapshots still name them.
	s.updatePolicyPair(t, s.policyA, s.l2.ID, nil)
	s.updatePolicyPair(t, s.policyB, s.l2.ID, nil)
	for _, repository := range []store.Repository{s.l1, s.ftp} {
		if _, err := s.database.DB().ExecContext(t.Context(),
			`DELETE FROM repositories WHERE id = ?`, repository.ID); err != nil {
			t.Fatal(err)
		}
	}

	// Neither completed-run snapshot survives: one names a local repository
	// that is gone, the other a replica repository that is gone.
	pairs, err := s.database.ReplicationPairs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 0 {
		t.Fatalf("replication pairs = %+v, want none: every pair names a missing repository", pairs)
	}
}

// TestReplicaRetryReachesAPulledPredecessor is the advisor's Z2: the manual
// retry must resolve the pair from the replica row, not from the producing run's
// snapshot. A chain base the predecessor pull queued was produced by a run that
// named no replica repository at all, so a snapshot-based lookup answers 404 for
// exactly the row that blocks the queue: with the base exhausted, StepItem
// attempts nothing, the remote point stays unset, and no API call can repair it.
func TestReplicaRetryReachesAPulledPredecessor(t *testing.T) {
	s := newSharedFTPFixture(t)
	plain := s.updatePolicyPair(t, s.policyA, s.l1.ID, nil) // (l1, no replica)
	base := time.Date(2026, 1, 8, 8, 0, 0, 0, time.UTC)
	full, fullRun := s.publishMember(t, plain, "z2-full", "full", base)
	paired := s.updatePolicyPair(t, plain, s.l1.ID, &s.ftp.ID)
	incremental, _ := s.publishMember(t, paired, "z2-inc", "incremental", base.Add(time.Hour))

	// The pull queues the base, and its producing run really does name no replica
	// repository: that is the state a snapshot lookup cannot resolve.
	if err := s.dispatcher.openPair(t.Context(), s.l1.ID, s.ftp.ID); err != nil {
		t.Fatal(err)
	}
	assertQueuedReplica(t, s.fixture, full, s.ftp.ID)
	assertRunNamesNoReplicaRepository(t, s, fullRun)
	replicator := s.dispatcher.replicatorForPair(s.l1.ID, s.ftp.ID)

	// The base exhausted its attempts, so the queue is blocked behind it.
	upsertReplica(t, s.database, full, s.ftp.ID, s.l1.ID, replication.StatusExhausted)
	assertBlockedBehindTheExhaustedBase(t, s, full, replicator)
	assertRetryReopensTheBase(t, s, full, replicator)

	// Once both members are verified the point advances over the repaired pair.
	markReplicaVerifiedAt(t, s.database, full, s.ftp.ID, s.l1.ID, base.Add(2*time.Hour))
	requireRemotePoint(t, s, full)
	markReplicaVerifiedAt(t, s.database, incremental, s.ftp.ID, s.l1.ID, base.Add(3*time.Hour))
	requireRemotePoint(t, s, incremental)
}

// assertRunNamesNoReplicaRepository requires one run's snapshot to name no
// replica repository, which is the state the predecessor pull produces: the base
// was produced before the policy had one.
func assertRunNamesNoReplicaRepository(t *testing.T, s *sharedFTPFixture, runID string) {
	t.Helper()
	producer, err := s.database.Run(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if producer.ReplicaRepositoryID != nil {
		t.Fatalf("run %s snapshot replica = %v, want none", runID, *producer.ReplicaRepositoryID)
	}
}

// assertBlockedBehindTheExhaustedBase requires the queue to be stuck: no step is
// possible, no point is proven, yet every read path still shows the row that
// blocks. BackupView and Replicas resolve pairs from the replica table, so an
// operator can always see the blocked copy and name it in a retry.
func assertBlockedBehindTheExhaustedBase(t *testing.T, s *sharedFTPFixture, backupID string,
	replicator *replication.Replicator,
) {
	t.Helper()
	view, err := s.dispatcher.BackupView(t.Context(), backupID)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Replicas) != 1 || view.Replicas[0].Status != replication.StatusExhausted {
		t.Fatalf("backup view replicas = %+v, want the exhausted row on the shared repository",
			view.Replicas)
	}
	views, err := s.dispatcher.Replicas(t.Context(), ReplicaFilter{SourceID: s.source.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 2 || views[0].RepositoryID != s.ftp.ID {
		t.Fatalf("replica list = %+v, want both queue rows on the shared repository", views)
	}
	requireNoRemotePoint(t, s)
	if stepped, attempted, stepErr := replicator.StepItem(t.Context()); attempted || stepErr != nil {
		t.Fatalf("step behind an exhausted base = %+v, %v, %v; want nothing attempted",
			stepped, attempted, stepErr)
	}
}

// assertRetryReopensTheBase requires the manual retry to reach the pulled
// predecessor through its own pair, restart its budget, and let the queue step
// it again. The memory remote cannot copy, so the attempt may fail; what is under
// test is that the blocked predecessor is workable again.
func assertRetryReopensTheBase(t *testing.T, s *sharedFTPFixture, backupID string,
	replicator *replication.Replicator,
) {
	t.Helper()
	if retryErr := s.dispatcher.ReplicaRetry(t.Context(), backupID, s.ftp.ID); retryErr != nil {
		t.Fatalf("retry of a predecessor the pull queued: %v", retryErr)
	}
	retried, err := replicator.Replica(t.Context(), backupID)
	if err != nil {
		t.Fatal(err)
	}
	if retried.Status != replication.StatusPending {
		t.Fatalf("retried replica status = %s, want pending", retried.Status)
	}
	stepped, attempted, stepErr := replicator.StepItem(t.Context())
	if !attempted {
		t.Fatalf("step after the retry attempted nothing: %+v, %v", stepped, stepErr)
	}
	if stepped.BackupID != backupID {
		t.Fatalf("stepped backup = %s, want the retried predecessor %s", stepped.BackupID, backupID)
	}
}

// TestBackupViewAndRetryResolveTheReplicaRowNotTheRunSnapshot pins the rule the
// manual retry follows: the replica row is the binding of a copy, and every path
// that answers for one backup's copies reads it. A run written before the local
// repository was recorded in the run snapshot names nothing, so a snapshot-derived
// pair lookup loses the row from the backup view while the replica list and the
// repository point still count it. Reading the row's own local_repository_id keeps
// the four paths in agreement.
func TestBackupViewAndRetryResolveTheReplicaRowNotTheRunSnapshot(t *testing.T) {
	s := newSharedFTPFixture(t)
	plain := s.updatePolicyPair(t, s.policyA, s.l1.ID, nil)
	at := time.Date(2026, 1, 10, 8, 0, 0, 0, time.UTC)
	full, _ := s.publishMember(t, plain, "row-full", "full", at)
	paired := s.updatePolicyPair(t, plain, s.l1.ID, &s.ftp.ID)
	incremental, _ := s.publishMember(t, paired, "row-inc", "incremental", at.Add(time.Hour))
	if err := s.dispatcher.openPair(t.Context(), s.l1.ID, s.ftp.ID); err != nil {
		t.Fatal(err)
	}

	// The pre-upgrade shape: no run of this source names a local repository, so a
	// snapshot-derived read has nothing to scope itself by.
	if _, err := s.database.DB().ExecContext(t.Context(),
		`UPDATE runs SET local_repository_id = NULL WHERE source_id = ?`, s.source.ID); err != nil {
		t.Fatal(err)
	}

	// The replica list and the backup view agree on one row per queue, because
	// both read the replica rows.
	views, err := s.dispatcher.Replicas(t.Context(), ReplicaFilter{SourceID: s.source.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 2 {
		t.Fatalf("replica list = %+v, want both queued rows", views)
	}
	view, err := s.dispatcher.BackupView(t.Context(), full)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Replicas) != 1 || view.Replicas[0].RepositoryID != s.ftp.ID {
		t.Fatalf("backup view replicas = %+v, want the base's row on the shared repository",
			view.Replicas)
	}

	// And the retry reaches the same row through the same pair.
	if retryErr := s.dispatcher.ReplicaRetry(t.Context(), full, s.ftp.ID); retryErr != nil {
		t.Fatalf("retry after the run snapshot lost its local repository: %v", retryErr)
	}
	if _, err := s.dispatcher.BackupView(t.Context(), incremental); err != nil {
		t.Fatalf("backup view of the untouched member: %v", err)
	}
}

var (
	_ replication.Remote = memoryRemote{}
	_ io.Reader          = io.Reader(nil)
	_ context.Context    = nil
)
