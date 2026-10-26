package replication

import (
	"context"
	"database/sql"
	"errors"
	"time"

	physicalstore "github.com/flyingEirc/Intern/internal/physical/store"
	"github.com/flyingEirc/Intern/internal/replication/internal/replsql"
	"github.com/flyingEirc/Intern/internal/storage/ftp"
)

// Replica is the observable state of one (backup, repository) copy. The times
// are nil until the corresponding step happened; NextAttemptAt is the schedule
// the queue enforces.
type Replica struct {
	BackupID, RepositoryID, Status string
	Attempts                       int
	NextAttemptAt                  *time.Time
	UploadedAt, VerifiedAt         *time.Time
	AlertSince                     *time.Time
	LastErrorCode                  string
	LastReplyCode                  int
}

// idemKey is the durable idempotency key of one replica.
func idemKey(backupID, repositoryID string) string {
	return "replicate:" + backupID + ":" + repositoryID
}

// Enqueue queues one completed, locally present backup. Enqueue is idempotent:
// the idem_key makes a repeated call a no-op rather than a second copy.
func (r *Replicator) Enqueue(ctx context.Context, backupID string) error {
	if backupID == "" || r.RepositoryID == "" {
		return errors.New("replication: backup and repository are required")
	}
	backup, err := r.Store.Backup(ctx, backupID)
	if err != nil {
		return err
	}
	if err = acceptable(backup); err != nil {
		return err
	}
	now := stamp(r.clock())
	return replsql.New(r.DB).InsertReplica(ctx, replsql.InsertReplicaParams{
		BackupID: backupID, RepositoryID: r.RepositoryID,
		IdemKey: idemKey(backupID, r.RepositoryID), NextAttemptAt: now,
		CreatedAt: now, UpdatedAt: now,
	})
}

// acceptable requires the states a copy may read: completed and locally present.
func acceptable(backup physicalstore.Backup) error {
	switch {
	case backup.Status != "completed":
		return errors.New("replication: backup is not completed")
	case !backup.LocalPresent:
		return errors.New("replication: backup payload is not present locally")
	}
	return nil
}

