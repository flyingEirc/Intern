package dispatch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/flyingEirc/Intern/app/web/internal/store"
)

// Restore timing. restorePoll is the queued-restore poll interval; the worker
// shares the backup worker's one-second cadence. restorePhaseDeadline bounds
// one progress write, matching the run-log budget.
const (
	restorePoll         = time.Second
	restorePhaseTimeout = 10 * time.Second
)

// Restore plan refusal codes. They are the machine codes of restore.PlanError
// and are part of the W5 API contract: the plan and execute endpoints answer
// 404 backup_not_found, 422 matrix_unsupported, 409 for every other refusal and
// 503 for unconfigured.
const (
	RestoreCodeUnconfigured      = "unconfigured"
	RestoreCodeBackupNotFound    = "backup_not_found"
	RestoreCodeMatrixUnsupported = "matrix_unsupported"
	RestoreCodePlanChanged       = "plan_changed"
)

// RestorePlanError is a refused plan build with a fixed machine code.
type RestorePlanError struct {
	Code     string
	BackupID string
}

func (e *RestorePlanError) Error() string {
	return fmt.Sprintf("restore plan refused: %s", e.Code)
}

// RestoreExecError is a failed execution before export publish. SideEffects is
// the engine's exact cleanup observation; a published export is never removed.
type RestoreExecError struct {
	Code        string
	Phase       string
	SideEffects store.RestoreSideEffects
}

func (e *RestoreExecError) Error() string {
	return fmt.Sprintf("restore execution failed at %s: %s", e.Phase, e.Code)
}

// RestoreRequest is one execution request. Plan is the fresh plan the digest
// check accepted, so the engine can refuse a stale request itself.
type RestoreRequest struct {
	RestoreID string
	Plan      store.RestorePlan
	Rehearse  bool
	VerifySQL []string
}

// RestoreResult is a successful execution: the published export, the exact
// cleanup observation and the rehearsal outcome. A rehearsal that ran and
// failed does not fail the restore; VerifyStatus carries that separately.
type RestoreResult struct {
	ExportPath   string
	ExportBytes  int64
	SideEffects  store.RestoreSideEffects
	VerifyStatus string
	VerifyResult any
}

// RestoreProgress is the engine's phase callback. Data is sanitized before it
// is persisted: only an allowlist of scalar keys is stored and no secret or raw
// SQL ever reaches restore_logs.
type RestoreProgress func(phase string, data map[string]any)

// RestoreEngine is the execution seam. Production wraps internal/restore; unit
// tests inject a fake to drive mutual exclusion, cancel and recovery without a
// real xtrabackup.
type RestoreEngine interface {
	Plan(ctx context.Context, backupID string) (store.RestorePlan, error)
	Execute(ctx context.Context, req RestoreRequest, progress RestoreProgress) (RestoreResult, error)
	Recover(ctx context.Context, restoreIDs []string) error
}

// RestoreControl is the dispatch surface the HTTP layer may call.
type RestoreControl interface {
	PlanRestore(ctx context.Context, backupID string) (store.RestorePlan, error)
	ExecuteRestore(ctx context.Context, in RestoreExecute) (RestoreOutcome, RestoreDecision)
	RequestRestoreCancel(ctx context.Context, restoreID string) error
	RestoreConfigured() bool
}

// RestoreOutcome is the result class of one execute request.
type RestoreOutcome int

// Execute outcomes. created and replayed answer 202; refused and plan_changed
// answer 409; rejected answers 422 restore_refused; not_found answers 404
// restore_refused; validation answers 422 validation_failed; unavailable
// answers 503.
const (
	RestoreCreated RestoreOutcome = iota
	RestoreReplayed
	RestoreRefused
	RestoreRejected
	RestoreNotFound
	RestorePlanChanged
	RestoreUnavailable
	RestoreValidation
	RestoreInternal
)

// RestoreDecision is the payload of one execute outcome. Code is the machine
// code a refusal carries in error.detail.code.
type RestoreDecision struct {
	RestoreID string
	Code      string
}

// RestoreExecute is one confirmed execute request. The audit entry is built by
// the HTTP layer and committed in the same transaction as the restore row.
type RestoreExecute struct {
	BackupID       string
	PlanDigest     string
	Rehearse       bool
	VerifySQL      []string
	IdempotencyKey string
	RequestHash    string
	ActorID        string
	Audit          store.AuditLog
}

