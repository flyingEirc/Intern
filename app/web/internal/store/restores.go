package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"

	"github.com/flyingEirc/Intern/app/web/internal/store/internal/storesql"
)

// Restore statuses and the verify outcome. The status path has no finalizing
// state: the export publish is an interior step of restore.Execute and the row
// is written once at the end.
const (
	RestoreStatusQueued    = "queued"
	RestoreStatusRunning   = "running"
	RestoreStatusCompleted = "completed"
	RestoreStatusFailed    = "failed"
	RestoreStatusCanceled  = "canceled"

	// Restore error classes are a closed set. unavailable covers a restore
	// whose runtime inputs disappeared between queueing and execution (and a
	// rehearsal the deployment cannot run); refused covers a plan that no
	// longer matches the fresh one; restore_failed is an execution failure at
	// a named engine phase; canceled and interrupted are the terminal
	// transitions of a cancel request and of restart recovery.
	RestoreErrorUnavailable = "unavailable"
	RestoreErrorRefused     = "refused"
	RestoreErrorFailed      = "restore_failed"
	RestoreErrorCanceled    = "canceled"
	RestoreErrorInterrupted = "interrupted"

	// Restore verify status is separate from the restore status: a rehearsal
	// that ran and failed does not make a published restore a failure.
	RestoreVerifyNotRequested = "not_requested"
	RestoreVerifyPassed       = "passed"
	RestoreVerifyFailed       = "failed"
)

// RestorePlanLink is one chain member of a restore plan. Source is "local" or
// "ftp"; kind is the physical backup kind. Sizes are the stored payload and
// metadata sizes the plan authenticated against.
type RestorePlanLink struct {
	BackupID     string `json:"backup_id"`
	Seq          int64  `json:"seq"`
	Kind         string `json:"kind"`
	FromLSN      uint64 `json:"from_lsn"`
	ToLSN        uint64 `json:"to_lsn"`
	Source       string `json:"source"`
	RepositoryID string `json:"repository_id"`
	PayloadSize  int64  `json:"payload_size"`
	MetadataSize int64  `json:"metadata_size"`
}

// RestorePlan is the persisted and returned restore plan. Digest is the sha256
// the execute request must confirm; RehearsalAvailable is false when the
// deployment configures no mysqld and a rehearsal would answer 422.
type RestorePlan struct {
	BackupID             string            `json:"backup_id"`
	ChainID              string            `json:"chain_id"`
	SourceID             string            `json:"source_id"`
	ServerUUID           string            `json:"server_uuid"`
	ServerVersion        string            `json:"server_version"`
	ToolVersion          string            `json:"tool_version"`
	TargetToLSN          uint64            `json:"target_to_lsn"`
	Links                []RestorePlanLink `json:"links"`
	RequiredWorkBytes    int64             `json:"required_work_bytes"`
	RequiredExportBytes  int64             `json:"required_export_bytes"`
	AvailableWorkBytes   int64             `json:"available_work_bytes"`
	AvailableExportBytes int64             `json:"available_export_bytes"`
	ExportPath           string            `json:"export_path"`
	Risks                []string          `json:"risks"`
	Digest               string            `json:"plan_digest"`
	RehearsalAvailable   bool              `json:"rehearsal_available"`
}

// RestoreSideEffects is the exact persisted outcome of the two restore-owned
// targets. A published export is never deleted, by recovery or by a cancel.
type RestoreSideEffects struct {
	WorkDirRemoved       bool `json:"work_dir_removed"`
	PendingExportRemoved bool `json:"pending_export_removed"`
	ExportPublished      bool `json:"export_published"`
}

// Restore is one restore row. Plan is the confirmed plan snapshot; the
// idempotency key and request hash are internal request state and stay off the
// wire like the run equivalents.
type Restore struct {
	ID              string              `json:"id"`
	BackupID        string              `json:"backup_id"`
	Status          string              `json:"status"`
	Phase           *string             `json:"phase"`
	Plan            RestorePlan         `json:"plan"`
	PlanDigest      string              `json:"plan_digest"`
	Rehearse        bool                `json:"rehearse"`
	VerifySQL       []string            `json:"verify_sql"`
	RequestedBy     string              `json:"requested_by"`
	WorkDir         *string             `json:"work_dir"`
	ExportPath      *string             `json:"export_path"`
	ExportBytes     *int64              `json:"export_bytes"`
	CreatedAt       string              `json:"created_at"`
	StartedAt       *string             `json:"started_at"`
	FinishedAt      *string             `json:"finished_at"`
	CancelRequested bool                `json:"cancel_requested"`
	ErrorClass      *string             `json:"error_class"`
	ErrorCode       *string             `json:"error_code"`
	SideEffects     *RestoreSideEffects `json:"side_effects"`
	VerifyStatus    string              `json:"verify_status"`
	VerifyResult    any                 `json:"verify_result"`

	IdempotencyKey string `json:"-"`
	RequestHash    string `json:"-"`
}

