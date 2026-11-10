package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/flyingEirc/Intern/app/web/internal/store/internal/storesql"
)

// Run kinds, triggers and statuses. The status path has no finalizing state:
// the physical chain's success commit is an interior step of its Run call and
// is not observable from the control plane.
const (
	RunKindFull        = "full"
	RunKindIncremental = "incremental"

	RunTriggerCron      = "cron"
	RunTriggerManual    = "manual"
	RunTriggerAdmission = "admission"

	RunStatusQueued    = "queued"
	RunStatusRunning   = "running"
	RunStatusCompleted = "completed"
	RunStatusFailed    = "failed"
	RunStatusCanceled  = "canceled"

	// Run error classes are a closed set. unavailable is the fixed outcome for
	// runs created while the deployment lacks the KEK, work root or XtraBackup
	// executable; preflight covers a failed fresh preflight with its code;
	// requires_full marks an incremental refused by admission; interrupted
	// marks a run that a restart found still running; canceled marks a run
	// stopped by a cancel request; backup_failed is a physical chain failure.
	RunErrorUnavailable    = "unavailable"
	RunErrorPreflight      = "preflight"
	RunErrorRequiresFull   = "requires_full"
	RunErrorCanceled       = "canceled"
	RunErrorInterrupted    = "interrupted"
	RunErrorBackupFailed   = "backup_failed"
	RunErrorRecoveryFailed = "recovery_failed"
)

// Run is one backup attempt of one policy. LocalRepositoryID and
// ReplicaRepositoryID are the pair the claim snapshotted from the policy, so
// replication resolves the backup's repositories from the run itself and a
// later policy edit never moves a backup that already exists. They are internal
// replication state and stay off the wire, like the idempotency key.
type Run struct {
	ID              string  `json:"id"`
	PolicyID        string  `json:"policy_id"`
	SourceID        string  `json:"source_id"`
	Kind            string  `json:"kind"`
	Trigger         string  `json:"trigger"`
	Status          string  `json:"status"`
	ScheduledAt     string  `json:"scheduled_at"`
	CreatedAt       string  `json:"created_at"`
	StartedAt       *string `json:"started_at"`
	FinishedAt      *string `json:"finished_at"`
	BackupID        *string `json:"backup_id"`
	ChainID         *string `json:"chain_id"`
	PayloadSize     *int64  `json:"payload_size"`
	ErrorClass      *string `json:"error_class"`
	ErrorCode       *string `json:"error_code"`
	CancelRequested bool    `json:"cancel_requested"`
	ActorID         *string `json:"actor_id"`
	// IdempotencyKey, RequestHash and the claimed (local, replica) pair are
	// internal claim state and are deliberately not part of the wire shape.
	IdempotencyKey      string  `json:"-"`
	RequestHash         string  `json:"-"`
	LocalRepositoryID   *string `json:"-"`
	ReplicaRepositoryID *string `json:"-"`
}

// RunLog is one bounded event of one run.
type RunLog struct {
	RunID   string `json:"run_id"`
	Seq     int64  `json:"seq"`
	TS      string `json:"ts"`
	Level   string `json:"level"`
	Event   string `json:"event"`
	Message string `json:"message"`
	Data    any    `json:"data"`
}

// RunLogWrite is one event to persist. Data is marshaled as JSON; nil stores NULL.
type RunLogWrite struct {
	Level, Event, Message string
	Data                  any
}

// MaxRunLogRows is the hard cap of run_logs rows per run. Further events are
// dropped after one final truncated marker.
const MaxRunLogRows = 1000

// TruncatedEvent is the event name of the final marker row of a full log.
const TruncatedEvent = "truncated"

