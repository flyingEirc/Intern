package replication

import (
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/flyingEirc/Intern/internal/artifact"
	"github.com/flyingEirc/Intern/internal/storage/local"
)

func TestEnqueueIsIdempotent(t *testing.T) {
	f := newEngine(t)
	first := f.newBackup(engineSourceID, 0)
	for range 3 {
		if err := f.r.Enqueue(t.Context(), first.id); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.r.EnqueueCompleted(t.Context(), engineSourceID); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := f.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM replicas").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("replica rows = %d; want 1", rows)
	}
	replica := f.assertStatus(first.id, StatusPending)
	if replica.Attempts != 0 || replica.RepositoryID != engineRepoID {
		t.Fatalf("enqueued replica = %+v", replica)
	}
}

func TestEnqueueRefusesUncompletedOrAbsentPayload(t *testing.T) {
	f := newEngine(t)
	record := f.newBackup(engineSourceID, 0)
	if _, err := f.db.ExecContext(t.Context(), "UPDATE backups SET local_present = 0 WHERE id = ?", record.id); err != nil {
		t.Fatal(err)
	}
	if err := f.r.Enqueue(t.Context(), record.id); err == nil {
		t.Fatal("enqueued a backup without a local payload")
	}
	if err := f.r.Enqueue(t.Context(), "missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing backup: %v", err)
	}
}

// TestStepUploadsThenVerifiesTheReadback proves uploaded is not verified: the
// four commits happen first, and verified requires the authenticated readback.
func TestStepUploadsThenVerifiesTheReadback(t *testing.T) {
	f := newEngine(t)
	record := f.newBackup(engineSourceID, 0)
	if err := f.r.Enqueue(t.Context(), record.id); err != nil {
		t.Fatal(err)
	}
	// Fail the readback at the last member, so all four commits are durable.
	f.remote.failOpen = record.objectPath + "/" + memberMetaEnvelope
	f.assertFails()
	f.assertStatus(record.id, StatusUploaded)
	if members := f.memberStatuses(record.id); members[memberPayload] != memberUploaded {
		t.Fatalf("payload member status = %q", members[memberPayload])
	}
	for _, member := range memberOrder {
		if _, ok := f.remote.object(record.objectPath + "/" + member); !ok {
			t.Fatalf("committed member %s is absent remotely", member)
		}
	}
	// A second attempt must not re-upload any committed member.
	creates := f.remote.createCount(record.objectPath + "/" + memberPayload)
	f.remote.failOpen = ""
	replica := f.assertVerified(record.id)
	if got := f.remote.createCount(record.objectPath + "/" + memberPayload); got != creates {
		t.Fatalf("payload was re-uploaded: creates %d -> %d", creates, got)
	}
	if f.partRows() != 0 {
		t.Fatal("part rows survived a verified replica")
	}
	if replica.VerifiedAt == nil || replica.UploadedAt == nil || replica.AlertSince != nil {
		t.Fatalf("verified replica = %+v", replica)
	}
	assertNoVerifyTemps(t, f.work)
}

// TestFIFOBlockingKeepsTheProtectedPrefixContiguous proves a failed predecessor
// blocks every later item of that source while another source proceeds.
func TestFIFOBlockingKeepsTheProtectedPrefixContiguous(t *testing.T) {
	f := newEngine(t)
	first := f.newBackup(engineSourceID, 0)
	second := f.newBackup(engineSourceID, 1)
	other := f.newBackup(engineOtherSource, 0)
	for _, record := range []backupRecord{first, second, other} {
		if err := f.r.Enqueue(t.Context(), record.id); err != nil {
			t.Fatal(err)
		}
	}
	f.remote.deny[first.objectPath+"/"] = fs.ErrPermission
	f.assertFails()
	f.assertStatus(first.id, StatusFailed)
	if _, ok := f.remote.object(second.objectPath + "/" + memberPayload); ok {
		t.Fatal("later item of a blocked source was copied")
	}
	// Another source is independent.
	f.advanceToDue(other.id)
	f.mustStep()
	f.assertStatus(other.id, StatusVerified)
	f.assertStatus(second.id, StatusPending)
	point, err := f.r.RemotePoint(t.Context(), engineSourceID)
	if err != nil || point != nil {
		t.Fatalf("remote point skipped a failed predecessor: %+v, %v", point, err)
	}
}