// MarshalRestoreRequest is the canonical request hash input. It covers every
// field that changes what is executed and never the password.
func MarshalRestoreRequest(backupID, planDigest string, rehearse bool, verifySQL []string) string {
	body := struct {
		BackupID   string   `json:"backup_id"`
		PlanDigest string   `json:"plan_digest"`
		Rehearse   bool     `json:"rehearse"`
		VerifySQL  []string `json:"verify_sql"`
	}{BackupID: backupID, PlanDigest: planDigest, Rehearse: rehearse, VerifySQL: verifySQL}
	if body.VerifySQL == nil {
		body.VerifySQL = []string{}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return backupID + ":" + planDigest
	}
	return string(encoded)
}

// newRestoreID returns the 32 lowercase hex id the API contract requires.
func newRestoreID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}

// restoreWorker claims and executes queued restores. The restore queue shares
// the backup queue's global slot, so the claim itself refuses while any run or
// restore is running.
func (d *Dispatcher) restoreWorker(ctx context.Context) {
	for {
		if err := d.claimAndRestore(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			d.logger.ErrorContext(ctx, "restore worker failed", "error", err)
			if d.hasRunningRestore(ctx) {
				d.logger.ErrorContext(ctx, "a restore is still running after a finish failure; stopping the restore worker so restart recovery can reclaim it")
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(restorePoll):
		}
	}
}

func (d *Dispatcher) hasRunningRestore(ctx context.Context) bool {
	restores, err := d.config.Database.InterruptedRestores(ctx)
	return err == nil && len(restores) > 0
}

// claimAndRestore claims one queued restore and executes it to a terminal
// status.
func (d *Dispatcher) claimAndRestore(ctx context.Context) error {
	restore, claimed, err := d.config.Database.ClaimRestore(ctx)
	if err != nil || !claimed {
		return err
	}
	d.logRestoreEvent(ctx, restore.ID, "info", "restore claimed", map[string]any{
		"backup_id": restore.BackupID, "rehearse": restore.Rehearse,
	})
	d.auditRestore(ctx, "restores.claimed")
	workDir := ""
	if root := d.workRoot(); root != "" {
		workDir = filepath.Join(root, "restore-"+restore.ID)
		if writeErr := d.config.Database.SetRestoreWorkDir(ctx, restore.ID, workDir); writeErr != nil {
			d.logger.ErrorContext(ctx, "cannot record restore work directory", "restore_id", restore.ID, "error", writeErr)
		}
	}
	return d.executeRestore(ctx, restore, workDir)
}

// executeRestore runs one claimed restore through the engine. Every outcome is
// terminal: queued -> running -> completed | failed | canceled. A published
// export wins over a cancel request; a rehearsal failure never turns a
// published restore into a failure.
func (d *Dispatcher) executeRestore(ctx context.Context, restore store.Restore, workDir string) error {
	if d.config.Restore == nil {
		return d.finishRestoreFailed(ctx, restore, store.RestoreErrorUnavailable, RestoreCodeUnconfigured,
			store.RestoreOutcome{SideEffects: store.RestoreSideEffects{}}, "restore runtime is not configured")
	}
	restoreCtx, unregister := d.registerRestoreContext(ctx, restore.ID)
	defer unregister()

	tracker := &restoreProgress{dispatcher: d, parent: ctx, restoreID: restore.ID}
	result, execErr := d.config.Restore.Execute(restoreCtx, RestoreRequest{
		RestoreID: restore.ID, Plan: restore.Plan, Rehearse: restore.Rehearse, VerifySQL: restore.VerifySQL,
	}, tracker.progress)
	if execErr == nil {
		return d.completeRestore(ctx, restore, result)
	}
	return d.refuseOrFailRestore(ctx, restore, restoreCtx, tracker, execErr)
}

// completeRestore records the published export. The write wins over any cancel
// request that arrived while the engine was finishing.
func (d *Dispatcher) completeRestore(ctx context.Context, restore store.Restore, result RestoreResult) error {
	log := restoreLogWrite("info", "restore completed", map[string]any{
		"export_path": result.ExportPath, "export_bytes": result.ExportBytes,
		"verify_status": result.VerifyStatus,
	})
	if err := d.finishRestoreWrite(ctx, restore.ID, func(writeCtx context.Context) error {
		return d.config.Database.FinishRestoreCompleted(writeCtx, restore.ID, store.RestoreOutcome{
			ExportPath: result.ExportPath, ExportBytes: result.ExportBytes,
			SideEffects: result.SideEffects, VerifyStatus: result.VerifyStatus,
			VerifyResult: result.VerifyResult,
		}, store.SystemAudit("restores.completed", store.AuditDetail{}), log)
	}); err != nil {
		return err
	}
	d.logger.InfoContext(ctx, "restore completed", "restore_id", restore.ID, "backup_id", restore.BackupID,
		"export_bytes", result.ExportBytes, "verify_status", result.VerifyStatus)
	return nil
}

// refuseOrFailRestore decides the terminal outcome of a failed execution: an
// operator cancel wins only when no export was published, a shutdown keeps the
// interrupted class every other interrupted row gets, and every other cause is
// recorded under the class restoreFailure derived from the engine's own typed
// error, with the engine's code.
func (d *Dispatcher) refuseOrFailRestore(ctx context.Context, restore store.Restore, restoreCtx context.Context,
	tracker *restoreProgress, execErr error,
) error {
	code, class, phase, effects := restoreFailure(execErr, tracker.phase())
	// The stored flag is re-read because the request may have landed while the
	// engine was finishing its own cleanup.
	var canceled bool
	if stored, err := d.config.Database.Restore(ctx, restore.ID); err == nil {
		canceled = stored.CancelRequested && restoreCtx.Err() != nil
	} else {
		canceled = restore.CancelRequested
	}
	outcome := store.RestoreOutcome{SideEffects: effects, VerifyStatus: store.RestoreVerifyNotRequested}
	if canceled {
		log := restoreLogWrite("warn", "restore canceled", map[string]any{"error_code": code, "phase": phase})
		if err := d.finishRestoreWrite(ctx, restore.ID, func(writeCtx context.Context) error {
			return d.config.Database.FinishRestoreCanceled(writeCtx, restore.ID, code, outcome,
				store.SystemAudit("restores.canceled", store.AuditDetail{}), log)
		}); err != nil {
			return err
		}
		d.logger.InfoContext(ctx, "restore canceled", "restore_id", restore.ID, "phase", phase)
		return nil
	}
	if ctx.Err() != nil {
		class = store.RestoreErrorInterrupted
	}
	return d.finishRestoreFailed(ctx, restore, class, code, outcome,
		fmt.Sprintf("restore failed at %s: %s", phase, code))
}

// restoreProgress is the engine phase callback. It records the newest phase
// once per change and keeps it for the terminal classification.
type restoreProgress struct {
	dispatcher *Dispatcher
	parent     context.Context
	restoreID  string

	mu      sync.Mutex
	last    string
	current string
}

func (p *restoreProgress) progress(phase string, data map[string]any) {
	p.mu.Lock()
	if phase == "" || phase == p.last {
		p.mu.Unlock()
		return
	}
	p.last = phase
	p.current = phase
	p.mu.Unlock()
	p.dispatcher.progressRestore(p.parent, p.restoreID, phase, data)
}

// phase reports the newest phase the engine reached.
func (p *restoreProgress) phase() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.current
}