// RestoreLog is one bounded event of one restore. The log has no event name:
// the message is a fixed phase/lifecycle sentence and data carries the typed
// object.
type RestoreLog struct {
	RestoreID string `json:"restore_id"`
	Seq       int64  `json:"seq"`
	TS        string `json:"ts"`
	Level     string `json:"level"`
	Message   string `json:"message"`
	Data      any    `json:"data"`
}

// MaxRestoreLogRows is the hard cap of restore_logs rows per restore.
const MaxRestoreLogRows = 1000

// RestoreTruncatedMessage is the final message of a full restore log. Further
// events are dropped after it is written once.
const RestoreTruncatedMessage = "restore log reached its cap; later events were dropped"

func restoreFromRow(row storesql.Restore) (Restore, error) {
	restore := Restore{
		ID: row.ID, BackupID: row.BackupID, Status: row.Status, PlanDigest: row.PlanDigest,
		Rehearse: row.Rehearse != 0, RequestedBy: row.RequestedBy, CreatedAt: row.CreatedAt,
		CancelRequested: row.CancelRequested != 0, VerifyStatus: row.VerifyStatus,
		IdempotencyKey: row.IdempotencyKey.String, RequestHash: row.RequestHash.String,
	}
	if err := decodeRestoreJSON(row, &restore); err != nil {
		return Restore{}, err
	}
	nullableString(&restore.Phase, row.Phase)
	nullableString(&restore.WorkDir, row.WorkDir)
	nullableString(&restore.ExportPath, row.ExportPath)
	nullableString(&restore.StartedAt, row.StartedAt)
	nullableString(&restore.FinishedAt, row.FinishedAt)
	nullableString(&restore.ErrorClass, row.ErrorClass)
	nullableString(&restore.ErrorCode, row.ErrorCode)
	return restore, nil
}

// decodeRestoreJSON turns the plan, verify-sql and outcome TEXT columns into
// their typed forms.
func decodeRestoreJSON(row storesql.Restore, restore *Restore) error {
	if err := decodeRestorePlan(row, restore); err != nil {
		return err
	}
	if err := decodeRestoreVerifySQL(row, restore); err != nil {
		return err
	}
	return decodeRestoreOutcome(row, restore)
}

// decodeRestorePlan reads the confirmed plan. Its arrays are always
// materialised as empty slices, never null, so the wire shape is stable.
func decodeRestorePlan(row storesql.Restore, restore *Restore) error {
	if err := json.Unmarshal([]byte(row.PlanJson), &restore.Plan); err != nil {
		return fmt.Errorf("restore %s plan: %w", row.ID, err)
	}
	if restore.Plan.Links == nil {
		restore.Plan.Links = []RestorePlanLink{}
	}
	if restore.Plan.Risks == nil {
		restore.Plan.Risks = []string{}
	}
	return nil
}

// decodeRestoreVerifySQL reads the requested verification statements. An
// absent column is an empty list, not null.
func decodeRestoreVerifySQL(row storesql.Restore, restore *Restore) error {
	restore.VerifySQL = []string{}
	if !row.VerifySqlJson.Valid {
		return nil
	}
	if err := json.Unmarshal([]byte(row.VerifySqlJson.String), &restore.VerifySQL); err != nil {
		return fmt.Errorf("restore %s verify_sql: %w", row.ID, err)
	}
	if restore.VerifySQL == nil {
		restore.VerifySQL = []string{}
	}
	return nil
}