// remoteCorruption is one way a copied object stops being authentic.
type remoteCorruption struct {
	name    string
	corrupt func(f *engineFixture, record backupRecord)
	key     func(f *engineFixture) *string
}

var remoteCorruptions = []remoteCorruption{
	{
		name: "tampered payload",
		corrupt: func(f *engineFixture, record backupRecord) {
			key := record.objectPath + "/" + memberPayload
			data, _ := f.remote.object(key)
			data[0] ^= 0xff
			f.remote.put(key, data)
		},
	},
	{
		name: "truncated metadata envelope",
		corrupt: func(f *engineFixture, record backupRecord) {
			key := record.objectPath + "/" + memberMetaEnvelope
			data, _ := f.remote.object(key)
			f.remote.put(key, data[:len(data)-1])
		},
	},
	{
		name: "reordered payload member",
		corrupt: func(f *engineFixture, record backupRecord) {
			envelope := record.objectPath + "/" + memberEnvelope
			payload := record.objectPath + "/" + memberPayload
			envelopeData, _ := f.remote.object(envelope)
			payloadData, _ := f.remote.object(payload)
			f.remote.put(envelope, payloadData)
			f.remote.put(payload, envelopeData)
		},
	},
	{
		name: "wrong chain key",
		key: func(f *engineFixture) *string {
			other := filepath.Join(filepath.Dir(filepath.Dir(f.key)), "other.key")
			if err := artifact.GenerateKey(other); err != nil {
				f.t.Fatal(err)
			}
			return &other
		},
	},
}

func TestTamperedTruncatedAndWrongKeyReadbacksNeverVerify(t *testing.T) {
	for _, tc := range remoteCorruptions {
		t.Run(tc.name, func(t *testing.T) {
			assertCorruptReadbackNeverVerifies(t, tc)
		})
	}
}

func assertCorruptReadbackNeverVerifies(t *testing.T, tc remoteCorruption) {
	t.Helper()
	f := newEngine(t)
	record := f.newBackup(engineSourceID, 0)
	if tc.key != nil {
		f.r = f.replicator(tc.key(f))
	}
	if tc.corrupt != nil {
		// Corrupt the remote copy only after all four commits.
		f.remote.afterCommit = func(string) { tc.corrupt(f, record) }
	}
	if err := f.r.Enqueue(t.Context(), record.id); err != nil {
		t.Fatal(err)
	}
	f.assertFails()
	replica := f.assertStatus(record.id, StatusUploaded)
	if replica.Attempts != 1 || replica.VerifiedAt != nil {
		t.Fatalf("unverified replica = %+v", replica)
	}
	point, err := f.r.RemotePoint(t.Context(), engineSourceID)
	if err != nil || point != nil {
		t.Fatalf("remote point = %+v, %v; want none", point, err)
	}
	f.assertLocalIntact(record)
	assertNoVerifyTemps(t, f.work)
}