func runFromRow(row storesql.Run) Run {
	run := Run{
		ID: row.ID, PolicyID: row.PolicyID, SourceID: row.SourceID, Kind: row.Kind,
		Trigger: row.Trigger, Status: row.Status, ScheduledAt: row.ScheduledAt,
		CreatedAt: row.CreatedAt, CancelRequested: row.CancelRequested != 0,
		IdempotencyKey: row.IdempotencyKey, RequestHash: row.RequestHash,
	}
	nullableString(&run.StartedAt, row.StartedAt)
	nullableString(&run.FinishedAt, row.FinishedAt)
	nullableString(&run.BackupID, row.BackupID)
	nullableString(&run.ChainID, row.ChainID)
	if row.PayloadSize.Valid {
		size := row.PayloadSize.Int64
		run.PayloadSize = &size
	}
	nullableString(&run.ErrorClass, row.ErrorClass)
	nullableString(&run.ErrorCode, row.ErrorCode)
	nullableString(&run.ActorID, row.ActorID)
	nullableString(&run.LocalRepositoryID, row.LocalRepositoryID)
	nullableString(&run.ReplicaRepositoryID, row.ReplicaRepositoryID)
	return run
}

func nullableString(dest **string, value sql.NullString) {
	if value.Valid {
		text := value.String
		*dest = &text
	}
}

func nullString(value *string) sql.NullString {
	if value == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *value, Valid: true}
}

func runLogFromRow(row storesql.RunLog) RunLog {
	return RunLog{
		RunID: row.RunID, Seq: row.Seq, TS: row.Ts, Level: row.Level, Event: row.Event,
		Message: row.Message, Data: decodeRunLogData(row.Data),
	}
}

func decodeRunLogData(value sql.NullString) any {
	if !value.Valid || value.String == "" {
		return nil
	}
	var data any
	if err := json.Unmarshal([]byte(value.String), &data); err != nil {
		return nil
	}
	return data
}

func encodeRunLogData(data any) sql.NullString {
	if data == nil {
		return sql.NullString{}
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return sql.NullString{}
	}
	return sql.NullString{String: string(encoded), Valid: true}
}

// DB exposes the single control-plane connection so the dispatcher, the
// physical store and the replicators share it. The database stays owned by the
// Store: callers must not close it or open a second connection to the same
// file, because the write lock is per database, not per handle.
func (s *Store) DB() *sql.DB { return s.db }

// EnabledPolicies returns every enabled policy, oldest id first.
func (s *Store) EnabledPolicies(ctx context.Context) ([]Policy, error) {
	rows, err := storesql.New(s.db).EnabledPolicies(ctx)
	if err != nil {
		return nil, err
	}
	policies := make([]Policy, 0, len(rows))
	for _, row := range rows {
		policy, err := policyFromRow(row)
		if err != nil {
			return nil, err
		}
		policies = append(policies, policy)
	}
	return policies, nil
}

// stampTime is the fixed-width UTC text the schedule columns use, so lexical
// order is chronological order like every other timestamp in this database.
func stampTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000000000Z")
}

func nullableTime(value time.Time) sql.NullString {
	if value.IsZero() {
		return sql.NullString{}
	}
	return sql.NullString{String: stampTime(value), Valid: true}
}

// InitializeSchedule fills NULL next_full_at and next_incremental_at on one
// enabled policy. A failed initialization keeps the NULL columns, so the
// scheduler retries on the next tick instead of skipping it silently.
func (s *Store) InitializeSchedule(ctx context.Context, policyID string, nextFull, nextIncremental time.Time) error {
	_, err := storesql.New(s.db).SetPolicySchedule(ctx, storesql.SetPolicyScheduleParams{
		NextFullAt: nullableTime(nextFull), NextIncrementalAt: nullableTime(nextIncremental),
		UpdatedAt: timestamp(), ID: policyID,
	})
	return err
}

// ScheduleStamp renders one stored schedule timestamp.
func ScheduleStamp(value time.Time) string { return stampTime(value) }

// PolicySchedule reads the stored next trigger times. A missing policy reports
// ErrNotFound. A NULL column reports a zero time.
func (s *Store) PolicySchedule(ctx context.Context, policyID string) (nextFull, nextIncremental *time.Time, err error) {
	row, err := storesql.New(s.db).GetPolicySchedule(ctx, policyID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, ErrNotFound
		}
		return nil, nil, err
	}
	if nextFull, err = parseScheduleTime(row.NextFullAt); err != nil {
		return nil, nil, err
	}
	nextIncremental, err = parseScheduleTime(row.NextIncrementalAt)
	return nextFull, nextIncremental, err
}

