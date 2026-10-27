package replication

import (
	"database/sql"
	"errors"
	"testing"
)

// TestRealStepPassesThroughUploadedBeforeVerified attaches the attempt hook at
// the "uploaded" boundary of a real Step and reads the durable row there: the
// copy is committed but not yet proven, so the status must be exactly uploaded,
// the upload time must be recorded, verification must not have happened, and the
// row must be bound to this pair's local repository. The same row reaches
// verified only after the readback.
func TestRealStepPassesThroughUploadedBeforeVerified(t *testing.T) {
	f := newEngine(t)
	record := f.newBackup(engineSourceID, 0)
	if err := f.r.Enqueue(t.Context(), record.id); err != nil {
		t.Fatal(err)
	}
	atUploaded := f.captureUploadedBoundary(record.id)
	f.mustStep()
	requireUploadedBoundary(t, *atUploaded)
	requireVerifiedAfterUpload(t, f.assertStatus(record.id, StatusVerified))
}

// captureUploadedBoundary installs the attempt hook that reads the durable row
// at the "uploaded" boundary of the next Step and returns the slot it fills. The
// hook must not call back into the Replicator, so it reads the table through the
// fixture's own connection.
func (f *engineFixture) captureUploadedBoundary(backupID string) *replicaSnapshot {
	f.t.Helper()
	snapshot := new(replicaSnapshot)
	original := f.r.hook
	f.t.Cleanup(func() { f.r.hook = original })
	f.r.hook = stepHook(func(point string) error {
		if point != "uploaded" {
			return nil
		}
		read, err := f.snapshot(backupID)
		if err != nil {
			return err
		}
		*snapshot = read
		return nil
	})
	return snapshot
}

// requireUploadedBoundary requires the durable row to be exactly uploaded and
// bound to this pair's local repository, with no verification yet.
func requireUploadedBoundary(t *testing.T, snapshot replicaSnapshot) {
	t.Helper()
	if snapshot.status != StatusUploaded {
		t.Fatalf("status at the uploaded boundary = %q, want %q", snapshot.status, StatusUploaded)
	}
	if snapshot.localRepositoryID != engineLocalRepoID {
		t.Fatalf("uploaded row is bound to local repository %q, want %q",
			snapshot.localRepositoryID, engineLocalRepoID)
	}
	if !snapshot.uploaded.Valid || snapshot.verified.Valid {
		t.Fatalf("uploaded boundary times: uploaded=%v verified=%v", snapshot.uploaded, snapshot.verified)
	}
}

// requireVerifiedAfterUpload requires the settled row to carry both times, with
// the upload first.
func requireVerifiedAfterUpload(t *testing.T, verified Replica) {
	t.Helper()
	if verified.UploadedAt == nil || verified.VerifiedAt == nil {
		t.Fatalf("verified replica has no upload and verify times: %+v", verified)
	}
	if verified.VerifiedAt.Before(*verified.UploadedAt) {
		t.Fatalf("verification %s precedes the upload %s", verified.VerifiedAt, verified.UploadedAt)
	}
}

// replicaSnapshot is one raw replicas row, read without the engine's accessors.
type replicaSnapshot struct {
	status             string
	localRepositoryID  string
	uploaded, verified sql.NullString
}

func (f *engineFixture) snapshot(backupID string) (replicaSnapshot, error) {
	var out replicaSnapshot
	err := f.db.QueryRowContext(f.t.Context(), `
SELECT status, local_repository_id, uploaded_at, verified_at FROM replicas
WHERE backup_id = ? AND repository_id = ?`, backupID, engineRepoID,
	).Scan(&out.status, &out.localRepositoryID, &out.uploaded, &out.verified)
	return out, err
}