// TestLocalCorruptionFailsWithoutRetryOrLocalChange proves the pre-upload check
// refuses a changed local ciphertext and never touches the local row or object.
func TestLocalCorruptionFailsWithoutRetryOrLocalChange(t *testing.T) {
	f := newEngine(t)
	record := f.newBackup(engineSourceID, 0)
	path := f.localFile(record.objectPath, memberPayload)
	if err := os.WriteFile(path, []byte("corrupted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.r.Enqueue(t.Context(), record.id); err != nil {
		t.Fatal(err)
	}
	f.assertFails()
	replica := f.assertStatus(record.id, StatusFailed)
	assertFailureCode(t, replica, CodeLocalCorrupt)
	if replica.AlertSince == nil {
		t.Fatal("local corruption did not alert")
	}
	if replica.NextAttemptAt != nil && replica.NextAttemptAt.After(f.now) {
		t.Fatal("local corruption was scheduled for a retry")
	}
	f.assertNoRemote(record)
	f.assertIdle()
	backup, err := f.store.Backup(t.Context(), record.id)
	if err != nil || backup.Status != "completed" || !backup.LocalPresent {
		t.Fatalf("local row changed: %+v %v", backup, err)
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatalf("local object removed: %v", err)
	}
}

func assertFailureCode(t *testing.T, replica Replica, want string) {
	t.Helper()
	if replica.LastErrorCode != want {
		t.Fatalf("last error code = %q; want %q (%+v)", replica.LastErrorCode, want, replica)
	}
}

func TestUncertainCommitIsAcceptedOnlyByTheReadback(t *testing.T) {
	f := newEngine(t)
	record := f.newBackup(engineSourceID, 0)
	key := record.objectPath + "/" + memberPayload
	f.remote.uncertain[key] = true
	f.remote.uncertainKeepsObject = true
	if err := f.r.Enqueue(t.Context(), record.id); err != nil {
		t.Fatal(err)
	}
	f.assertFails()
	if members := f.memberStatuses(record.id); members[memberPayload] != memberUncertain {
		t.Fatalf("member state = %q; want uncertain", members[memberPayload])
	}
	if _, ok := f.remote.object(key); !ok {
		t.Fatal("the uncertain final key was not retained")
	}
	// The next attempt adopts the existing key without another upload and proves
	// it by the readback only.
	creates := f.remote.createCount(key)
	f.assertVerified(record.id)
	if got := f.remote.createCount(key); got != creates {
		t.Fatalf("uncertain member was re-uploaded: %d -> %d", creates, got)
	}
}

func TestUncertainCommitWithHashMismatchIsAConflict(t *testing.T) {
	f := newEngine(t)
	record := f.newBackup(engineSourceID, 0)
	key := record.objectPath + "/" + memberPayload
	f.remote.uncertain[key] = true
	f.remote.uncertainKeepsObject = true
	if err := f.r.Enqueue(t.Context(), record.id); err != nil {
		t.Fatal(err)
	}
	f.assertFails()
	// A different object now occupies the final key.
	f.remote.put(key, []byte("someone else's object"))
	f.advanceToDue(record.id)
	f.assertFails()
	replica := f.assertStatus(record.id, StatusFailed)
	assertFailureCode(t, replica, CodeRemoteConflict)
	if replica.AlertSince == nil {
		t.Fatal("remote conflict did not alert")
	}
	if data, _ := f.remote.object(key); string(data) != "someone else's object" {
		t.Fatal("the conflicting remote object was overwritten")
	}
}

func TestUncertainCommitWithoutFinalKeyUsesANewPart(t *testing.T) {
	f := newEngine(t)
	record := f.newBackup(engineSourceID, 0)
	key := record.objectPath + "/" + memberPayload
	f.remote.uncertain[key] = true
	f.remote.uncertainKeepsObject = false
	if err := f.r.Enqueue(t.Context(), record.id); err != nil {
		t.Fatal(err)
	}
	f.assertFails()
	if _, ok := f.remote.object(key); ok {
		t.Fatal("an uncertain commit invented a final object")
	}
	delete(f.remote.uncertain, key)
	first := f.remote.createCount(key)
	f.assertVerified(record.id)
	if got := f.remote.createCount(key); got != first+1 {
		t.Fatalf("creates %d -> %d; want a fresh part", first, got)
	}
}

// TestPreexistingFinalKeyIsNeverOverwritten covers an already-present final key
// that this attempt did not publish: identical bytes are accepted only by the
// readback, different bytes are a conflict, and neither is overwritten.
func TestPreexistingFinalKeyIsNeverOverwritten(t *testing.T) {
	for _, tc := range []struct {
		name     string
		replace  bool
		wantCode string
	}{
		{name: "identical bytes verify"},
		{name: "different bytes conflict", replace: true, wantCode: CodeRemoteConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertPreexistingFinalKey(t, tc.replace, tc.wantCode)
		})
	}
}