// decodeRestoreOutcome reads the published export size, the exact cleanup
// observation and the rehearsal result.
func decodeRestoreOutcome(row storesql.Restore, restore *Restore) error {
	if row.ExportBytes.Valid {
		size := row.ExportBytes.Int64
		restore.ExportBytes = &size
	}
	if row.SideEffectsJson.Valid {
		var effects RestoreSideEffects
		if err := json.Unmarshal([]byte(row.SideEffectsJson.String), &effects); err != nil {
			return fmt.Errorf("restore %s side_effects: %w", row.ID, err)
		}
		restore.SideEffects = &effects
	}
	if row.VerifyResultJson.Valid {
		var result any
		if err := json.Unmarshal([]byte(row.VerifyResultJson.String), &result); err != nil {
			return fmt.Errorf("restore %s verify_result: %w", row.ID, err)
		}
		restore.VerifyResult = result
	}
	return nil
}

func restoreLogFromRow(row storesql.RestoreLog) RestoreLog {
	return RestoreLog{
		RestoreID: row.RestoreID, Seq: row.Seq, TS: row.Ts, Level: row.Level,
		Message: row.Message, Data: decodeRunLogData(row.Data),
	}
}

// RestoreInput is the complete description of a queued restore. The caller
// generates the 32-hex id, because the plan shown to the operator names the
// export path built from it.
type RestoreInput struct {
	ID             string
	BackupID       string
	Plan           RestorePlan
	PlanDigest     string
	Rehearse       bool
	VerifySQL      []string
	IdempotencyKey string
	RequestHash    string
	RequestedBy    string
	Audit          AuditLog
}

// CreateRestore inserts a queued restore and its success audit in one
// transaction. A duplicate idempotency key reports ErrConflict and leaves the
// stored row untouched; an audit write failure rolls the insert back.
func (s *Store) CreateRestore(ctx context.Context, in RestoreInput) (Restore, error) {
	plan, err := json.Marshal(in.Plan)
	if err != nil {
		return Restore{}, err
	}
	verifySQL, err := encodeVerifySQL(in.VerifySQL)
	if err != nil {
		return Restore{}, err
	}
	var created storesql.Restore
	err = s.withTx(ctx, func(ctx context.Context, q *storesql.Queries) error {
		row, insertErr := q.InsertRestore(ctx, storesql.InsertRestoreParams{
			ID: in.ID, BackupID: in.BackupID, PlanJson: string(plan), PlanDigest: in.PlanDigest,
			Rehearse: encodeBool(in.Rehearse), VerifySqlJson: verifySQL,
			IdempotencyKey: nullable(in.IdempotencyKey), RequestHash: nullable(in.RequestHash),
			RequestedBy: in.RequestedBy, CreatedAt: timestamp(),
		})
		if insertErr != nil {
			return resourceError(insertErr)
		}
		if auditErr := insertSuccessAudit(ctx, q, in.Audit, "restore", row.ID); auditErr != nil {
			return auditErr
		}
		created = row
		return nil
	})
	if err != nil {
		return Restore{}, err
	}
	return restoreFromRow(created)
}

func encodeVerifySQL(statements []string) (sql.NullString, error) {
	if len(statements) == 0 {
		return sql.NullString{}, nil
	}
	encoded, err := json.Marshal(statements)
	if err != nil {
		return sql.NullString{}, err
	}
	return sql.NullString{String: string(encoded), Valid: true}, nil
}

// Restore reads one restore. A missing row reports ErrNotFound.
func (s *Store) Restore(ctx context.Context, id string) (Restore, error) {
	row, err := storesql.New(s.db).GetRestore(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Restore{}, ErrNotFound
		}
		return Restore{}, err
	}
	return restoreFromRow(row)
}

// RestoreByIdempotencyKey reads the restore a key already created.
func (s *Store) RestoreByIdempotencyKey(ctx context.Context, key string) (Restore, error) {
	row, err := storesql.New(s.db).GetRestoreByIdempotencyKey(ctx, nullable(key))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Restore{}, ErrNotFound
		}
		return Restore{}, err
	}
	return restoreFromRow(row)
}

// RestoresFilter bounds a restore list.
type RestoresFilter struct {
	Status, BackupID string
	Before           string
	Limit            int
}

// Restores returns the newest matching restores in descending UUIDv7 order.
func (s *Store) Restores(ctx context.Context, filter RestoresFilter) ([]Restore, error) {
	rows, err := storesql.New(s.db).ListRestores(ctx, storesql.ListRestoresParams{
		Before: filter.Before, Status: filter.Status, BackupID: filter.BackupID,
		RowLimit: int64(filter.Limit),
	})
	if err != nil {
		return nil, err
	}
	restores := make([]Restore, 0, len(rows))
	for _, row := range rows {
		restore, err := restoreFromRow(row)
		if err != nil {
			return nil, err
		}
		restores = append(restores, restore)
	}
	return restores, nil
}