func parseScheduleTime(value sql.NullString) (*time.Time, error) {
	if !value.Valid {
		return nil, nil
	}
	parsed, err := time.Parse("2006-01-02T15:04:05.000000000Z", value.String)
	if err != nil {
		return nil, fmt.Errorf("stored schedule %q: %w", value.String, err)
	}
	return &parsed, nil
}

// RunInput is the complete description of a new run.
type RunInput struct {
	PolicyID, SourceID, Kind, Trigger string
	IdempotencyKey, RequestHash       string
	ScheduledAt                       time.Time
	ActorID                           *string
}

// CreateRun inserts a queued run. The caller supplies the unique idempotency
// key; a duplicate reports ErrConflict and leaves the stored row untouched.
func (s *Store) CreateRun(ctx context.Context, in RunInput) (Run, error) {
	id, err := newID()
	if err != nil {
		return Run{}, err
	}
	row, err := storesql.New(s.db).InsertRun(ctx, storesql.InsertRunParams{
		ID: id, PolicyID: in.PolicyID, SourceID: in.SourceID, Kind: in.Kind, Trigger: in.Trigger,
		IdempotencyKey: in.IdempotencyKey, RequestHash: in.RequestHash,
		ScheduledAt: stampTime(in.ScheduledAt), CreatedAt: timestamp(), ActorID: nullString(in.ActorID),
	})
	if err != nil {
		return Run{}, resourceError(err)
	}
	return runFromRow(row), nil
}

// ClaimRun marks the next queued run running. Rows with a cancel request are
// skipped: the API canceled them before execution. It returns false when no
// claimable run exists. Global backup concurrency is 1: a claim is refused
// while any run is already running, which is also per-source exclusion.
//
// The claim snapshots the policy's (local, replica) pair onto the row in the
// same statement, so the repositories the run actually uses are the ones
// replication later resolves for its backup.
func (s *Store) ClaimRun(ctx context.Context) (Run, bool, error) {
	row, err := storesql.New(s.db).ClaimNextRun(ctx, nullableTime(time.Now()))
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, false, nil
	}
	if err != nil {
		return Run{}, false, err
	}
	return runFromRow(row), true, nil
}

// RunByIdempotencyKey reads the run an idempotency key already created.
func (s *Store) RunByIdempotencyKey(ctx context.Context, key string) (Run, error) {
	row, err := storesql.New(s.db).GetRunByIdempotencyKey(ctx, key)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Run{}, ErrNotFound
		}
		return Run{}, err
	}
	return runFromRow(row), nil
}

// Run reads one run. A missing run reports ErrNotFound.
func (s *Store) Run(ctx context.Context, id string) (Run, error) {
	row, err := storesql.New(s.db).GetRun(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Run{}, ErrNotFound
		}
		return Run{}, err
	}
	return runFromRow(row), nil
}

// RunsFilter bounds a run list.
type RunsFilter struct {
	PolicyID, SourceID, Status string
	Before                     string
	Limit                      int
}

// Runs returns the newest matching runs in descending UUIDv7 order.
func (s *Store) Runs(ctx context.Context, filter RunsFilter) ([]Run, error) {
	rows, err := storesql.New(s.db).ListRuns(ctx, storesql.ListRunsParams{
		Before:   filter.Before,
		PolicyID: filter.PolicyID, SourceID: filter.SourceID, Status: filter.Status,
		RowLimit: int64(filter.Limit),
	})
	if err != nil {
		return nil, err
	}
	runs := make([]Run, 0, len(rows))
	for _, row := range rows {
		runs = append(runs, runFromRow(row))
	}
	return runs, nil
}