func assertPreexistingFinalKey(t *testing.T, replace bool, wantCode string) {
	t.Helper()
	f := newEngine(t)
	record := f.newBackup(engineSourceID, 0)
	key := record.objectPath + "/" + memberPayload
	local, err := os.ReadFile(f.localFile(record.objectPath, memberPayload))
	if err != nil {
		t.Fatal(err)
	}
	if replace {
		local = []byte("another publisher's object")
	}
	f.remote.put(key, local)
	if err = f.r.Enqueue(t.Context(), record.id); err != nil {
		t.Fatal(err)
	}
	if replace {
		f.assertFails()
		replica := f.assertStatus(record.id, StatusFailed)
		assertFailureCode(t, replica, wantCode)
	} else {
		f.mustStep()
		f.assertStatus(record.id, StatusVerified)
	}
	if data, _ := f.remote.object(key); string(data) != string(local) {
		t.Fatal("the existing final key was overwritten")
	}
	if got := f.remote.createCount(key); got != 0 {
		t.Fatalf("the existing final key was uploaded %d times", got)
	}
}

// TestPartsAreRecordedBeforeTheFirstWriteAndRecovered proves the temporary key
// obligation is durable before payload bytes, and that Recover retries it.
func TestPartsAreRecordedBeforeTheFirstWriteAndRecovered(t *testing.T) {
	f := newEngine(t)
	record := f.newBackup(engineSourceID, 0)
	if err := f.r.Enqueue(t.Context(), record.id); err != nil {
		t.Fatal(err)
	}
	assertPartsRecordedBeforeWrite(t, f)
	f.mustStep()
	if f.partRows() != 0 {
		t.Fatal("committed parts were retained")
	}
	assertInterruptedUploadRecovers(t, f)
}

// assertPartsRecordedBeforeWrite observes every member write and requires its
// part row to exist already.
func assertPartsRecordedBeforeWrite(t *testing.T, f *engineFixture) {
	t.Helper()
	f.remote.onWrite = func(part string) {
		var rows int
		if err := f.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM replica_parts WHERE part = ?", part).Scan(&rows); err != nil {
			t.Errorf("part lookup: %v", err)
		}
		if rows != 1 {
			t.Errorf("write ran with %d recorded parts", rows)
		}
	}
	t.Cleanup(func() { f.remote.onWrite = nil })
}