// restoreFailure extracts the engine's code, phase and cleanup observation, and
// the closed failure class the API documents. A plan refusal is its own class:
// the catalog moved between the confirmed plan and the claim, so the row names
// the engine's stable code under `refused` instead of degrading it to a generic
// execution failure. An error that is not typed keeps a stable generic code.
func restoreFailure(err error, lastPhase string) (code, class, phase string, effects store.RestoreSideEffects) {
	var execErr *RestoreExecError
	if errors.As(err, &execErr) {
		return execErr.Code, store.RestoreErrorFailed, execErr.Phase, execErr.SideEffects
	}
	var planErr *RestorePlanError
	if errors.As(err, &planErr) {
		return planErr.Code, store.RestoreErrorRefused, lastPhase, store.RestoreSideEffects{}
	}
	return "work_failed", store.RestoreErrorFailed, lastPhase, store.RestoreSideEffects{}
}

// finishRestoreFailed records a failed restore with a retry on the detached
// write, so a shutdown or a busy database does not leave the row running.
func (d *Dispatcher) finishRestoreFailed(ctx context.Context, restore store.Restore, class, code string,
	outcome store.RestoreOutcome, message string,
) error {
	if outcome.VerifyStatus == "" {
		outcome.VerifyStatus = store.RestoreVerifyNotRequested
	}
	log := restoreLogWrite("error", message, map[string]any{"error_class": class, "error_code": code})
	return d.finishRestoreWrite(ctx, restore.ID, func(writeCtx context.Context) error {
		return d.config.Database.FinishRestoreFailed(writeCtx, restore.ID, class, code, outcome,
			store.SystemAudit("restores.failed", store.AuditDetail{}), log)
	})
}