// RequestCancel marks a queued or running run for cancellation. A terminal run
// reports ErrConflict so the caller can answer 409.
func (s *Store) RequestCancel(ctx context.Context, id string, entry AuditLog) error {
	return s.withTx(ctx, func(ctx context.Context, q *storesql.Queries) error {
		if _, err := q.GetRun(ctx, id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		changed, err := q.RequestCancel(ctx, id)
		if err != nil {
			return err
		}
		if changed != 1 {
			return ErrConflict
		}
		return insertSuccessAudit(ctx, q, entry, "run", id)
	})
}

// CancelQueuedRun cancels a queued run before a worker claims it. changed is
// false when a claim won the race.
func (s *Store) CancelQueuedRun(ctx context.Context, id string, entry AuditLog, log *RunLogWrite) (changed bool, err error) {
	err = s.withTx(ctx, func(ctx context.Context, q *storesql.Queries) error {
		affected, writeErr := q.CancelQueuedRun(ctx, storesql.CancelQueuedRunParams{
			ID: id, FinishedAt: nullableTime(time.Now()),
		})
		if writeErr != nil {
			return writeErr
		}
		changed = affected == 1
		if !changed {
			return nil
		}
		if auditErr := insertSuccessAudit(ctx, q, entry, "run", id); auditErr != nil {
			return auditErr
		}
		return appendRunLogTx(ctx, q, id, log)
	})
	return changed, err
}

// FinishRunCompleted records a completed run with its published artifact.
func (s *Store) FinishRunCompleted(ctx context.Context, id string, backupID, chainID string, payloadSize int64, entry AuditLog, log *RunLogWrite) error {
	return s.withTx(ctx, func(ctx context.Context, q *storesql.Queries) error {
		changed, err := q.CompleteRun(ctx, storesql.CompleteRunParams{
			ID: id, FinishedAt: nullableTime(time.Now()), BackupID: nullable(backupID),
			ChainID: nullable(chainID), PayloadSize: sql.NullInt64{Int64: payloadSize, Valid: true},
		})
		if err != nil {
			return err
		}
		if changed != 1 {
			return fmt.Errorf("run %s is not running", id)
		}
		if err := insertSuccessAudit(ctx, q, entry, "run", id); err != nil {
			return err
		}
		return appendRunLogTx(ctx, q, id, log)
	})
}

// FinishRunFailed records a failed run. class is one of the RunError* values
// and code the stable failure code when one exists.
func (s *Store) FinishRunFailed(ctx context.Context, id, class, code string, entry AuditLog, log *RunLogWrite) error {
	return s.withTx(ctx, func(ctx context.Context, q *storesql.Queries) error {
		changed, err := q.FailRun(ctx, storesql.FailRunParams{
			ID: id, FinishedAt: nullableTime(time.Now()), ErrorClass: nullable(class), ErrorCode: nullString(&code),
		})
		if err != nil {
			return err
		}
		if changed != 1 {
			return fmt.Errorf("run %s is not running", id)
		}
		if err := insertSuccessAudit(ctx, q, entry, "run", id); err != nil {
			return err
		}
		return appendRunLogTx(ctx, q, id, log)
	})
}

// FinishRunCanceled records a canceled run. It refuses a completed run: a
// success point never becomes a cancellation.
func (s *Store) FinishRunCanceled(ctx context.Context, id string, entry AuditLog, log *RunLogWrite) error {
	return s.withTx(ctx, func(ctx context.Context, q *storesql.Queries) error {
		changed, err := q.CancelRunningRun(ctx, storesql.CancelRunningRunParams{
			ID: id, FinishedAt: nullableTime(time.Now()),
		})
		if err != nil {
			return err
		}
		if changed != 1 {
			return ErrConflict
		}
		if err := insertSuccessAudit(ctx, q, entry, "run", id); err != nil {
			return err
		}
		return appendRunLogTx(ctx, q, id, log)
	})
}

// InterruptedRuns lists every running run, for startup recovery.
func (s *Store) InterruptedRuns(ctx context.Context) ([]Run, error) {
	rows, err := storesql.New(s.db).InterruptedRuns(ctx)
	if err != nil {
		return nil, err
	}
	runs := make([]Run, 0, len(rows))
	for _, row := range rows {
		runs = append(runs, runFromRow(row))
	}
	return runs, nil
}

// AppendRunLog appends one fixed-format event, enforcing the per-run cap. When
// the log is full the event is dropped; a final truncated marker replaces the
// newest row exactly once. The cap check and the insert share one transaction
// on the single write connection.
func (s *Store) AppendRunLog(ctx context.Context, runID, level, event, message string, data any) error {
	return s.withTx(ctx, func(ctx context.Context, q *storesql.Queries) error {
		return appendRunLogTx(ctx, q, runID, &RunLogWrite{Level: level, Event: event, Message: message, Data: data})
	})
}

func appendRunLogTx(ctx context.Context, q *storesql.Queries, runID string, log *RunLogWrite) error {
	if log == nil {
		return nil
	}
	count, err := q.CountRunLogs(ctx, runID)
	if err != nil {
		return err
	}
	if count >= MaxRunLogRows {
		return truncateRunLog(ctx, q, runID, log.Event)
	}
	return q.InsertRunLog(ctx, storesql.InsertRunLogParams{
		RunID: runID, Seq: count + 1, Ts: timestamp(), Level: log.Level, Event: log.Event,
		Message: log.Message, Data: encodeRunLogData(log.Data),
	})
}

// truncateRunLog rewrites the newest row once with the truncated marker, so the
// log always ends in the marker and stays inside the cap. Further appends are
// dropped without rewriting it again.
func truncateRunLog(ctx context.Context, q *storesql.Queries, runID, event string) error {
	if event == TruncatedEvent {
		return nil
	}
	last, err := q.GetRunLog(ctx, storesql.GetRunLogParams{RunID: runID, Seq: MaxRunLogRows})
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil || last.Event == TruncatedEvent {
		return err
	}
	_, err = q.ReplaceRunLog(ctx, storesql.ReplaceRunLogParams{
		RunID: runID, Seq: MaxRunLogRows, Ts: timestamp(), Level: "warn", Event: TruncatedEvent,
		Message: "run log reached its cap; later events were dropped", Data: sql.NullString{},
	})
	return err
}

// RunByBackupID reads the newest run that published this backup. A missing row
// reports ErrNotFound.
func (s *Store) RunByBackupID(ctx context.Context, backupID string) (Run, error) {
	row, err := storesql.New(s.db).RunByBackupID(ctx, nullable(backupID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Run{}, ErrNotFound
		}
		return Run{}, err
	}
	return runFromRow(row), nil
}

// HasActiveFull reports whether a queued or running full already exists for the
// source, so an admission full can coalesce instead of inserting another.
func (s *Store) HasActiveFull(ctx context.Context, sourceID string) (bool, error) {
	count, err := storesql.New(s.db).CountActiveFullsForSource(ctx, sourceID)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// PairBackups lists the backups published by runs claimed with one
// (local repository, replica repository) pair. The run's snapshot is the
// binding, so a backup produced on another local repository is never listed and
// a policy edit never moves an existing backup to a pair it never ran on.
func (s *Store) PairBackups(ctx context.Context, localRepositoryID, replicaRepositoryID string) ([]string, error) {
	rows, err := storesql.New(s.db).PairBackups(ctx, storesql.PairBackupsParams{
		LocalRepositoryID: nullString(&localRepositoryID), ReplicaRepositoryID: nullable(replicaRepositoryID),
	})
	if err != nil {
		return nil, err
	}
	backups := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.Valid {
			backups = append(backups, row.String)
		}
	}
	return backups, nil
}

// ChainPredecessorsForPair lists the chain members before one backup that one
// (local, replica) pair may still enqueue, in seq order: produced by a run on
// the pair's own local repository, completed and locally present, and not yet
// queued on the pair's repository. The producing run is resolved through the
// backup it published — the same runs.backup_id binding RunByBackupID reads —
// and that run's snapshot is what binds a backup to a local repository, so a
// chain that crossed local repositories never queues the other repository's
// base here.
//
// The statement reads the physical backups table and the replication replicas
// table, which sqlc cannot model from the Web schema alone, so it is a raw
// query like the reference counts DeleteRepository runs through withTxDB.
func (s *Store) ChainPredecessorsForPair(ctx context.Context, backupID, localRepositoryID, replicaRepositoryID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT b.id
FROM backups b
JOIN runs producer ON producer.backup_id = b.id
WHERE b.chain_id = (SELECT chain_id FROM backups WHERE id = $1)
  AND b.seq < (SELECT seq FROM backups WHERE id = $1)
  AND b.status = 'completed' AND b.local_present = 1
  AND producer.local_repository_id = $2
  AND NOT EXISTS (
      SELECT 1 FROM replicas r WHERE r.backup_id = b.id AND r.repository_id = $3
  )
ORDER BY b.seq`, backupID, localRepositoryID, replicaRepositoryID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }() //nolint:errcheck // best-effort cleanup; the scan error or rows.Err() carries the failure
	predecessors := make([]string, 0)
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		predecessors = append(predecessors, id)
	}
	return predecessors, rows.Err()
}

// ReplicaLocalRepository returns the local repository a replica row binds one
// backup to on one replica repository: the queue that owns the copy. The run's
// snapshot cannot answer this. A backup is enqueued on a pair after the policy
// moved to it - the predecessor pull queues a base whose own run named no
// replica repository, or another pair's repository - so only the row written at
// enqueue time records which local repository the payload is read from. The
// manual retry resolves its pair through it, which is what lets an operator
// unblock a queue whose predecessor was pulled rather than produced.
//
// The statement reads the replication package's replicas table, which sqlc
// cannot model from the Web schema, so it is a raw query like
// ChainPredecessorsForPair. A backup with no row on the repository reports
// ErrNotFound.
func (s *Store) ReplicaLocalRepository(ctx context.Context, backupID, replicaRepositoryID string) (string, error) {
	var localID string
	err := s.db.QueryRowContext(ctx,
		`SELECT local_repository_id FROM replicas WHERE backup_id = $1 AND repository_id = $2`,
		backupID, replicaRepositoryID).Scan(&localID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if localID == "" {
		// A row from before the local repository was recorded matches no pair and
		// is inert; retrying it could not reach a queue, so report it as absent.
		return "", ErrNotFound
	}
	return localID, nil
}

// ReplicaLocalRepositories lists the local repositories whose queue holds a
// replica row for one backup, in id order. The replica row is the binding of a
// copy: it names the queue that owns it and the local repository the payload is
// read from, so every path that answers for one backup's copies resolves its
// pairs through it - the list page, the backup view, the repository-wide point and
// the manual retry all read the same rows. A run snapshot cannot answer it: a
// chain base queued by the predecessor pull was produced by a run that named no
// replica repository at all.
//
// The statement reads the replication package's replicas table, which sqlc cannot
// model from the Web schema, so it is a raw query like ChainPredecessorsForPair.
// An inert row from before the local repository was recorded names nothing and is
// left out: no pair can read it.
func (s *Store) ReplicaLocalRepositories(ctx context.Context, backupID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT local_repository_id FROM replicas
WHERE backup_id = $1 AND local_repository_id <> '' ORDER BY local_repository_id`, backupID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }() //nolint:errcheck // best-effort cleanup; the scan error or rows.Err() carries the failure
	locals := make([]string, 0)
	for rows.Next() {
		var localID string
		if err = rows.Scan(&localID); err != nil {
			return nil, err
		}
		locals = append(locals, localID)
	}
	return locals, rows.Err()
}