// TestReplicaTransitionsRequireTheLocalRepositoryBinding proves every status
// transition that must hit exactly one row is scoped to this pair's local
// repository. Run through another local repository behind the same replica
// repository, each transition reports an error and leaves the row unchanged;
// run through its own pair, it writes the status. A transition that matched
// nothing without reporting it would leave a stale status behind, which is
// exactly the defect that kept the uploaded window invisible.
func TestReplicaTransitionsRequireTheLocalRepositoryBinding(t *testing.T) {
	transitions := []struct {
		name string
		// apply runs one transition and returns its error.
		apply     func(t *testing.T, r *Replicator, item *queueItem, backupID string) error
		wantAfter string
	}{
		{
			name: "uploading",
			apply: func(t *testing.T, r *Replicator, item *queueItem, _ string) error {
				t.Helper()
				return r.begin(t.Context(), item)
			},
			wantAfter: StatusUploading,
		},
		{
			name: "uploaded",
			apply: func(t *testing.T, r *Replicator, _ *queueItem, backupID string) error {
				t.Helper()
				members := map[string]memberRecord{}
				for _, member := range memberOrder {
					members[member] = memberRecord{status: memberUploaded}
				}
				return r.markUploaded(t.Context(), backupID, members)
			},
			wantAfter: StatusUploaded,
		},
		{
			name: "verified",
			apply: func(t *testing.T, r *Replicator, _ *queueItem, backupID string) error {
				t.Helper()
				return r.recordVerified(t.Context(), backupID)
			},
			wantAfter: StatusVerified,
		},
		{
			name:      "failed",
			apply:     applyFailure,
			wantAfter: StatusFailed,
		},
		{
			name: "retried",
			apply: func(t *testing.T, r *Replicator, _ *queueItem, backupID string) error {
				t.Helper()
				return r.Retry(t.Context(), backupID)
			},
			wantAfter: StatusPending,
		},
	}
	for _, transition := range transitions {
		t.Run(transition.name, func(t *testing.T) {
			assertTransitionScopedToPair(t, transition.name, transition.wantAfter, transition.apply)
		})
	}
}

// applyFailure records one retryable failure through the pair's own row.
func applyFailure(t *testing.T, r *Replicator, item *queueItem, _ string) error {
	t.Helper()
	failure := asReplicaFailure(retryable(CodeRemoteTransport, errors.New("refused")))
	return r.writeFailure(t.Context(), item, failure, StatusFailed, 1, 0)
}

// assertTransitionScopedToPair runs one transition through a foreign local
// repository and then through the pair's own one, requiring the first attempt to
// report an error without changing the row and the second to write the status.
func assertTransitionScopedToPair(t *testing.T, name, want string,
	apply func(*testing.T, *Replicator, *queueItem, string) error,
) {
	t.Helper()
	f := newEngine(t)
	record := f.newBackup(engineSourceID, 0)
	if err := f.r.Enqueue(t.Context(), record.id); err != nil {
		t.Fatal(err)
	}
	item := &queueItem{backupID: record.id, status: StatusPending, nextAttemptAt: f.now}
	before := requireSnapshot(t, f, record.id)

	other := f.replicator(nil)
	other.LocalRepositoryID = engineOtherLocalRepoID
	if err := apply(t, other, item, record.id); err == nil {
		t.Fatal("a transition through another local repository matched a row")
	}
	if unchanged := requireSnapshot(t, f, record.id); unchanged != before {
		t.Fatalf("row changed through another local repository: %+v -> %+v", before, unchanged)
	}

	if err := apply(t, f.r, item, record.id); err != nil {
		t.Fatalf("transition through its own local repository: %v", err)
	}
	after := requireSnapshot(t, f, record.id)
	if after.status != want {
		t.Fatalf("status after %s = %q, want %q", name, after.status, want)
	}
	if after.localRepositoryID != engineLocalRepoID {
		t.Fatalf("status after %s is bound to %q, want %q", name, after.localRepositoryID, engineLocalRepoID)
	}
}

// requireSnapshot reads the replica row and fails on a read error.
func requireSnapshot(t *testing.T, f *engineFixture, backupID string) replicaSnapshot {
	t.Helper()
	snapshot, err := f.snapshot(backupID)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

// TestUploadedTransitionReportsAMissingRow proves the uploaded write is checked
// even though the statement is a plain UPDATE: with no row to match it returns a
// state error instead of a silent success that would leave the status stale.
func TestUploadedTransitionReportsAMissingRow(t *testing.T) {
	f := newEngine(t)
	record := f.newBackup(engineSourceID, 0)
	members := map[string]memberRecord{}
	for _, member := range memberOrder {
		members[member] = memberRecord{status: memberUploaded}
	}
	if _, err := f.db.ExecContext(t.Context(), `DELETE FROM replicas WHERE backup_id = ?`, record.id); err != nil {
		t.Fatal(err)
	}
	err := f.r.markUploaded(t.Context(), record.id, members)
	if err == nil {
		t.Fatal("markUploaded reported success for a row it did not update")
	}
	var failure *replicaFailure
	if !errors.As(err, &failure) || failure.code != CodeStateInvalid || failure.retryable {
		t.Fatalf("markUploaded error = %v; want a non-retryable %s", err, CodeStateInvalid)
	}
}