// finishRestoreWrite persists one terminal restore outcome. Like the run
// terminal writes it is detached from the worker's cancellation with a bounded
// window, because shutdown cancels the worker context first.
func (d *Dispatcher) finishRestoreWrite(parent context.Context, restoreID string, op func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), finishWindow)
	defer cancel()
	var err error
	for attempt := 1; attempt <= finishAttempts; attempt++ {
		err = op(ctx)
		if err == nil {
			return nil
		}
		d.logger.ErrorContext(parent, "cannot persist restore outcome", "restore_id", restoreID, "attempt", attempt, "error", err)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(claimRetry):
		}
	}
	return err
}

// registerRestoreContext derives the cancelable context of one claimed restore
// and registers it, so RequestRestoreCancel reaches the engine's tools.
func (d *Dispatcher) registerRestoreContext(ctx context.Context, restoreID string) (context.Context, func()) {
	restoreCtx, cancel := context.WithCancel(ctx)
	unregister := func() {
		cancel()
		d.mu.Lock()
		if d.runningRestoreID == restoreID {
			d.runningRestoreCancel = nil
			d.runningRestoreID = ""
		}
		d.mu.Unlock()
	}
	d.mu.Lock()
	d.runningRestoreCancel = cancel
	d.runningRestoreID = restoreID
	d.mu.Unlock()
	return restoreCtx, unregister
}

// progressRestore records one engine phase and appends its bounded log event.
// The write is detached from the engine context: a phase the engine reached is
// evidence even when the next step is canceled.
func (d *Dispatcher) progressRestore(parent context.Context, restoreID, phase string, data map[string]any) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), restorePhaseTimeout)
	defer cancel()
	if err := d.config.Database.UpdateRestorePhase(ctx, restoreID, phase); err != nil {
		d.logger.ErrorContext(parent, "cannot record restore phase", "restore_id", restoreID, "phase", phase, "error", err)
	}
	safe := sanitizeProgressData(data)
	safe["phase"] = phase
	d.logRestoreEvent(ctx, restoreID, "info", "restore phase "+phase, safe)
}

// progressKeys is the allowlist of progress keys that reach restore_logs.
// Everything else is dropped: the engine's map is not a trusted wire format.
var progressKeys = map[string]bool{
	"backup_id": true, "bytes": true, "count": true, "link": true, "phase": true,
	"repository_id": true, "seq": true, "source": true, "status": true,
}

// sanitizeProgressData keeps only allowlisted scalar values of bounded size.
func sanitizeProgressData(data map[string]any) map[string]any {
	safe := make(map[string]any, len(data))
	for key, value := range data {
		if !progressKeys[key] {
			continue
		}
		switch typed := value.(type) {
		case string:
			if len(typed) <= 128 && !strings.ContainsAny(typed, "\x00\n\r") {
				safe[key] = typed
			}
		case int, int32, int64, uint32, uint64, float64, bool:
			safe[key] = typed
		}
	}
	return safe
}

// RestoreConfigured reports whether this deployment can plan and execute
// restores. The API answers 503 for plan and execute when it cannot.
func (d *Dispatcher) RestoreConfigured() bool { return d.config.Restore != nil }

// PlanRestore builds a fresh restore plan. It writes nothing: no restore row,
// no file and no FTP object. A refusal is a *RestorePlanError.
func (d *Dispatcher) PlanRestore(ctx context.Context, backupID string) (store.RestorePlan, error) {
	if d.config.Restore == nil {
		return store.RestorePlan{}, &RestorePlanError{Code: RestoreCodeUnconfigured, BackupID: backupID}
	}
	plan, err := d.config.Restore.Plan(ctx, backupID)
	if err != nil {
		var planErr *RestorePlanError
		if errors.As(err, &planErr) {
			return store.RestorePlan{}, planErr
		}
		return store.RestorePlan{}, err
	}
	return plan, nil
}