// ReplicationPair is one (local, replica) repository pair the dispatcher serves.
type ReplicationPair struct {
	LocalRepositoryID   string `json:"local_repository_id"`
	ReplicaRepositoryID string `json:"replica_repository_id"`
}

// ReplicationPairs is the installed pair set: every pair a completed run was
// claimed with, plus every pair a current policy names. The run snapshots keep
// old backups replicating after a policy edit; the policy rows make new runs
// replicate on the pair the edit introduced. A pair whose local or replica
// repository row no longer exists is not installed, because it could never be
// opened.
func (s *Store) ReplicationPairs(ctx context.Context) ([]ReplicationPair, error) {
	queries := storesql.New(s.db)
	completed, err := queries.CompletedRunReplicationPairs(ctx)
	if err != nil {
		return nil, err
	}
	policies, err := queries.ListPolicies(ctx, storesql.ListPoliciesParams{Before: "", RowLimit: 1_000_000})
	if err != nil {
		return nil, err
	}
	candidates := make([]ReplicationPair, 0, len(completed)+len(policies))
	for _, row := range completed {
		candidates = append(candidates, ReplicationPair{
			LocalRepositoryID: row.LocalRepositoryID.String, ReplicaRepositoryID: row.ReplicaRepositoryID.String,
		})
	}
	for _, policy := range policies {
		candidates = append(candidates, ReplicationPair{
			LocalRepositoryID: policy.LocalRepositoryID, ReplicaRepositoryID: policy.ReplicaRepositoryID.String,
		})
	}
	return installedPairs(candidates), nil
}