// assertInterruptedUploadRecovers simulates a crash between the upload status
// change and cleanup, then requires Recover and Step to finish the item.
func assertInterruptedUploadRecovers(t *testing.T, f *engineFixture) {
	t.Helper()
	f.remote.onWrite = nil
	interrupted := f.newBackup(engineSourceID, 1)
	if err := f.r.Enqueue(t.Context(), interrupted.id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(t.Context(), "UPDATE replicas SET status = 'uploading' WHERE backup_id = ?", interrupted.id); err != nil {
		t.Fatal(err)
	}
	part := ".intern-upload-000000000000000000000000000000ff.part"
	f.remote.mu.Lock()
	f.remote.parts[part] = []byte("partial")
	f.remote.mu.Unlock()
	if _, err := f.db.ExecContext(t.Context(), `INSERT INTO replica_parts (backup_id, repository_id, member, part, created_at)
VALUES (?, ?, 'payload.bin', ?, '2026-10-26T09:00:00.000000000Z')`, interrupted.id, engineRepoID, part); err != nil {
		t.Fatal(err)
	}
	if err := f.r.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.assertStatus(interrupted.id, StatusPending)
	if f.partRows() != 0 || f.remote.partCount() != 0 {
		t.Fatalf("cleanup left parts: rows=%d remote=%d", f.partRows(), f.remote.partCount())
	}
	f.mustStep()
	f.assertStatus(interrupted.id, StatusVerified)
}

func TestCleanupFailureKeepsThePartRow(t *testing.T) {
	f := newEngine(t)
	record := f.newBackup(engineSourceID, 0)
	f.remote.failWrite = record.objectPath + "/" + memberPayload
	f.remote.cleanupErr = fs.ErrPermission
	if err := f.r.Enqueue(t.Context(), record.id); err != nil {
		t.Fatal(err)
	}
	f.assertFails()
	if f.partRows() != 1 {
		t.Fatalf("part rows = %d; want the failed cleanup recorded", f.partRows())
	}
	if err := f.r.Recover(t.Context()); err == nil {
		t.Fatal("recover hid a pending cleanup")
	}
	if f.partRows() != 1 {
		t.Fatalf("part rows = %d; want the pending cleanup retained", f.partRows())
	}
	f.remote.cleanupErr = nil
	if err := f.r.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.partRows() != 0 {
		t.Fatal("a successful cleanup retained its part row")
	}
}

func TestBackoffExhaustionAlertAndManualRetry(t *testing.T) {
	f := newEngine(t)
	record := f.newBackup(engineSourceID, 0)
	f.remote.deny[record.objectPath+"/"] = fs.ErrPermission
	if err := f.r.Enqueue(t.Context(), record.id); err != nil {
		t.Fatal(err)
	}
	start := f.now
	f.assertFails()
	replica := f.assertStatus(record.id, StatusFailed)
	assertBackoff(t, replica, 1, start.Add(time.Minute))
	if replica.AlertSince != nil {
		t.Fatal("a fresh retryable failure alerted immediately")
	}
	f.assertIdle()
	f.advanceToDue(record.id)
	f.assertFails()
	replica = f.assertStatus(record.id, StatusFailed)
	assertBackoff(t, replica, 2, f.now.Add(2*time.Minute))
	// Exhaustion on the last allowed attempt.
	f.advanceToDue(record.id)
	f.assertFails()
	replica = f.assertStatus(record.id, StatusExhausted)
	if replica.Attempts != 3 || replica.AlertSince == nil {
		t.Fatalf("exhausted replica = %+v", replica)
	}
	f.assertIdle()
	assertManualRetryRestoresAttemptBudget(t, f, record, replica)
}

func assertBackoff(t *testing.T, replica Replica, attempts int, want time.Time) {
	t.Helper()
	if replica.Attempts != attempts || replica.NextAttemptAt == nil || !replica.NextAttemptAt.Equal(want) {
		t.Fatalf("attempt %d backoff = %+v; want next %s", attempts, replica, want)
	}
}

// assertManualRetryRestoresAttemptBudget requires Retry to reset attempts and the
// schedule while keeping the failure history, and the next attempt to succeed.
func assertManualRetryRestoresAttemptBudget(t *testing.T, f *engineFixture, record backupRecord, exhausted Replica) {
	t.Helper()
	failedSince := f.failedSince(record.id)
	if err := f.r.Retry(t.Context(), record.id); err != nil {
		t.Fatal(err)
	}
	replica := f.assertStatus(record.id, StatusPending)
	if replica.Attempts != 0 || replica.NextAttemptAt == nil || !replica.NextAttemptAt.Equal(f.now) {
		t.Fatalf("retried replica = %+v", replica)
	}
	if kept := f.failedSince(record.id); failedSince == nil || kept == nil || !kept.Equal(*failedSince) {
		t.Fatalf("manual retry discarded the failure history: %v -> %v", failedSince, kept)
	}
	assertFailureCode(t, replica, CodeRemoteOwnership)
	f.remote.deny = map[string]error{}
	f.mustStep()
	replica = f.assertStatus(record.id, StatusVerified)
	if replica.AlertSince != nil {
		t.Fatal("verification did not clear the alert")
	}
}

func TestAlertAfterSustainedRetryableFailure(t *testing.T) {
	f := newEngine(t)
	record := f.newBackup(engineSourceID, 0)
	f.r.Policy.MaxAttempts = 100
	f.remote.deny[record.objectPath+"/"] = fs.ErrPermission
	if err := f.r.Enqueue(t.Context(), record.id); err != nil {
		t.Fatal(err)
	}
	start := f.now
	f.assertFails()
	f.now = start.Add(6 * time.Hour)
	f.assertFails()
	replica := f.assertStatus(record.id, StatusFailed)
	if replica.AlertSince == nil || !replica.AlertSince.Equal(f.now) {
		t.Fatalf("sustained failure alert = %+v", replica)
	}
}

func TestPointsFollowTheirOwnPrefixes(t *testing.T) {
	f := newEngine(t)
	first := f.newBackup(engineSourceID, 0)
	second := f.newBackup(engineSourceID, 1)
	for _, record := range []backupRecord{first, second} {
		if err := f.r.Enqueue(t.Context(), record.id); err != nil {
			t.Fatal(err)
		}
	}
	local, err := LocalPoint(t.Context(), f.store, engineSourceID)
	if err != nil || local == nil || local.BackupID != second.id || local.ChainID != second.chainID ||
		local.At.IsZero() {
		t.Fatalf("local point = %+v, %v", local, err)
	}
	f.mustStep()
	assertRemotePoint(t, f, first)
	f.mustStep()
	assertRemotePoint(t, f, second)
	assertGapStopsRemotePoint(t, f, first)
}

// assertGapStopsRemotePoint requires a non-verified predecessor to end the
// verified prefix even when a later item is verified.
func assertGapStopsRemotePoint(t *testing.T, f *engineFixture, predecessor backupRecord) {
	t.Helper()
	if _, err := f.db.ExecContext(t.Context(), "UPDATE replicas SET status = 'failed' WHERE backup_id = ?", predecessor.id); err != nil {
		t.Fatal(err)
	}
	point, err := f.r.RemotePoint(t.Context(), engineSourceID)
	if err != nil || point != nil {
		t.Fatalf("remote point bridged a gap: %+v, %v", point, err)
	}
}

func assertRemotePoint(t *testing.T, f *engineFixture, want backupRecord) {
	t.Helper()
	point, err := f.r.RemotePoint(t.Context(), engineSourceID)
	if err != nil || point == nil {
		t.Fatalf("remote point = %+v, %v", point, err)
	}
	if point.BackupID != want.id || point.ChainID != want.chainID || point.At.After(f.now) {
		t.Fatalf("remote point = %+v; want %s", point, want.id)
	}
}

func TestLocalPointStopsAtAnIncompleteMember(t *testing.T) {
	f := newEngine(t)
	first := f.newBackup(engineSourceID, 0)
	second := f.newBackup(engineSourceID, 1)
	if _, err := f.db.ExecContext(t.Context(), "UPDATE backups SET status = 'running', completed_at = NULL WHERE id = ?", second.id); err != nil {
		t.Fatal(err)
	}
	point, err := LocalPoint(t.Context(), f.store, engineSourceID)
	if err != nil || point == nil || point.BackupID != first.id {
		t.Fatalf("local point = %+v, %v", point, err)
	}
}

func TestStepRefusesAnUnusableWorkRoot(t *testing.T) {
	f := newEngine(t)
	record := f.newBackup(engineSourceID, 0)
	if err := f.r.Enqueue(t.Context(), record.id); err != nil {
		t.Fatal(err)
	}
	f.r.WorkRoot = filepath.Join(f.root, "..", "missing")
	if processed, err := f.step(); processed || err == nil {
		t.Fatalf("step with a missing work root = %v, %v", processed, err)
	}
	f.assertStatus(record.id, StatusPending)
}

func assertNoVerifyTemps(t *testing.T, work string) {
	t.Helper()
	entries, err := os.ReadDir(work)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		t.Fatalf("verification left %s behind", entry.Name())
	}
}