// ExecuteRestore creates one restore behind the Idempotency-Key contract. The
// row and its success audit commit in one transaction; a replay of the same
// key and request answers the stored id, a different request with the same key
// is a refusal. The plan is always rebuilt at request time, so a stale digest
// is refused before any row exists.
func (d *Dispatcher) ExecuteRestore(ctx context.Context, in RestoreExecute) (RestoreOutcome, RestoreDecision) {
	if d.config.Restore == nil {
		return RestoreUnavailable, RestoreDecision{}
	}
	if in.IdempotencyKey != "" {
		if outcome, decision, found := d.replayRestore(ctx, in); found {
			return outcome, decision
		}
	}
	fresh, err := d.config.Restore.Plan(ctx, in.BackupID)
	if err != nil {
		return d.restorePlanOutcome(err)
	}
	if fresh.Digest != in.PlanDigest {
		return RestorePlanChanged, RestoreDecision{Code: RestoreCodePlanChanged}
	}
	if in.Rehearse && !fresh.RehearsalAvailable {
		return RestoreValidation, RestoreDecision{Code: "rehearsal_unavailable"}
	}
	return d.insertRestore(ctx, in, fresh)
}

// insertRestore writes the queued row with its confirmed plan. A lost
// idempotency race replays the winner when its request matches.
func (d *Dispatcher) insertRestore(ctx context.Context, in RestoreExecute, fresh store.RestorePlan) (RestoreOutcome, RestoreDecision) {
	id, err := newRestoreID()
	if err != nil {
		d.logger.ErrorContext(ctx, "cannot generate restore id", "error", err)
		return RestoreInternal, RestoreDecision{}
	}
	row, err := d.config.Database.CreateRestore(ctx, store.RestoreInput{
		ID: id, BackupID: in.BackupID, Plan: fresh, PlanDigest: fresh.Digest,
		Rehearse: in.Rehearse, VerifySQL: in.VerifySQL, IdempotencyKey: in.IdempotencyKey,
		RequestHash: in.RequestHash, RequestedBy: in.ActorID, Audit: in.Audit,
	})
	if errors.Is(err, store.ErrConflict) {
		if outcome, decision, found := d.replayRestore(ctx, in); found {
			return outcome, decision
		}
		return RestoreRefused, RestoreDecision{Code: "conflict"}
	}
	if err != nil {
		d.logger.ErrorContext(ctx, "cannot create restore", "backup_id", in.BackupID, "error", err)
		return RestoreInternal, RestoreDecision{}
	}
	d.logRestoreEvent(ctx, row.ID, "info", "restore queued", map[string]any{
		"backup_id": row.BackupID, "rehearse": row.Rehearse,
	})
	return RestoreCreated, RestoreDecision{RestoreID: row.ID}
}

// replayRestore answers an existing idempotency key: the same request replays
// the stored restore, anything else is a refusal. found reports whether the key
// already decided the outcome.
func (d *Dispatcher) replayRestore(ctx context.Context, in RestoreExecute) (RestoreOutcome, RestoreDecision, bool) {
	existing, err := d.config.Database.RestoreByIdempotencyKey(ctx, in.IdempotencyKey)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return RestoreCreated, RestoreDecision{}, false
	case err != nil:
		d.logger.ErrorContext(ctx, "cannot read idempotent restore", "key", in.IdempotencyKey, "error", err)
		return RestoreInternal, RestoreDecision{}, true
	case existing.RequestHash != in.RequestHash || existing.BackupID != in.BackupID:
		return RestoreRefused, RestoreDecision{Code: "conflict"}, true
	default:
		return RestoreReplayed, RestoreDecision{RestoreID: existing.ID}, true
	}
}

// restorePlanOutcome maps a plan refusal onto the execute outcome classes.
func (d *Dispatcher) restorePlanOutcome(err error) (RestoreOutcome, RestoreDecision) {
	var planErr *RestorePlanError
	if !errors.As(err, &planErr) {
		d.logger.ErrorContext(context.Background(), "restore plan failed", "error", err)
		return RestoreInternal, RestoreDecision{}
	}
	switch planErr.Code {
	case RestoreCodeUnconfigured:
		return RestoreUnavailable, RestoreDecision{Code: planErr.Code}
	case RestoreCodeBackupNotFound:
		return RestoreNotFound, RestoreDecision{Code: planErr.Code}
	case RestoreCodeMatrixUnsupported:
		return RestoreRejected, RestoreDecision{Code: planErr.Code}
	default:
		return RestoreRefused, RestoreDecision{Code: planErr.Code}
	}
}