// installedPairs drops the candidates that name no pair and the repeats, then
// orders the rest so the pair set is stable across calls.
func installedPairs(candidates []ReplicationPair) []ReplicationPair {
	seen := make(map[ReplicationPair]struct{}, len(candidates))
	pairs := make([]ReplicationPair, 0, len(candidates))
	for _, pair := range candidates {
		if pair.LocalRepositoryID == "" || pair.ReplicaRepositoryID == "" {
			continue
		}
		if _, ok := seen[pair]; ok {
			continue
		}
		seen[pair] = struct{}{}
		pairs = append(pairs, pair)
	}
	slices.SortFunc(pairs, func(left, right ReplicationPair) int {
		if order := cmp.Compare(left.LocalRepositoryID, right.LocalRepositoryID); order != 0 {
			return order
		}
		return cmp.Compare(left.ReplicaRepositoryID, right.ReplicaRepositoryID)
	})
	return pairs
}

// RunLog reads one log row. A missing row reports ErrNotFound.
func (s *Store) RunLog(ctx context.Context, runID string, seq int64) (RunLog, error) {
	row, err := storesql.New(s.db).GetRunLog(ctx, storesql.GetRunLogParams{RunID: runID, Seq: seq})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RunLog{}, ErrNotFound
		}
		return RunLog{}, err
	}
	return runLogFromRow(row), nil
}