// TestSharedReplicaRepositoryKeepsLocalRepositoriesApart is N2: one replica
// repository serves two local repositories. Each pair has its own replicator, and
// neither may step the other's backup. Both backups are real published objects,
// so a cross-pair step would fail on the wrong root instead of passing silently;
// the test therefore proves the queue scoping, not just the outcome.
func TestSharedReplicaRepositoryKeepsLocalRepositoriesApart(t *testing.T) {
	f := newEngine(t)
	otherRepo := openSecondLocalRepository(t, f)

	// Two independent sources, each in its own local repository. A separate
	// source per repository keeps the FIFO prefix logic out of the way: what is
	// under test is the repository binding.
	first := f.newBackupIn(f.repo, engineSourceID, 0)
	second := f.newBackupIn(otherRepo, engineOtherSource, 0)

	firstReplicator := f.replicator(nil)
	secondReplicator := f.replicator(nil)
	secondReplicator.Local = otherRepo
	secondReplicator.LocalRepositoryID = engineOtherLocalRepoID

	// Production enqueues a backup only on the policy's own pair.
	for _, enqueue := range []struct {
		replicator *Replicator
		record     backupRecord
	}{{firstReplicator, first}, {secondReplicator, second}} {
		if err := enqueue.replicator.Enqueue(t.Context(), enqueue.record.id); err != nil {
			t.Fatal(err)
		}
	}

	// Each pair's queue holds only the rows bound to its own local repository,
	// even though both rows name the same replica repository. The scoping is
	// load-bearing: the second pair's object is absent from the first root, so a
	// pair that stepped the other's backup would fail instead of verifying it.
	assertQueueHoldsOnly(t, firstReplicator, first.id)
	assertQueueHoldsOnly(t, secondReplicator, second.id)
	if _, err := f.repo.Size(second.objectPath + "/" + memberPayload); err == nil {
		t.Fatal("the second pair's payload is visible in the first local repository")
	}

	// One step per pair reaches verified through this pair's own root.
	stepped := assertPairStepsItsOwn(t, firstReplicator, first.id, engineLocalRepoID)
	if stepped.Status != StatusVerified {
		t.Fatalf("first pair status = %q, want %s", stepped.Status, StatusVerified)
	}
	stepped = assertPairStepsItsOwn(t, secondReplicator, second.id, engineOtherLocalRepoID)
	if stepped.Status != StatusVerified {
		t.Fatalf("second pair status = %q, want %s", stepped.Status, StatusVerified)
	}

	// Every replica reaches verified without an error; the step above already
	// proved which local repository produced it.
	for _, pair := range []struct {
		replicator *Replicator
		record     backupRecord
	}{
		{firstReplicator, first},
		{secondReplicator, second},
	} {
		replica, err := pair.replicator.Replica(t.Context(), pair.record.id)
		if err != nil {
			t.Fatal(err)
		}
		if replica.Status != StatusVerified || replica.LastErrorCode != "" {
			t.Fatalf("replica of %s = %+v, want verified without an error", pair.record.id, replica)
		}
	}
}