// RequestRestoreCancel asks a restore to stop. A queued restore becomes
// canceled without running; a running restore's context is canceled, and the
// cancel only wins when no export was published.
func (d *Dispatcher) RequestRestoreCancel(ctx context.Context, restoreID string) error {
	restore, err := d.config.Database.Restore(ctx, restoreID)
	if err != nil {
		return err
	}
	if restore.Status == store.RestoreStatusQueued {
		changed, err := d.config.Database.CancelQueuedRestore(ctx, restore.ID,
			store.SystemAudit("restores.canceled", store.AuditDetail{}), restoreLogWrite("warn", "restore canceled", nil))
		if err != nil {
			return err
		}
		if changed {
			return nil
		}
		// A claim won the race; fall through to the running-restore path.
	}
	if err := d.config.Database.RequestRestoreCancel(ctx, restore.ID,
		store.SystemAudit("restores.cancel_requested", store.AuditDetail{})); err != nil {
		return err
	}
	d.mu.Lock()
	cancel, active := d.runningRestoreCancel, d.runningRestoreID == restoreID
	d.mu.Unlock()
	if active && cancel != nil {
		cancel()
	}
	return nil
}

// auditRestore writes one system audit row for a restore lifecycle transition.
func (d *Dispatcher) auditRestore(ctx context.Context, action string) {
	entry := store.SystemAudit(action, store.AuditDetail{})
	if err := d.config.Database.Audit(ctx, entry); err != nil {
		d.logger.ErrorContext(ctx, "cannot write system audit", "action", action, "error", err)
	}
}

// logRestoreEvent appends one fixed-format restore log event. The write is
// detached from the caller's cancellation and bounded like the run log.
func (d *Dispatcher) logRestoreEvent(ctx context.Context, restoreID, level, message string, data any) {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runLogCapDeadline)
	defer cancel()
	if err := d.config.Database.AppendRestoreLog(writeCtx, restoreID, level, message, eventPayload(data)); err != nil {
		d.logger.ErrorContext(ctx, "cannot append restore log", "restore_id", restoreID, "error", err)
	}
}

func restoreLogWrite(level, message string, data any) *store.RunLogWrite {
	return &store.RunLogWrite{Level: level, Message: message, Data: eventPayload(data)}
}

// failInterruptedRestores is the second frozen recovery step: every restore a
// previous process left running is failed/interrupted after its two exact
// targets are reclaimed. A published export is observed, never removed.
func (d *Dispatcher) failInterruptedRestores(ctx context.Context) error {
	restores, err := d.config.Database.InterruptedRestores(ctx)
	if err != nil {
		return fmt.Errorf("dispatch: interrupted restores: %w", err)
	}
	for _, restore := range restores {
		effects, recoverErr := d.recoverRestoreTargets(ctx, restore.ID)
		if recoverErr != nil {
			return fmt.Errorf("dispatch: recover restore %s: %w", restore.ID, recoverErr)
		}
		entry := store.SystemAudit("restores.interrupted", store.AuditDetail{})
		log := restoreLogWrite("error", "restore interrupted", map[string]any{
			"work_dir_removed": effects.WorkDirRemoved, "pending_export_removed": effects.PendingExportRemoved,
			"export_published": effects.ExportPublished,
		})
		if failErr := d.config.Database.FailInterruptedRestore(ctx, restore.ID, effects, entry, log); failErr != nil {
			return fmt.Errorf("dispatch: interrupt restore %s: %w", restore.ID, failErr)
		}
		d.logger.InfoContext(ctx, "interrupted restore failed at startup",
			"restore_id", restore.ID, "backup_id", restore.BackupID)
	}
	return nil
}

// recoverRestoreTargets reclaims the two persisted targets of one interrupted
// restore and reports what was observed. restore.Recover only removes the ids
// it is given: nothing is discovered by prefix scans.
func (d *Dispatcher) recoverRestoreTargets(ctx context.Context, restoreID string) (store.RestoreSideEffects, error) {
	var effects store.RestoreSideEffects
	exportRoot := d.config.Settings.ExportRoot
	if d.config.Restore == nil || exportRoot == "" || d.workRoot() == "" {
		return effects, nil
	}
	workDir := filepath.Join(d.workRoot(), "restore-"+restoreID)
	pending := filepath.Join(exportRoot, ".pending-"+restoreID)
	published := filepath.Join(exportRoot, restoreID)
	workExisted := pathExists(workDir)
	pendingExisted := pathExists(pending)
	if err := d.config.Restore.Recover(ctx, []string{restoreID}); err != nil {
		return effects, err
	}
	effects.WorkDirRemoved = workExisted && !pathExists(workDir)
	effects.PendingExportRemoved = pendingExisted && !pathExists(pending)
	effects.ExportPublished = pathExists(published)
	return effects, nil
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
