package replication

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/flyingEirc/Intern/internal/physical/store"
	"github.com/flyingEirc/Intern/internal/storage/local"
)

const (
	testSourceID        = "source"
	testRepositoryID    = "ftp-repository"
	testLocalRepository = "local-repository"
	testInstanceID      = "instance"
)

var objectMembers = []string{"payload.bin", "envelope.json", "metadata.bin", "metadata.envelope.json"}

// fixture is a real control-plane database with the physical and replication
// migrations applied, plus a real local object repository published by staging.
type fixture struct {
	t      *testing.T
	db     *sql.DB
	store  *store.Store
	repo   *local.Repository
	root   string
	now    time.Time
	serial int
	r      *Replicator
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{t: t, root: filepath.Join(dir, "objects"), now: time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)}
	f.db = openDatabase(t, filepath.Join(dir, "intern.db"))
	if err := store.Migrate(t.Context(), f.db); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), f.db); err != nil {
		t.Fatal(err)
	}
	f.store = store.New(f.db)
	f.repo = openRepository(t, f.root)
	f.r = f.replicator(f.repo)
	return f
}

func openDatabase(t *testing.T, name string) *sql.DB {
	t.Helper()
	u := url.URL{Scheme: "file", Path: name}
	q := u.Query()
	for _, pragma := range []string{"journal_mode(WAL)", "synchronous(FULL)", "foreign_keys(1)", "busy_timeout(5000)"} {
		q.Add("_pragma", pragma)
	}
	q.Set("_txlock", "immediate")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func openRepository(t *testing.T, root string) *local.Repository {
	t.Helper()
	repo, err := local.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	return repo
}

// replicator binds one repository root, so a test can replace a closed one.
func (f *fixture) replicator(repo *local.Repository) *Replicator {
	return &Replicator{
		DB: f.db, Store: f.store, Local: repo, Remote: newFakeRemote(),
		RepositoryID: testRepositoryID, LocalRepositoryID: testLocalRepository,
		InstanceID: testInstanceID, WorkRoot: filepath.Join(filepath.Dir(f.root), "work"),
		Now: func() time.Time { return f.now },
	}
}

func (f *fixture) tick() { f.now = f.now.Add(time.Second) }

type testChain struct {
	id      string
	backups []string
	objects []string
}

// newChain publishes a real chain of members in order, exactly like the
// physical chain: running row and ledger, published object directory, then the
// completion record.
func (f *fixture) newChain(members int) testChain {
	f.t.Helper()
	var chain testChain
	var last uint64
	for i := range members {
		plan, err := f.store.Plan(f.t.Context(), store.PlanRequest{
			SourceID: testSourceID, ServerUUID: "server-uuid", ServerVersion: "8.4.11",
			ToolVersion: "8.4.0-7", Kind: fullOrIncremental(i), MaxChainLength: 100,
		})
		if err != nil {
			f.t.Fatalf("plan chain member %d: %v", i, err)
		}
		f.serial++
		backupID := fmt.Sprintf("backup-%d", f.serial)
		objectPath := path.Join(testInstanceID, testSourceID, plan.ChainID, fmt.Sprintf("%04d-%s", plan.Seq, backupID))
		f.tick()
		err = f.store.Begin(f.t.Context(), store.BeginRequest{
			Plan: plan, SourceID: testSourceID, ServerUUID: "server-uuid", ServerVersion: "8.4.11",
			ToolVersion: "8.4.0-7", InstanceID: testInstanceID, RunID: backupID, BackupID: backupID,
			ObjectPath: objectPath, StagingToken: backupID, WorkDir: filepath.Join(f.r.WorkRoot, "run-"+backupID),
			KeyID: "key-id", CreatedAt: f.now,
		})
		if err != nil {
			f.t.Fatalf("begin %s: %v", backupID, err)
		}
		f.publish(objectPath, backupID)
		f.tick()
		to := last + 100
		if err = f.store.Complete(f.t.Context(), store.Completion{
			BackupID: backupID, FromLSN: last, ToLSN: to, LastLSN: to,
			CheckpointsRaw: checkpoints(i, last, to), InfoRaw: []byte("tool_name = xtrabackup\n"),
			PayloadSize: 64, PayloadSHA256: "payload-digest", MetadataSize: 32,
			MetadataSHA256: "metadata-digest", CompletedAt: f.now,
		}); err != nil {
			f.t.Fatalf("complete %s: %v", backupID, err)
		}
		last = to
		chain.id = plan.ChainID
		chain.backups = append(chain.backups, backupID)
		chain.objects = append(chain.objects, objectPath)
	}
	return chain
}

func fullOrIncremental(index int) string {
	if index == 0 {
		return "full"
	}
	return "incremental"
}

func checkpoints(index int, from, to uint64) []byte {
	kind := "incremental"
	if index == 0 {
		kind = "full-backuped"
	}
	return fmt.Appendf(nil, "backup_type = %s\nfrom_lsn = %d\nto_lsn = %d\nlast_lsn = %d\n", kind, from, to, to)
}

// publish writes the four real member files through the publishing staging
// directory, so retention removes a genuine published tree.
func (f *fixture) publish(objectPath, token string) {
	f.t.Helper()
	staging, err := f.repo.BeginOwned(objectPath, token)
	if err != nil {
		f.t.Fatalf("begin object %s: %v", objectPath, err)
	}
	for _, member := range objectMembers {
		if err = writeMember(f.t, staging, objectPath, member); err != nil {
			f.t.Fatal(err)
		}
	}
	if err = staging.Publish(f.t.Context()); err != nil {
		f.t.Fatalf("publish %s: %v", objectPath, err)
	}
}

func writeMember(t *testing.T, staging *local.Staging, objectPath, member string) error {
	t.Helper()
	sink, err := staging.Create(member)
	if err != nil {
		return fmt.Errorf("create %s/%s: %w", objectPath, member, err)
	}
	if _, err = sink.Write([]byte("ciphertext:" + objectPath + "/" + member)); err != nil {
		return fmt.Errorf("write %s/%s: %w", objectPath, member, err)
	}
	if _, err = sink.Commit(t.Context()); err != nil {
		return fmt.Errorf("commit %s/%s: %w", objectPath, member, err)
	}
	return nil
}

// verify records the durable verified replica state for these members. The
// retention rules only read it, so inserting the rows directly keeps the
// upload engine out of this test.
func (f *fixture) verify(backups ...string) {
	f.t.Helper()
	for _, backupID := range backups {
		stamp := f.now.Format("2006-01-02T15:04:05.000000000Z")
		_, err := f.db.ExecContext(f.t.Context(), `INSERT INTO replicas
(backup_id, repository_id, local_repository_id, idem_key, status, attempts, next_attempt_at, uploaded_at, verified_at, created_at, updated_at)
VALUES (?, ?, ?, ?, 'verified', 1, ?, ?, ?, ?, ?)
ON CONFLICT(backup_id, repository_id) DO UPDATE SET status = 'verified', verified_at = excluded.verified_at`,
			backupID, testRepositoryID, testLocalRepository, "replicate:"+backupID+":"+testRepositoryID,
			stamp, stamp, stamp, stamp, stamp)
		if err != nil {
			f.t.Fatalf("record verified replica %s: %v", backupID, err)
		}
	}
}

func (f *fixture) verifyChain(chain testChain) { f.verify(chain.backups...) }

func (f *fixture) exists(objectPath string) bool {
	f.t.Helper()
	_, err := os.Lstat(filepath.Join(f.root, filepath.FromSlash(objectPath)))
	return err == nil
}

func TestEligibilityRequiresANewerVerifiedChain(t *testing.T) {
	f := newFixture(t)
	first := f.newChain(2)
	f.verifyChain(first)
	assertEligibility(t, f, first.id, false, RetentionLatest)

	second := f.newChain(2)
	assertEligibility(t, f, first.id, false, RetentionNoNewerVerified)
	// A chain with unverified members outranks the latest-chain reason.
	assertEligibility(t, f, second.id, false, RetentionUnverified)

	f.verifyChain(second)
	assertEligibility(t, f, first.id, true, "")
	assertEligibility(t, f, second.id, false, RetentionLatest)
}

func TestEligibilityRefusesPinned(t *testing.T) {
	f := newFixture(t)
	first, second := f.newChain(2), f.newChain(2)
	f.verifyChain(first)
	f.verifyChain(second)
	assertEligibility(t, f, first.id, true, "")
	if err := f.store.SetPinned(t.Context(), first.id, true); err != nil {
		t.Fatal(err)
	}
	assertEligibility(t, f, first.id, false, RetentionPinned)
	if err := f.store.SetPinned(t.Context(), first.id, false); err != nil {
		t.Fatal(err)
	}
	assertEligibility(t, f, first.id, true, "")
}

func TestEligibilityRefusesIncompleteChain(t *testing.T) {
	f := newFixture(t)
	first, second := f.newChain(2), f.newChain(2)
	f.verifyChain(first)
	f.verifyChain(second)
	// A running member is never eligible, and it also never counts as a newer
	// verified chain for the chain before it.
	plan, err := f.store.Plan(t.Context(), store.PlanRequest{
		SourceID: testSourceID, ServerUUID: "server-uuid", ServerVersion: "8.4.11",
		ToolVersion: "8.4.0-7", Kind: "incremental", MaxChainLength: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.store.Begin(t.Context(), store.BeginRequest{
		Plan: plan, SourceID: testSourceID, ServerUUID: "server-uuid", ServerVersion: "8.4.11",
		ToolVersion: "8.4.0-7", InstanceID: testInstanceID, RunID: "running", BackupID: "running",
		ObjectPath:   path.Join(testInstanceID, testSourceID, plan.ChainID, "0003-running"),
		StagingToken: "running", WorkDir: filepath.Join(f.r.WorkRoot, "run-running"), KeyID: "key-id", CreatedAt: f.now,
	}); err != nil {
		t.Fatal(err)
	}
	if plan.ChainID != second.id {
		t.Fatalf("running member joined chain %s; want %s", plan.ChainID, second.id)
	}
	assertEligibility(t, f, second.id, false, RetentionIncomplete)
	assertEligibility(t, f, first.id, false, RetentionNoNewerVerified)
}

func TestEligibilityRefusesUnverifiedMember(t *testing.T) {
	f := newFixture(t)
	first, second := f.newChain(2), f.newChain(2)
	f.verifyChain(first)
	// One member of the newer chain has only been uploaded, so the chain as a
	// whole is not verified and cannot displace the older chain.
	f.verify(second.backups[0])
	assertEligibility(t, f, second.id, false, RetentionUnverified)
	assertEligibility(t, f, first.id, false, RetentionNoNewerVerified)
}

func TestEligibilityIsChainLevelOnly(t *testing.T) {
	f := newFixture(t)
	first, middle, latest := f.newChain(2), f.newChain(3), f.newChain(1)
	for _, chain := range []testChain{first, middle, latest} {
		f.verifyChain(chain)
	}
	// A middle chain is eligible as a whole; there is no per-member rule and no
	// reason that depends on the number of links.
	assertEligibility(t, f, middle.id, true, "")
	assertEligibility(t, f, first.id, true, "")
	assertEligibility(t, f, latest.id, false, RetentionLatest)
}

func TestDeleteLocalChainReclaimsOnlyThatChain(t *testing.T) {
	f := newFixture(t)
	first, middle, latest := f.newChain(2), f.newChain(3), f.newChain(1)
	for _, chain := range []testChain{first, middle, latest} {
		f.verifyChain(chain)
	}
	if err := f.r.DeleteLocalChain(t.Context(), middle.id); err != nil {
		t.Fatal(err)
	}
	assertReclaimed(t, f, middle, true)
	assertReclaimed(t, f, first, false)
	assertReclaimed(t, f, latest, false)
	assertReservationEmpty(t, f, middle)
	// Records, checkpoints, and metadata stay; only local presence flips.
	for _, chain := range []testChain{first, middle, latest} {
		assertRecordsRetained(t, f, chain, chain.id != middle.id)
	}
	// Repeating the operation is a no-op, and the chain is still described the
	// same way, so a retried UI action cannot fail.
	if err := f.r.DeleteLocalChain(t.Context(), middle.id); err != nil {
		t.Fatal(err)
	}
	assertReservationEmpty(t, f, middle)
	assertEligibility(t, f, middle.id, true, "")
}

// A chain is deleted whole or not at all. Removing the old end of a run never
// leaves a gap: the newer chains keep every object, and the deleted chain loses
// every object, because a partial chain would break restore planning.
func TestDeleteLocalChainNeverLeavesMiddleLinks(t *testing.T) {
	f := newFixture(t)
	first, middle, latest := f.newChain(3), f.newChain(3), f.newChain(3)
	for _, chain := range []testChain{first, middle, latest} {
		f.verifyChain(chain)
	}
	if err := f.r.DeleteLocalChain(t.Context(), first.id); err != nil {
		t.Fatal(err)
	}
	assertReclaimed(t, f, first, true)
	assertReclaimed(t, f, middle, false)
	assertReclaimed(t, f, latest, false)
	// The chain that lost its payload is still fully described, so its successor
	// chain is still usable and its own local delete stays independent.
	assertRecordsRetained(t, f, first, false)
	if err := f.r.DeleteLocalChain(t.Context(), middle.id); err != nil {
		t.Fatal(err)
	}
	assertReclaimed(t, f, middle, true)
	assertReclaimed(t, f, latest, false)
	assertRecordsRetained(t, f, middle, false)
	// The newest chain can never be deleted while it is the source's latest.
	assertEligibility(t, f, latest.id, false, RetentionLatest)
	assertReclaimed(t, f, latest, false)
}

func TestDeleteLocalChainRefusesIneligibleChain(t *testing.T) {
	f := newFixture(t)
	first, second := f.newChain(2), f.newChain(2)
	f.verifyChain(first)
	f.verifyChain(second)
	if err := f.store.SetPinned(t.Context(), first.id, true); err != nil {
		t.Fatal(err)
	}
	err := f.r.DeleteLocalChain(t.Context(), first.id)
	var ineligible *IneligibleError
	if !errors.As(err, &ineligible) || ineligible.Reason != RetentionPinned || ineligible.ChainID != first.id {
		t.Fatalf("ineligible delete = %v; want reason %s", err, RetentionPinned)
	}
	assertReclaimed(t, f, first, false)
	assertReclaimed(t, f, second, false)
	assertRecordsRetained(t, f, first, true)
	assertReservationEmpty(t, f, first)
}

func TestDeleteLocalChainFinishesAfterLaterPin(t *testing.T) {
	f := newFixture(t)
	first, second := f.newChain(2), f.newChain(2)
	f.verifyChain(first)
	f.verifyChain(second)
	if _, err := f.store.ReserveLocalDelete(t.Context(), first.id, f.now); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SetPinned(t.Context(), first.id, true); err != nil {
		t.Fatal(err)
	}
	if err := f.r.DeleteLocalChain(t.Context(), first.id); err != nil {
		t.Fatal(err)
	}
	assertReclaimed(t, f, first, true)
	assertReservationEmpty(t, f, first)
	assertRecordsRetained(t, f, first, false)
	assertReclaimed(t, f, second, false)
}

func TestRecoverFinishesPendingLocalDeleteAfterLaterPin(t *testing.T) {
	f := newFixture(t)
	first, second := f.newChain(2), f.newChain(2)
	f.verifyChain(first)
	f.verifyChain(second)
	if _, err := f.store.ReserveLocalDelete(t.Context(), first.id, f.now); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SetPinned(t.Context(), first.id, true); err != nil {
		t.Fatal(err)
	}
	if err := f.r.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertReclaimed(t, f, first, true)
	assertReservationEmpty(t, f, first)
	assertRecordsRetained(t, f, first, false)
	assertReclaimed(t, f, second, false)
}

func TestRecoverFinishesFailedLocalDelete(t *testing.T) {
	f := newFixture(t)
	first, second := f.newChain(2), f.newChain(2)
	f.verifyChain(first)
	f.verifyChain(second)
	if err := f.repo.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.r.DeleteLocalChain(t.Context(), first.id); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("closed repository reclaim = %v; want %v", err, fs.ErrClosed)
	}
	assertReclaimed(t, f, first, false)
	assertRetainedReservation(t, f, first)
	resumed := f.replicator(openRepository(t, f.root))
	if err := resumed.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertReclaimed(t, f, first, true)
	assertReservationEmpty(t, f, first)
	assertRecordsRetained(t, f, first, false)
	assertReclaimed(t, f, second, false)
}

func TestEligibleNeverDeletesAnything(t *testing.T) {
	f := newFixture(t)
	first, second := f.newChain(2), f.newChain(2)
	f.verifyChain(first)
	f.verifyChain(second)
	for range 3 {
		assertEligibility(t, f, first.id, true, "")
	}
	assertReclaimed(t, f, first, false)
	assertReservationEmpty(t, f, first)
	assertRecordsRetained(t, f, first, true)
}

func TestDeleteLocalChainResumesAfterCrash(t *testing.T) {
	f := newFixture(t)
	first, second := f.newChain(2), f.newChain(2)
	f.verifyChain(first)
	f.verifyChain(second)
	// A crash right after the durable reservation leaves the rows and the
	// directories in place, with local_present already cleared.
	reserved, err := f.store.ReserveLocalDelete(t.Context(), first.id, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if len(reserved) != len(first.backups) {
		t.Fatalf("reservation covers %d members; want %d", len(reserved), len(first.backups))
	}
	assertReclaimed(t, f, first, false)
	assertRecordsRetained(t, f, first, false)
	// The rerun finishes the interrupted reclaim.
	if err = f.r.DeleteLocalChain(t.Context(), first.id); err != nil {
		t.Fatal(err)
	}
	assertReclaimed(t, f, first, true)
	assertReservationEmpty(t, f, first)
	assertRecordsRetained(t, f, first, false)
}

func TestDeleteLocalChainRetainsFailedReservation(t *testing.T) {
	f := newFixture(t)
	first, second := f.newChain(2), f.newChain(2)
	f.verifyChain(first)
	f.verifyChain(second)
	// A closed repository fails the reclaim after the reservation commits.
	if err := f.repo.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.r.DeleteLocalChain(t.Context(), first.id); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("closed repository reclaim = %v; want %v", err, fs.ErrClosed)
	}
	assertReclaimed(t, f, first, false)
	assertRetainedReservation(t, f, first)
	// Reopening the same root and running again completes the delete.
	resumed := f.replicator(openRepository(t, f.root))
	if err := resumed.DeleteLocalChain(t.Context(), first.id); err != nil {
		t.Fatal(err)
	}
	assertReclaimed(t, f, first, true)
	assertReservationEmpty(t, f, first)
	assertReclaimed(t, f, second, false)
}

func assertEligibility(t *testing.T, f *fixture, chainID string, want bool, reason string) {
	t.Helper()
	eligible, got, err := f.r.Eligible(t.Context(), chainID)
	if err != nil {
		t.Fatalf("eligibility of %s: %v", chainID, err)
	}
	if eligible != want || got != reason {
		t.Fatalf("eligibility of %s = %t %q; want %t %q", chainID, eligible, got, want, reason)
	}
}

func assertReclaimed(t *testing.T, f *fixture, chain testChain, want bool) {
	t.Helper()
	for _, objectPath := range chain.objects {
		if f.exists(objectPath) != !want {
			t.Fatalf("object %s present = %t; want reclaimed %t", objectPath, !want, want)
		}
	}
}

// assertRecordsRetained checks that rows, checkpoints, and metadata survive and
// that local presence matches the requested state.
func assertRecordsRetained(t *testing.T, f *fixture, chain testChain, present bool) {
	t.Helper()
	if _, err := f.store.Chain(t.Context(), chain.id); err != nil {
		t.Fatalf("chain record %s: %v", chain.id, err)
	}
	members, err := f.store.ChainBackups(t.Context(), chain.id)
	if err != nil || len(members) != len(chain.backups) {
		t.Fatalf("chain members %s: %+v %v", chain.id, members, err)
	}
	for _, member := range members {
		assertCompletedRecord(t, member)
		if member.LocalPresent != present {
			t.Fatalf("member %s local_present = %t; want %t", member.ID, member.LocalPresent, present)
		}
	}
}

func assertCompletedRecord(t *testing.T, member store.Backup) {
	t.Helper()
	if member.Status != "completed" || !member.CompletedAt.After(time.Time{}) ||
		len(member.CheckpointsRaw) == 0 || len(member.InfoRaw) == 0 ||
		member.PayloadSize == 0 || member.PayloadSHA256 == "" || member.MetadataSHA256 == "" {
		t.Fatalf("member record changed: %+v", member)
	}
}

func assertReservationEmpty(t *testing.T, f *fixture, chain testChain) {
	t.Helper()
	reserved, err := f.store.LocalDeletePending(t.Context(), chain.id)
	if err != nil || len(reserved) != 0 {
		t.Fatalf("reservation of %s = %+v %v; want none", chain.id, reserved, err)
	}
}

func assertRetainedReservation(t *testing.T, f *fixture, chain testChain) {
	t.Helper()
	reserved, err := f.store.LocalDeletePending(t.Context(), chain.id)
	if err != nil || len(reserved) != len(chain.backups) {
		t.Fatalf("reservation of %s = %+v %v; want %d rows", chain.id, reserved, err, len(chain.backups))
	}
	var attempts int64
	var code sql.NullString
	for _, row := range reserved {
		err = f.db.QueryRowContext(t.Context(),
			"SELECT attempts, last_error_code FROM cleanup_ledger WHERE id = ?", row.ID).Scan(&attempts, &code)
		if err != nil || attempts != 1 || code.String != "cleanup_failed" {
			t.Fatalf("reservation retry state = %d %q %v", attempts, code.String, err)
		}
	}
}

// TestDeleteLocalChainSkipsAnotherPairsReservation is the pair-scoping rule for
// the library entry point: a pending reservation that belongs to another local
// repository sharing this FTP repository is never reclaimed here and never
// marked done, so its own pair can finish it in the right root.
func TestDeleteLocalChainSkipsAnotherPairsReservation(t *testing.T) {
	f := newFixture(t)
	first, second := f.newChain(2), f.newChain(2)
	f.verifyChain(first)
	f.verifyChain(second)

	// The reservation of the first chain belongs to another pair: its replica
	// row names a different local repository, which is exactly what the queue
	// binding of a shared FTP repository looks like.
	const otherLocalRepository = "other-local-repository"
	if _, err := f.store.ReserveLocalDelete(t.Context(), first.id, f.now); err != nil {
		t.Fatal(err)
	}
	rebindReplicas(t, f, first, otherLocalRepository)

	// Deleting through this pair must leave the other pair's reservation and its
	// objects alone.
	if err := f.r.DeleteLocalChain(t.Context(), first.id); err != nil {
		t.Fatal(err)
	}
	assertReclaimed(t, f, first, false)
	reserved, err := f.store.LocalDeletePending(t.Context(), first.id)
	if err != nil || len(reserved) != len(first.backups) {
		t.Fatalf("reservation of %s = %+v %v; want the other pair's %d rows left alone",
			first.id, reserved, err, len(first.backups))
	}
	assertReplicaBinding(t, f, first, otherLocalRepository)

	// The owning pair finishes it, in its own root.
	owner := f.replicator(f.repo)
	owner.LocalRepositoryID = otherLocalRepository
	if err := owner.DeleteLocalChain(t.Context(), first.id); err != nil {
		t.Fatal(err)
	}
	assertReclaimed(t, f, first, true)
	assertReservationEmpty(t, f, first)
	// The other chain was never part of either reservation.
	assertReclaimed(t, f, second, false)
}

// rebindReplicas moves one chain's replica rows to another local repository.
func rebindReplicas(t *testing.T, f *fixture, chain testChain, localRepositoryID string) {
	t.Helper()
	for _, backupID := range chain.backups {
		if _, err := f.db.ExecContext(t.Context(), `UPDATE replicas SET local_repository_id = ?
WHERE backup_id = ? AND repository_id = ?`, localRepositoryID, backupID, testRepositoryID); err != nil {
			t.Fatal(err)
		}
	}
}

// assertReplicaBinding requires every member's replica row to stay bound to one
// local repository, so no reclaim rebound it to this pair.
func assertReplicaBinding(t *testing.T, f *fixture, chain testChain, localRepositoryID string) {
	t.Helper()
	for _, backupID := range chain.backups {
		var bound string
		if err := f.db.QueryRowContext(t.Context(), `SELECT local_repository_id FROM replicas
WHERE backup_id = ? AND repository_id = ?`, backupID, testRepositoryID).Scan(&bound); err != nil {
			t.Fatal(err)
		}
		if bound != localRepositoryID {
			t.Fatalf("replica of %s is bound to %q, want %q", backupID, bound, localRepositoryID)
		}
	}
}