// ClaimRestore marks the next queued restore running. Rows with a cancel
// request are skipped, and a claim is refused while any run or restore is
// already running: the two queues share one global execution slot. It returns
// false when no claimable restore exists.
func (s *Store) ClaimRestore(ctx context.Context) (Restore, bool, error) {
	row, err := storesql.New(s.db).ClaimNextRestore(ctx, nullableTime(time.Now()))
	if errors.Is(err, sql.ErrNoRows) {
		return Restore{}, false, nil
	}
	if err != nil {
		return Restore{}, false, err
	}
	restore, err := restoreFromRow(row)
	if err != nil {
		return Restore{}, false, err
	}
	return restore, true, nil
}

// SetRestoreWorkDir records the private work directory the claimed restore
// uses. The execution rebuilds the same path from the work root; the column
// makes it visible in the API.
func (s *Store) SetRestoreWorkDir(ctx context.Context, id, workDir string) error {
	return storesql.New(s.db).SetRestoreWorkDir(ctx, storesql.SetRestoreWorkDirParams{WorkDir: nullable(workDir), ID: id})
}

// UpdateRestorePhase records the engine phase of a running restore.
func (s *Store) UpdateRestorePhase(ctx context.Context, id, phase string) error {
	return storesql.New(s.db).UpdateRestorePhase(ctx, storesql.UpdateRestorePhaseParams{Phase: nullable(phase), ID: id})
}