// openSecondLocalRepository creates the second local repository of the shared
// replica repository, outside the fixture's own root.
func openSecondLocalRepository(t *testing.T, f *engineFixture) *local.Repository {
	t.Helper()
	root := filepath.Join(filepath.Dir(f.root), "objects-other")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	repo, err := local.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

// assertQueueHoldsOnly requires one pair's queue to hold exactly one item: the
// backup stored in that pair's local repository.
func assertQueueHoldsOnly(t *testing.T, replicator *Replicator, want string) {
	t.Helper()
	items, err := replicator.queue(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].backupID != want {
		t.Fatalf("%s pair queue = %+v, want only %s", replicator.RepositoryID, items, want)
	}
}

// assertPairStepsItsOwn steps one pair and requires it to have processed its own
// backup out of its own local repository.
func assertPairStepsItsOwn(t *testing.T, replicator *Replicator, wantBackup, wantLocal string) Stepped {
	t.Helper()
	stepped, attempted, err := replicator.StepItem(t.Context())
	if err != nil || !attempted {
		t.Fatalf("step = %+v, attempted %v, %v", stepped, attempted, err)
	}
	if stepped.BackupID != wantBackup || stepped.LocalRepositoryID != wantLocal {
		t.Fatalf("stepped %+v, want %s in %s", stepped, wantBackup, wantLocal)
	}
	return stepped
}