// RunLogsAfter returns at most limit rows with a sequence greater than after,
// in ascending order. after = 0 replays from the first event.
func (s *Store) RunLogsAfter(ctx context.Context, runID string, after, limit int64) ([]RunLog, error) {
	rows, err := storesql.New(s.db).RunLogsAfter(ctx, storesql.RunLogsAfterParams{
		RunID: runID, Seq: after, RowLimit: limit,
	})
	if err != nil {
		return nil, err
	}
	logs := make([]RunLog, 0, len(rows))
	for _, row := range rows {
		logs = append(logs, runLogFromRow(row))
	}
	return logs, nil
}

// MaxRunLogSeq returns the newest sequence of one run's log, 0 when empty.
func (s *Store) MaxRunLogSeq(ctx context.Context, runID string) (int64, error) {
	return storesql.New(s.db).MaxRunLogSeq(ctx, runID)
}

// RunLoggerFor returns a logger whose every record is appended to one run's
// bounded log. Records carry fixed, secret-safe messages only; raw tool
// output never passes through here.
func (s *Store) RunLoggerFor(runID string) *slog.Logger {
	return slog.New(&runLogHandler{store: s, runID: runID})
}

type runLogHandler struct {
	store *Store
	runID string
}

func (h *runLogHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *runLogHandler) Handle(_ context.Context, record slog.Record) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return h.store.AppendRunLog(ctx, h.runID, levelName(record.Level), record.Message, record.Message, nil)
}

func (h *runLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *runLogHandler) WithGroup(string) slog.Handler { return h }

func levelName(level slog.Level) string {
	switch {
	case level >= slog.LevelError:
		return "error"
	case level >= slog.LevelWarn:
		return "warn"
	case level >= slog.LevelInfo:
		return "info"
	default:
		return "debug"
	}
}