// EnqueueCompleted queues every completed, locally present backup of one source
// in creation order. Local backups are never held back by replication.
func (r *Replicator) EnqueueCompleted(ctx context.Context, sourceID string) error {
	if sourceID == "" {
		return errors.New("replication: source is required")
	}
	if r.RepositoryID == "" {
		return errors.New("replication: repository is required")
	}
	rows, err := replsql.New(r.DB).CompletedBackupsBySource(ctx, sourceID)
	if err != nil {
		return err
	}
	now := stamp(r.clock())
	q := replsql.New(r.DB)
	for _, row := range rows {
		if err = q.InsertReplica(ctx, replsql.InsertReplicaParams{
			BackupID: row.ID, RepositoryID: r.RepositoryID,
			IdemKey: idemKey(row.ID, r.RepositoryID), NextAttemptAt: now,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			return err
		}
	}
	return nil
}

// queueItem is one replica row with the fields a step decision needs.
type queueItem struct {
	backupID, sourceID, status string
	attempts, permanent        int64
	nextAttemptAt              time.Time
	failedSince                *time.Time
}

// queue reads this repository's replicas in FIFO order per source.
func (r *Replicator) queue(ctx context.Context) ([]queueItem, error) {
	rows, err := replsql.New(r.DB).ReplicasForRepository(ctx, r.RepositoryID)
	if err != nil {
		return nil, err
	}
	items := make([]queueItem, 0, len(rows))
	for _, row := range rows {
		item, err := queueRecord(row)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func queueRecord(row replsql.ReplicasForRepositoryRow) (queueItem, error) {
	next, err := requiredStamp(row.NextAttemptAt)
	if err != nil {
		return queueItem{}, err
	}
	failed, err := parseStamp(row.FailedSince)
	if err != nil {
		return queueItem{}, err
	}
	return queueItem{
		backupID: row.BackupID, sourceID: row.SourceID, status: row.Status,
		attempts: row.Attempts, permanent: row.Permanent, nextAttemptAt: next, failedSince: failed,
	}, nil
}

// nextEligible returns the first due item whose every predecessor for its source
// is verified. A failed or exhausted predecessor blocks later items, so the
// verified prefix never skips a gap.
func (r *Replicator) nextEligible(ctx context.Context, now time.Time) (*queueItem, error) {
	items, err := r.queue(ctx)
	if err != nil {
		return nil, err
	}
	blocked := make(map[string]bool, len(items))
	for i := range items {
		item := &items[i]
		if blocked[item.sourceID] {
			continue
		}
		if item.status == StatusVerified {
			continue
		}
		// The first non-verified item of a source is its only eligible candidate.
		blocked[item.sourceID] = true
		if item.permanent != 0 || item.status == StatusExhausted || item.nextAttemptAt.After(now) {
			continue
		}
		return item, nil
	}
	return nil, nil
}

// Step processes at most one due, FIFO-eligible item end-to-end: upload then
// verification readback. It reports whether an item was attempted, and that
// attempt's failure. Callers serialize Step; local backups are unaffected.
func (r *Replicator) Step(ctx context.Context) (bool, error) {
	if err := r.check(); err != nil {
		return false, err
	}
	item, err := r.nextEligible(ctx, r.clock())
	if err != nil || item == nil {
		return false, err
	}
	return true, r.process(ctx, item)
}

// process runs one attempt for one item and records its outcome.
func (r *Replicator) process(ctx context.Context, item *queueItem) error {
	backup, err := r.Store.Backup(ctx, item.backupID)
	if err != nil {
		return r.fail(ctx, item, terminal(CodeStateInvalid, err))
	}
	if err = acceptable(backup); err != nil {
		return r.fail(ctx, item, terminal(CodeLocalMissing, err))
	}
	if err = r.begin(ctx, item); err != nil {
		return err
	}
	members, confirmed, err := r.copy(ctx, item, backup)
	if err != nil {
		return err
	}
	// Every member is committed here; only the readback below can prove the copy.
	if err = r.hook.call("before_verify"); err != nil {
		return r.verifyFailure(ctx, item, confirmed, classify(err))
	}
	if err = r.verify(ctx, backup, members); err != nil {
		return r.verifyFailure(ctx, item, confirmed, err)
	}
	if err = r.recordVerified(ctx, backup.ID); err != nil {
		return errors.Join(err, r.fail(ctx, item, retryable(CodeInternal, err)))
	}
	return nil
}

// copy uploads the members still needed and reports whether every commit is
// confirmed in this attempt.
func (r *Replicator) copy(ctx context.Context, item *queueItem, backup physicalstore.Backup) (map[string]memberRecord, bool, error) {
	members, err := r.loadMembers(ctx, backup.ID)
	if err != nil {
		return nil, false, r.fail(ctx, item, retryable(CodeInternal, err))
	}
	if err = r.prepareMembers(ctx, backup, members); err != nil {
		return nil, false, r.fail(ctx, item, err)
	}
	confirmed, err := r.transfer(ctx, backup, members)
	if err != nil {
		return nil, false, r.fail(ctx, item, err)
	}
	if !confirmed {
		return members, false, nil
	}
	if err = r.markUploaded(ctx, backup.ID, members); err != nil {
		return nil, false, errors.Join(err, r.fail(ctx, item, retryable(CodeInternal, err)))
	}
	// The durable status is uploaded here: four commits, no proof yet.
	if err = r.hook.call("uploaded"); err != nil {
		return nil, false, r.fail(ctx, item, classify(err))
	}
	return members, true, nil
}

// begin marks a pending or failed item uploading. An uploaded item keeps its
// status because its bytes are already committed and only verification remains.
func (r *Replicator) begin(ctx context.Context, item *queueItem) error {
	if item.status == StatusUploaded {
		return nil
	}
	changed, err := replsql.New(r.DB).SetReplicaUploading(ctx, replsql.SetReplicaUploadingParams{
		UpdatedAt: stamp(r.clock()), BackupID: item.backupID, RepositoryID: r.RepositoryID,
	})
	if err != nil {
		return err
	}
	if changed != 1 {
		return errors.New("replication: replica is not claimable")
	}
	return nil
}

// fail records one failed attempt: backoff, exhaustion, and the alert state.
func (r *Replicator) fail(ctx context.Context, item *queueItem, cause error) error {
	failure := asReplicaFailure(cause)
	policy := r.effectivePolicy()
	attempts := min(item.attempts+1, int64(policy.MaxAttempts))
	status := StatusFailed
	permanent := int64(0)
	switch {
	case !failure.retryable:
		permanent = 1
	case attempts >= int64(policy.MaxAttempts):
		status = StatusExhausted
	}
	if err := r.writeFailure(ctx, item, failure, status, attempts, permanent); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// verifyFailure records a failed readback. With every commit confirmed the item
// stays uploaded, because only the proof is missing; an unconfirmed member cannot
// claim uploaded. A non-retryable failure (a conflicting final key) is failed, and
// the last allowed attempt is exhausted.
func (r *Replicator) verifyFailure(ctx context.Context, item *queueItem, confirmed bool, cause error) error {
	failure := asReplicaFailure(cause)
	policy := r.effectivePolicy()
	attempts := min(item.attempts+1, int64(policy.MaxAttempts))
	status := StatusUploaded
	permanent := int64(0)
	switch {
	case !failure.retryable:
		status, permanent = StatusFailed, 1
	case attempts >= int64(policy.MaxAttempts):
		status = StatusExhausted
	case !confirmed:
		status = StatusFailed
	}
	if err := r.writeFailure(ctx, item, failure, status, attempts, permanent); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// writeFailure persists one failure and decides the alert.
func (r *Replicator) writeFailure(ctx context.Context, item *queueItem, failure *replicaFailure, status string, attempts, permanent int64) error {
	policy := r.effectivePolicy()
	now := r.clock()
	next := now.Add(backoff(policy, attempts))
	if permanent != 0 {
		next = now
	}
	q := replsql.New(r.DB)
	if err := q.SetReplicaFailed(ctx, replsql.SetReplicaFailedParams{
		Status: status, Attempts: attempts, Permanent: permanent, NextAttemptAt: stamp(next),
		FailedSince:   nullableStamp(now),
		LastErrorCode: sql.NullString{String: failure.code, Valid: true},
		LastReplyCode: replyCodeOf(failure),
		UpdatedAt:     stamp(now), BackupID: item.backupID, RepositoryID: r.RepositoryID,
	}); err != nil {
		return err
	}
	if !r.alerts(item, failure, status, now) {
		return nil
	}
	return q.SetReplicaAlert(ctx, replsql.SetReplicaAlertParams{
		AlertSince: nullableStamp(now), BackupID: item.backupID, RepositoryID: r.RepositoryID,
	})
}

// alerts reports whether this failure raises the alert now. Exhaustion and
// non-retryable failures alert immediately; otherwise the alert waits until the
// first unresolved failure is older than AlertAfter.
func (r *Replicator) alerts(item *queueItem, failure *replicaFailure, status string, now time.Time) bool {
	if !failure.retryable || status == StatusExhausted {
		return true
	}
	if item.failedSince == nil {
		return false
	}
	return now.Sub(*item.failedSince) >= r.effectivePolicy().AlertAfter
}

// backoff returns min(BaseDelay·2^(attempts-1), MaxDelay).
func backoff(policy Policy, attempts int64) time.Duration {
	delay := policy.BaseDelay
	for i := int64(1); i < max(attempts, 1); i++ {
		if delay >= policy.MaxDelay/2 {
			return policy.MaxDelay
		}
		delay *= 2
	}
	return min(delay, policy.MaxDelay)
}

// replyCodeOf extracts the safe numeric reply code from a failure cause.
func replyCodeOf(failure *replicaFailure) sql.NullInt64 {
	code := replyCode(failure.cause)
	if code == 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(code), Valid: true}
}

// Retry resets one replica for an immediate manual attempt and keeps its history
// counters (upload time, verification time, attempt count, last error code).
func (r *Replicator) Retry(ctx context.Context, backupID string) error {
	now := stamp(r.clock())
	return replsql.New(r.DB).ResetReplicaAttempt(ctx, replsql.ResetReplicaAttemptParams{
		NextAttemptAt: now, UpdatedAt: now, BackupID: backupID, RepositoryID: r.RepositoryID,
	})
}

// Recover repairs state at startup: an interrupted upload returns to pending,
// every recorded temporary key is retried, and every pending local-delete
// reservation for this instance is finished. A committed local-delete
// reservation stays authorized after a later pin. A failed cleanup keeps its
// row and records the error code.
func (r *Replicator) Recover(ctx context.Context) error {
	if r.DB == nil || r.Store == nil || r.Local == nil || r.Remote == nil {
		return errors.New("replication: database, store, local repository and remote are required")
	}
	if r.RepositoryID == "" || r.InstanceID == "" {
		return errors.New("replication: repository and instance are required")
	}
	now := stamp(r.clock())
	if err := replsql.New(r.DB).ResetUploadingReplicas(ctx, replsql.ResetUploadingReplicasParams{
		NextAttemptAt: now, UpdatedAt: now, RepositoryID: r.RepositoryID,
	}); err != nil {
		return err
	}
	return errors.Join(r.recoverParts(ctx), r.recoverLocalDeletes(ctx))
}

// recoverParts retries CleanupPart for every recorded temporary key. A failed
// cleanup keeps the row.
func (r *Replicator) recoverParts(ctx context.Context) error {
	parts, err := replsql.New(r.DB).Parts(ctx, r.RepositoryID)
	if err != nil {
		return err
	}
	var result error
	for _, part := range parts {
		if err = r.Remote.CleanupPart(ctx, part.Part); err != nil {
			result = errors.Join(result, r.keepPart(ctx, part.ID, err))
			continue
		}
		if err = replsql.New(r.DB).DeletePart(ctx, part.ID); err != nil {
			result = errors.Join(result, err)
		}
	}
	return result
}

// keepPart records a still-pending cleanup so a later Recover retries it.
func (r *Replicator) keepPart(ctx context.Context, partID int64, cause error) error {
	var cleanup *ftp.CleanupError
	code := CodeRemoteTransport
	if errors.As(cause, &cleanup) {
		code = CodeCleanupPending
	}
	err := replsql.New(r.DB).PartFailed(ctx, replsql.PartFailedParams{
		LastErrorCode: sql.NullString{String: code, Valid: true}, ID: partID,
	})
	return errors.Join(cause, err)
}

// Replica reads one replica row. A missing row reports sql.ErrNoRows.
func (r *Replicator) Replica(ctx context.Context, backupID string) (Replica, error) {
	row, err := replsql.New(r.DB).GetReplica(ctx, replsql.GetReplicaParams{BackupID: backupID, RepositoryID: r.RepositoryID})
	if err != nil {
		return Replica{}, err
	}
	return replicaRecord(row)
}

// List reads every replica of one source in FIFO order.
func (r *Replicator) List(ctx context.Context, sourceID string) ([]Replica, error) {
	rows, err := replsql.New(r.DB).ReplicasBySource(ctx, replsql.ReplicasBySourceParams{
		RepositoryID: r.RepositoryID, SourceID: sourceID,
	})
	if err != nil {
		return nil, err
	}
	replicas := make([]Replica, 0, len(rows))
	for _, row := range rows {
		replica, err := replicaRecord(replsql.Replica{
			BackupID: row.BackupID, RepositoryID: row.RepositoryID, IdemKey: row.IdemKey,
			Status: row.Status, Attempts: row.Attempts, Permanent: row.Permanent,
			NextAttemptAt: row.NextAttemptAt, UploadedAt: row.UploadedAt, VerifiedAt: row.VerifiedAt,
			FailedSince: row.FailedSince, AlertSince: row.AlertSince, LastErrorCode: row.LastErrorCode,
			LastReplyCode: row.LastReplyCode, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		})
		if err != nil {
			return nil, err
		}
		replicas = append(replicas, replica)
	}
	return replicas, nil
}

// ReplicaStatuses returns the replica status of this repository for each listed
// backup. A backup without a replica row is absent from the map.
func (r *Replicator) ReplicaStatuses(ctx context.Context, backupIDs []string) (map[string]string, error) {
	statuses := make(map[string]string, len(backupIDs))
	if len(backupIDs) == 0 {
		return statuses, nil
	}
	rows, err := replsql.New(r.DB).ReplicasByBackupIDs(ctx, replsql.ReplicasByBackupIDsParams{
		RepositoryID: r.RepositoryID, BackupIds: backupIDs,
	})
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		statuses[row.BackupID] = row.Status
	}
	return statuses, nil
}

func replicaRecord(row replsql.Replica) (Replica, error) {
	next, err := requiredStamp(row.NextAttemptAt)
	if err != nil {
		return Replica{}, err
	}
	replica := Replica{
		BackupID: row.BackupID, RepositoryID: row.RepositoryID, Status: row.Status,
		Attempts: int(row.Attempts), NextAttemptAt: &next,
		LastErrorCode: row.LastErrorCode.String, LastReplyCode: int(row.LastReplyCode.Int64),
	}
	for _, field := range []struct {
		value sql.NullString
		dest  **time.Time
	}{
		{row.UploadedAt, &replica.UploadedAt},
		{row.VerifiedAt, &replica.VerifiedAt},
		{row.AlertSince, &replica.AlertSince},
	} {
		parsed, parseErr := parseStamp(field.value)
		if parseErr != nil {
			return Replica{}, parseErr
		}
		*field.dest = parsed
	}
	return replica, nil
}

// Point is one protection point: a backup, its chain, and when it was proven.
type Point struct {
	BackupID, ChainID string
	At                time.Time
}

// LocalPoint is the latest locally completed backup whose chain prefix is all
// completed. W4 compares it with the remote point to derive RPO.
func LocalPoint(ctx context.Context, s *physicalstore.Store, sourceID string) (*Point, error) {
	chains, err := s.SourceChains(ctx, sourceID)
	if err != nil {
		return nil, err
	}
	for i := len(chains) - 1; i >= 0; i-- {
		members, err := s.ChainBackups(ctx, chains[i].ID)
		if err != nil {
			return nil, err
		}
		point, err := completedPrefix(chains[i].ID, members)
		if err != nil {
			return nil, err
		}
		if point != nil {
			return point, nil
		}
	}
	return nil, nil
}

// completedPrefix returns the last completed member of the all-completed prefix.
func completedPrefix(chainID string, members []physicalstore.Backup) (*Point, error) {
	var point *Point
	for _, member := range members {
		if member.Status != "completed" || member.CompletedAt.IsZero() {
			return point, nil
		}
		point = &Point{BackupID: member.ID, ChainID: chainID, At: member.CompletedAt}
	}
	return point, nil
}

// RemotePoint is the last item of the longest all-verified FIFO prefix for this
// (source, repository) pair.
func (r *Replicator) RemotePoint(ctx context.Context, sourceID string) (*Point, error) {
	rows, err := replsql.New(r.DB).ReplicasBySource(ctx, replsql.ReplicasBySourceParams{
		RepositoryID: r.RepositoryID, SourceID: sourceID,
	})
	if err != nil {
		return nil, err
	}
	var point *Point
	for _, row := range rows {
		if row.Status != StatusVerified {
			break
		}
		verified, err := parseStamp(row.VerifiedAt)
		if err != nil {
			return nil, err
		}
		if verified == nil {
			return nil, errors.New("replication: verified replica has no verification time")
		}
		point = &Point{BackupID: row.BackupID, ChainID: row.ChainID, At: *verified}
	}
	return point, nil
}