// RequestRestoreCancel marks a queued or running restore for cancellation. A
// terminal restore reports ErrConflict so the caller can answer 409.
func (s *Store) RequestRestoreCancel(ctx context.Context, id string, entry AuditLog) error {
	return s.withTx(ctx, func(ctx context.Context, q *storesql.Queries) error {
		if _, err := q.GetRestore(ctx, id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		changed, err := q.RequestRestoreCancel(ctx, id)
		if err != nil {
			return err
		}
		if changed != 1 {
			return ErrConflict
		}
		return insertSuccessAudit(ctx, q, entry, "restore", id)
	})
}

// CancelQueuedRestore cancels a queued restore before a worker claims it.
// changed is false when a claim won the race. The row records the explicit
// all-absent side-effect observation: the engine never ran for this id, so it
// created no work directory, no pending export and no published export.
func (s *Store) CancelQueuedRestore(ctx context.Context, id string, entry AuditLog, log *RunLogWrite) (changed bool, err error) {
	effects, err := json.Marshal(RestoreSideEffects{})
	if err != nil {
		return false, err
	}
	err = s.withTx(ctx, func(ctx context.Context, q *storesql.Queries) error {
		affected, writeErr := q.CancelQueuedRestore(ctx, storesql.CancelQueuedRestoreParams{
			ID: id, FinishedAt: nullableTime(time.Now()), SideEffectsJson: nullable(string(effects)),
		})
		if writeErr != nil {
			return writeErr
		}
		changed = affected == 1
		if !changed {
			return nil
		}
		if auditErr := insertSuccessAudit(ctx, q, entry, "restore", id); auditErr != nil {
			return auditErr
		}
		return appendRestoreLogTx(ctx, q, id, log)
	})
	return changed, err
}

// RestoreOutcome is the terminal observation of one restore execution. Verify
// status and result are recorded even when the restore itself failed after a
// rehearsal ran, so the two outcomes stay separate.
type RestoreOutcome struct {
	ExportPath   string
	ExportBytes  int64
	SideEffects  RestoreSideEffects
	VerifyStatus string
	VerifyResult any
}

// FinishRestoreCompleted records a completed restore with its published
// export.
func (s *Store) FinishRestoreCompleted(ctx context.Context, id string, outcome RestoreOutcome, entry AuditLog, log *RunLogWrite) error {
	return s.withTx(ctx, func(ctx context.Context, q *storesql.Queries) error {
		effects, verifyResult, err := encodeOutcome(outcome)
		if err != nil {
			return err
		}
		changed, err := q.CompleteRestore(ctx, storesql.CompleteRestoreParams{
			ID: id, FinishedAt: nullableTime(time.Now()), ExportPath: nullable(outcome.ExportPath),
			ExportBytes:     sql.NullInt64{Int64: outcome.ExportBytes, Valid: true},
			SideEffectsJson: effects, VerifyStatus: outcome.VerifyStatus, VerifyResultJson: verifyResult,
		})
		if err != nil {
			return err
		}
		if changed != 1 {
			return fmt.Errorf("restore %s is not running", id)
		}
		if auditErr := insertSuccessAudit(ctx, q, entry, "restore", id); auditErr != nil {
			return auditErr
		}
		return appendRestoreLogTx(ctx, q, id, log)
	})
}

// FinishRestoreFailed records a failed restore. class is one of the
// RestoreError* values and code the stable engine code when one exists.
func (s *Store) FinishRestoreFailed(ctx context.Context, id, class, code string, outcome RestoreOutcome, entry AuditLog, log *RunLogWrite) error {
	return s.withTx(ctx, func(ctx context.Context, q *storesql.Queries) error {
		effects, verifyResult, err := encodeOutcome(outcome)
		if err != nil {
			return err
		}
		changed, err := q.FailRestore(ctx, storesql.FailRestoreParams{
			ID: id, FinishedAt: nullableTime(time.Now()), ErrorClass: nullable(class), ErrorCode: nullString(&code),
			SideEffectsJson: effects, VerifyStatus: outcome.VerifyStatus, VerifyResultJson: verifyResult,
		})
		if err != nil {
			return err
		}
		if changed != 1 {
			return fmt.Errorf("restore %s is not running", id)
		}
		if auditErr := insertSuccessAudit(ctx, q, entry, "restore", id); auditErr != nil {
			return auditErr
		}
		return appendRestoreLogTx(ctx, q, id, log)
	})
}

// FinishRestoreCanceled records a canceled restore. A completed restore is
// never rewritten: the published-export transition wins over a cancel request.
func (s *Store) FinishRestoreCanceled(ctx context.Context, id, code string, outcome RestoreOutcome, entry AuditLog, log *RunLogWrite) error {
	return s.withTx(ctx, func(ctx context.Context, q *storesql.Queries) error {
		effects, verifyResult, err := encodeOutcome(outcome)
		if err != nil {
			return err
		}
		changed, err := q.CancelRunningRestore(ctx, storesql.CancelRunningRestoreParams{
			ID: id, FinishedAt: nullableTime(time.Now()), ErrorClass: nullable(RestoreErrorCanceled),
			ErrorCode: nullString(&code), SideEffectsJson: effects,
			VerifyStatus: outcome.VerifyStatus, VerifyResultJson: verifyResult,
		})
		if err != nil {
			return err
		}
		if changed != 1 {
			return ErrConflict
		}
		if auditErr := insertSuccessAudit(ctx, q, entry, "restore", id); auditErr != nil {
			return auditErr
		}
		return appendRestoreLogTx(ctx, q, id, log)
	})
}

// FailInterruptedRestore is the startup-recovery write: every restore a
// previous process left running becomes failed/interrupted with the exact
// side effects recovery observed, so its slot is never re-executed.
func (s *Store) FailInterruptedRestore(ctx context.Context, id string, effects RestoreSideEffects, entry AuditLog, log *RunLogWrite) error {
	encoded, err := json.Marshal(effects)
	if err != nil {
		return err
	}
	return s.withTx(ctx, func(ctx context.Context, q *storesql.Queries) error {
		changed, err := q.FailInterruptedRestore(ctx, storesql.FailInterruptedRestoreParams{
			ID: id, FinishedAt: nullableTime(time.Now()), SideEffectsJson: nullable(string(encoded)),
		})
		if err != nil {
			return err
		}
		if changed != 1 {
			return fmt.Errorf("restore %s is not running", id)
		}
		if auditErr := insertSuccessAudit(ctx, q, entry, "restore", id); auditErr != nil {
			return auditErr
		}
		return appendRestoreLogTx(ctx, q, id, log)
	})
}

func encodeOutcome(outcome RestoreOutcome) (sql.NullString, sql.NullString, error) {
	effects, err := json.Marshal(outcome.SideEffects)
	if err != nil {
		return sql.NullString{}, sql.NullString{}, err
	}
	verifyResult := sql.NullString{}
	if outcome.VerifyResult != nil {
		encoded, err := json.Marshal(outcome.VerifyResult)
		if err != nil {
			return sql.NullString{}, sql.NullString{}, err
		}
		verifyResult = sql.NullString{String: string(encoded), Valid: true}
	}
	return sql.NullString{String: string(effects), Valid: true}, verifyResult, nil
}

// InterruptedRestores lists every running restore, for startup recovery.
func (s *Store) InterruptedRestores(ctx context.Context) ([]Restore, error) {
	rows, err := storesql.New(s.db).InterruptedRestores(ctx)
	if err != nil {
		return nil, err
	}
	restores := make([]Restore, 0, len(rows))
	for _, row := range rows {
		restore, err := restoreFromRow(row)
		if err != nil {
			return nil, err
		}
		restores = append(restores, restore)
	}
	return restores, nil
}

// logDataColumn encodes one restore log payload for its nullable TEXT column.
func logDataColumn(data any) (sql.NullString, error) {
	if data == nil {
		return sql.NullString{}, nil
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return sql.NullString{}, err
	}
	return sql.NullString{String: string(encoded), Valid: true}, nil
}

// AppendRestoreLog appends one fixed-format event, enforcing the per-restore
// cap. When the log is full the event is dropped; a final truncated marker
// replaces the newest row exactly once. The cap check and the insert share one
// transaction on the single write connection.
func (s *Store) AppendRestoreLog(ctx context.Context, id, level, message string, data any) error {
	return s.withTx(ctx, func(ctx context.Context, q *storesql.Queries) error {
		return appendRestoreLogTx(ctx, q, id, &RunLogWrite{Level: level, Message: message, Data: data})
	})
}

func appendRestoreLogTx(ctx context.Context, q *storesql.Queries, id string, log *RunLogWrite) error {
	if log == nil {
		return nil
	}
	count, err := q.CountRestoreLogs(ctx, id)
	if err != nil {
		return err
	}
	if count >= MaxRestoreLogRows {
		return truncateRestoreLog(ctx, q, id)
	}
	data, err := logDataColumn(log.Data)
	if err != nil {
		return err
	}
	return q.InsertRestoreLog(ctx, storesql.InsertRestoreLogParams{
		RestoreID: id, Seq: count + 1, Ts: timestamp(), Level: log.Level,
		Message: log.Message, Data: data,
	})
}

// truncateRestoreLog rewrites the newest row once with the truncated marker,
// so the log always ends in the marker and stays inside the cap.
func truncateRestoreLog(ctx context.Context, q *storesql.Queries, id string) error {
	last, err := q.GetRestoreLog(ctx, storesql.GetRestoreLogParams{RestoreID: id, Seq: MaxRestoreLogRows})
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil || last.Message == RestoreTruncatedMessage {
		return err
	}
	_, err = q.ReplaceRestoreLog(ctx, storesql.ReplaceRestoreLogParams{
		RestoreID: id, Seq: MaxRestoreLogRows, Ts: timestamp(), Level: "warn",
		Message: RestoreTruncatedMessage, Data: sql.NullString{},
	})
	return err
}

// RestoreLog reads one log row. A missing row reports ErrNotFound.
func (s *Store) RestoreLog(ctx context.Context, id string, seq int64) (RestoreLog, error) {
	row, err := storesql.New(s.db).GetRestoreLog(ctx, storesql.GetRestoreLogParams{RestoreID: id, Seq: seq})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RestoreLog{}, ErrNotFound
		}
		return RestoreLog{}, err
	}
	return restoreLogFromRow(row), nil
}

// RestoreLogsAfter returns at most limit rows with a sequence greater than
// after, in ascending order. after = 0 replays from the first event.
func (s *Store) RestoreLogsAfter(ctx context.Context, id string, after, limit int64) ([]RestoreLog, error) {
	rows, err := storesql.New(s.db).RestoreLogsAfter(ctx, storesql.RestoreLogsAfterParams{
		RestoreID: id, Seq: after, Limit: limit,
	})
	if err != nil {
		return nil, err
	}
	logs := make([]RestoreLog, 0, len(rows))
	for _, row := range rows {
		logs = append(logs, restoreLogFromRow(row))
	}
	return logs, nil
}

// MaxRestoreLogSeq returns the newest sequence of one restore's log, 0 when
// empty.
func (s *Store) MaxRestoreLogSeq(ctx context.Context, id string) (int64, error) {
	return storesql.New(s.db).MaxRestoreLogSeq(ctx, id)
}
