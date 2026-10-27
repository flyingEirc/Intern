package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/flyingEirc/Intern/app/web/internal/resources"
	"github.com/flyingEirc/Intern/app/web/internal/store"
	"github.com/flyingEirc/Intern/internal/physical"
	"github.com/flyingEirc/Intern/internal/physical/chain"
	physicalstore "github.com/flyingEirc/Intern/internal/physical/store"
	"github.com/flyingEirc/Intern/internal/storage/local"
)

// finishAttempts bounds the retries of one terminal run write. finishWindow
// bounds the detached window that write runs in: it is the same commit budget
// chain.Complete uses, and shutdownWait is sized to outlast it.
const (
	finishAttempts = 5
	finishWindow   = 15 * time.Second
)

// backupWorker claims and executes runs. Global backup concurrency is 1: the
// claim refuses a new row while any run is still running. A FinishRun* failure
// that leaves the row running is retried with bounded backoff; if it still
// fails the worker stops so restart recovery can reclaim the slot.
func (d *Dispatcher) backupWorker(ctx context.Context) {
	for {
		if err := d.claimAndRun(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			d.logger.ErrorContext(ctx, "backup worker failed", "error", err)
			if d.hasRunningRun(ctx) {
				d.logger.ErrorContext(ctx, "a run is still running after a finish failure; stopping the backup worker so restart recovery can reclaim it")
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(workerPoll):
		}
	}
}

func (d *Dispatcher) hasRunningRun(ctx context.Context) bool {
	runs, err := d.config.Database.InterruptedRuns(ctx)
	return err == nil && len(runs) > 0
}

// finishWithRetry persists one terminal run outcome. The write is detached
// from the caller's cancellation with a bounded window: shutdown cancels the
// run context before the workers drain, so a write that inherited it would
// fail every attempt and leave the row running for the next start's recovery
// to reclaim. The window matches chain.Complete's own commit window, which
// shutdownWait is sized to outlast.
func (d *Dispatcher) finishWithRetry(parent context.Context, runID string, op func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), finishWindow)
	defer cancel()
	var err error
	for attempt := 1; attempt <= finishAttempts; attempt++ {
		err = op(ctx)
		if err == nil {
			return nil
		}
		d.logger.ErrorContext(parent, "cannot persist run outcome", "run_id", runID, "attempt", attempt, "error", err)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(claimRetry):
		}
	}
	return err
}

// claimAndRun claims one queued run and executes it to a terminal status.
func (d *Dispatcher) claimAndRun(ctx context.Context) error {
	run, claimed, err := d.config.Database.ClaimRun(ctx)
	if err != nil || !claimed {
		return err
	}
	d.logger.InfoContext(ctx, "run claimed", "run_id", run.ID, "kind", run.Kind,
		"policy_id", run.PolicyID, "trigger", run.Trigger)
	d.auditRun(ctx, "runs.claimed")
	d.logEvent(ctx, run.ID, eventClaimed, nil)
	return d.executeRun(ctx, run)
}

// auditRun writes one system audit row for a run lifecycle transition. The
// audit does not depend on HTTP middleware.
func (d *Dispatcher) auditRun(ctx context.Context, action string) {
	entry := store.SystemAudit(action, store.AuditDetail{})
	if err := d.config.Database.Audit(ctx, entry); err != nil {
		d.logger.ErrorContext(ctx, "cannot write system audit", "action", action, "error", err)
	}
}

// executeRun runs one claimed run through preflight, the physical chain and
// the replication handoff. Every outcome is terminal: queued -> running ->
// completed | failed | canceled.
//
// The run's cancel context is registered before the post-claim reads, so a
// cancel request that arrives during the preflight, the version probe or the
// connection checks is honored too: the preflight reports the canceled code and
// preChainFailure turns that into a canceled run.
//
// The policy read, the local repository open and the KEK repository read run
// under the parent context, not the run context. They are short control-plane
// reads: cancelling them would surface as a policy or repository failure even
// though nothing was wrong with either. A cancel request that lands during them
// is still honored, because preChainFailure re-reads the stored flag before any
// pre-chain failure decides the outcome.
func (d *Dispatcher) executeRun(ctx context.Context, run store.Run) error {
	runCtx, unregister := d.registerRunContext(ctx, run.ID)
	defer unregister()
	policy, err := d.config.Database.Policy(ctx, run.PolicyID)
	if err != nil {
		return d.preChainFailure(ctx, run, store.RunErrorBackupFailed, "policy_missing", err)
	}
	bindRunPair(&policy, run)
	repo, cancelled, err := d.openRunRepository(ctx, run, policy)
	if cancelled {
		return err
	}
	if err != nil {
		return d.preChainFailure(ctx, run, store.RunErrorBackupFailed, "repository_unavailable", err)
	}
	// The repository handle is read-only here; a close failure cannot change
	// the run outcome that the rest of this function decides.
	defer func() { _ = repo.Close() }() //nolint:errcheck // read-only handle, run outcome already decided

	admitted, done, err := d.admit(ctx, runCtx, run, policy)
	if done {
		return err
	}
	// A cancel request that landed between the preflight probes and the chain
	// must end the run here: the chain would otherwise run to completion with
	// the flag set and no one re-reading it. The stored flag, not the context,
	// decides, so the read is ordered before chain.Run and a cancel that races
	// it is still covered by failChain afterwards.
	if canceled, err := d.refuseCanceledRun(ctx, run); canceled || err != nil {
		return err
	}
	d.logEvent(ctx, run.ID, eventStarted, nil)
	result, runErr := d.runChain(runCtx, run, d.chainJob(run, policy, repo, admitted))

	if admission := (*physicalstore.AdmissionError)(nil); errors.As(runErr, &admission) {
		return d.admissionFailure(ctx, run, policy, admission)
	}
	if runErr != nil {
		return d.failChain(ctx, run, runErr)
	}
	return d.completeRun(ctx, run, policy, result)
}

// bindRunPair pins the (local, replica) pair the claim snapshotted onto the
// policy this run works from, so the KEK check, the preflight, the chain and
// the replication handoff all use the repositories the run was claimed with. A
// PUT that moved the policy after the claim changes the next run's pair only.
func bindRunPair(policy *store.Policy, run store.Run) {
	if run.LocalRepositoryID != nil {
		policy.LocalRepositoryID = *run.LocalRepositoryID
	}
	policy.ReplicaRepositoryID = run.ReplicaRepositoryID
}

// preChainFailure records a failure that happened before chain.Run.
//
// It decides the outcome by the same rule failChain applies after the chain, so
// a shutdown or a cancel never depends on which side of the chain boundary it
// landed on:
//   - a cancel request wins while the worker's own context is still live: the
//     run ends canceled and leaves no backup;
//   - a shutdown of the worker loop (its parent context canceled) ends the run
//     failed with class backup_failed, which is exactly what failChain reports;
//   - every other cause keeps the class and code it was given.
func (d *Dispatcher) preChainFailure(parent context.Context, run store.Run, class, code string, cause error) error {
	stored, err := d.config.Database.Run(parent, run.ID)
	if err == nil && stored.CancelRequested && parent.Err() == nil {
		return d.cancelRun(parent, stored)
	}
	if parent.Err() != nil {
		return d.failRun(parent, run, store.RunErrorBackupFailed, "backup_failed", cause)
	}
	return d.failRun(parent, run, class, code, cause)
}

// registerRunContext derives the cancelable context of one claimed run and
// registers it, so RequestCancel reaches the preflight, the version probe and
// the connection checks as well as chain.Run.
func (d *Dispatcher) registerRunContext(ctx context.Context, runID string) (context.Context, func()) {
	runCtx, cancel := context.WithCancel(ctx)
	unregister := func() {
		cancel()
		d.mu.Lock()
		if d.runningRunID == runID {
			d.runningCancel = nil
			d.runningRunID = ""
		}
		d.mu.Unlock()
	}
	d.mu.Lock()
	d.runningCancel = cancel
	d.runningRunID = runID
	d.mu.Unlock()
	return runCtx, unregister
}

// refuseCanceledRun ends a claimed run as canceled when a cancel request has
// already arrived: the run is running, so its context is canceled and the
// terminal write is the canceled one, with no backup.
func (d *Dispatcher) refuseCanceledRun(ctx context.Context, run store.Run) (bool, error) {
	stored, err := d.config.Database.Run(ctx, run.ID)
	if err != nil {
		return false, err
	}
	if !stored.CancelRequested {
		return false, nil
	}
	d.mu.Lock()
	cancel, active := d.runningCancel, d.runningRunID == run.ID
	d.mu.Unlock()
	if active && cancel != nil {
		cancel()
	}
	return true, d.cancelRun(ctx, stored)
}

// failChain records a chain failure. A cancel request that arrived while the
// chain was running wins only when the chain did not succeed.
func (d *Dispatcher) failChain(ctx context.Context, run store.Run, runErr error) error {
	if stored, err := d.config.Database.Run(ctx, run.ID); err == nil &&
		stored.CancelRequested && ctx.Err() == nil {
		return d.cancelRun(ctx, stored)
	}
	return d.failRun(ctx, run, store.RunErrorBackupFailed, "backup_failed", runErr)
}

// completeRun commits the durable success point, which wins over any cancel
// request that arrived afterwards, then hands the backup to replication.
func (d *Dispatcher) completeRun(ctx context.Context, run store.Run, policy store.Policy, result chain.Result) error {
	log := runLogWrite(eventCompleted, map[string]any{
		"backup_id": result.BackupID, "chain_id": result.ChainID, "seq": result.Seq,
		"payload_size": result.PayloadSize, "payload_sha256": result.PayloadSHA256,
	})
	if err := d.finishWithRetry(ctx, run.ID, func(writeCtx context.Context) error {
		return d.config.Database.FinishRunCompleted(writeCtx, run.ID, result.BackupID, result.ChainID,
			result.PayloadSize, store.SystemAudit("runs.completed", store.AuditDetail{}), log)
	}); err != nil {
		return err
	}
	d.logger.InfoContext(ctx, "run completed", "run_id", run.ID, "backup_id", result.BackupID,
		"chain_id", result.ChainID, "payload_size", result.PayloadSize)
	d.enqueueReplication(ctx, run, policy, result.BackupID)
	return nil
}

// admit enforces the two frozen preconditions of a claimed run: the runtime is
// configured (decision 9) and a fresh preflight passed (decision 10). done
// reports that the run already reached a terminal failure and its write is the
// returned error.
//
// parent is the worker's context and runCtx the cancelable context of this run.
// The KEK repository read uses the parent because it is a short control-plane
// read; the preflight and the connection checks use runCtx, so a cancel request
// stops the running probe. Either way the terminal outcome goes through
// preChainFailure: a cancel request ends the run canceled and leaves no backup,
// and a shutdown ends it exactly as failChain reports a shutdown.
func (d *Dispatcher) admit(parent, runCtx context.Context, run store.Run, policy store.Policy) (admission, bool, error) {
	// Decision 9: without the KEK, work root or XtraBackup executable the run
	// was still created, and now fails visibly. Nothing is skipped silently.
	if !d.config.Settings.Preflight() {
		return admission{}, true, d.preChainFailure(parent, run, store.RunErrorUnavailable, "unconfigured",
			errors.New("backup runtime is not configured"))
	}
	if err := d.validateKEK(parent, run, policy); err != nil {
		return admission{}, true, err
	}
	outcome, code, preflightErr := d.preflightRun(runCtx, run, policy)
	if code == "canceled" || (code != "" && runCtx.Err() != nil) {
		return admission{}, true, d.preChainFailure(parent, run, store.RunErrorPreflight, code, preflightErr)
	}
	if code != "" {
		return admission{}, true, d.failRun(parent, run, store.RunErrorPreflight, code, preflightErr)
	}
	connection, identity, err := d.preflightConnection(runCtx, policy)
	if err != nil {
		if runCtx.Err() != nil {
			return admission{}, true, d.preChainFailure(parent, run, store.RunErrorPreflight, "identity_missing", err)
		}
		return admission{}, true, d.failRun(parent, run, store.RunErrorPreflight, "identity_missing", err)
	}
	return admissionFor(connection, identity, outcome), false, nil
}

// admission is what a passed admission hands the physical chain: the live
// connection, the server identity the fresh preflight recorded, and the
// XtraBackup version that same preflight probed.
type admission struct {
	Connection  physical.Connection
	Identity    physical.Identity
	ToolVersion string
}

// admissionFor binds one passed preflight to the live connection of a run. The
// tool version is the probed XtraBackup version, never the server version: the
// physical chain writes it into the backup metadata and rechecks it on every
// later member of the chain.
func admissionFor(connection physical.Connection, identity physical.Identity, outcome resources.PreflightOutcome) admission {
	return admission{Connection: connection, Identity: identity, ToolVersion: outcome.ToolVersion}
}

// openRunRepository resolves the pinned local repository for one run. A run
// canceled before its claim is cancelled here rather than executed; report
// cancelled=true with the terminal write already done. Every other failure goes
// through preChainFailure, so a cancel request that arrived in the meantime ends
// the run canceled instead of as a repository failure.
func (d *Dispatcher) openRunRepository(ctx context.Context, run store.Run, policy store.Policy) (*local.Repository, bool, error) {
	// A queued run that received a cancel request before its claim is canceled
	// without running. The claim skips such rows; this recheck closes the race
	// where the request landed between the claim and this read.
	stored, err := d.config.Database.Run(ctx, run.ID)
	if err != nil {
		return nil, false, err
	}
	if stored.CancelRequested {
		return nil, true, d.cancelRun(ctx, stored)
	}
	repository, err := d.config.Database.Repository(ctx, policy.LocalRepositoryID)
	if err != nil {
		return nil, true, d.preChainFailure(ctx, run, store.RunErrorBackupFailed, "repository_missing", err)
	}
	if repository.Kind != store.RepositoryLocal || repository.Local == nil {
		return nil, true, d.preChainFailure(ctx, run, store.RunErrorBackupFailed, "repository_kind",
			errors.New("local repository required"))
	}
	repo, err := local.Open(repository.Local.Root)
	if err != nil {
		return nil, true, d.preChainFailure(ctx, run, store.RunErrorBackupFailed, "repository_unavailable", err)
	}
	return repo, false, nil
}

// runChain runs one chain under the run context that executeRun registered at
// claim time, so a cancel request reaches the in-flight tool through the same
// registration that already covered the preflight.
func (d *Dispatcher) runChain(ctx context.Context, _ store.Run, job chain.Job) (chain.Result, error) {
	return chain.Run(ctx, job)
}

// preflightRun runs the fresh preflight and records it on the source. It
// returns the observed outcome, the stable failure code (empty when the run may
// proceed) and the underlying error for the run record. A canceled context is
// reported as the canceled code, so a cancel that lands during the preflight or
// the version probe ends the run as canceled, not as a preflight failure.
func (d *Dispatcher) preflightRun(ctx context.Context, run store.Run, policy store.Policy) (resources.PreflightOutcome, string, error) {
	outcome, err := d.preflightService().PreflightSource(ctx, policy.SourceID, policy.LocalRepositoryID)
	if code, failed := preflightErrorCode(ctx, err); failed {
		return outcome, code, err
	}
	record := outcome.Record()
	if _, err := d.config.Database.RecordSourcePreflight(ctx, policy.SourceID, record,
		store.SystemAudit("sources.preflight", store.AuditDetail{})); err != nil {
		d.logger.ErrorContext(ctx, "cannot record preflight", "run_id", run.ID, "error", err)
	}
	if outcome.OK() {
		d.logEvent(ctx, run.ID, eventPreflightOK, nil)
		return outcome, "", nil
	}
	if ctx.Err() != nil || (outcome.Failure != nil && outcome.Failure.Code == "canceled") {
		return outcome, "canceled", fmt.Errorf("preflight canceled")
	}
	code, check := "preflight_failed", "preflight"
	if outcome.Failure != nil {
		if outcome.Failure.Code != "" {
			code = outcome.Failure.Code
		}
		if outcome.Failure.Check != "" {
			check = outcome.Failure.Check
		}
	}
	d.logEvent(ctx, run.ID, eventPreflightFailed, map[string]any{"check": check, "code": code})
	return outcome, code, fmt.Errorf("preflight failed")
}

// preflightService returns the injected preflight or the production resources
// service over this dispatcher's database.
func (d *Dispatcher) preflightService() PreflightRunner {
	if d.config.Preflight != nil {
		return d.config.Preflight
	}
	return &resources.Service{Store: d.config.Database, Settings: d.config.Settings}
}

// preflightErrorCode maps a preflight Go error to the stable run code and
// reports whether the call failed at all. A canceled context is reported as
// canceled, so a cancel during the preflight is not recorded as a preflight
// failure.
func preflightErrorCode(ctx context.Context, err error) (string, bool) {
	switch {
	case err == nil:
		return "", false
	case errors.Is(err, resources.ErrUnavailable), errors.Is(err, store.ErrUnavailable):
		return "unconfigured", true
	case errors.Is(err, store.ErrNotFound):
		return "source_missing", true
	case ctx.Err() != nil:
		return "canceled", true
	default:
		return "preflight_failed", true
	}
}

// validateKEK rechecks that the KEK is outside the run's pinned local
// repository root before chain.Run. The repository read and every failure exit
// run under the parent context: this is a short control-plane read, and a
// cancel request must not be reported as a missing repository. A violation
// fails the run with error_class preflight.
func (d *Dispatcher) validateKEK(ctx context.Context, run store.Run, policy store.Policy) error {
	repository, err := d.config.Database.Repository(ctx, policy.LocalRepositoryID)
	if err != nil {
		return d.preChainFailure(ctx, run, store.RunErrorPreflight, "repository_missing", err)
	}
	if repository.Kind != store.RepositoryLocal || repository.Local == nil {
		return d.preChainFailure(ctx, run, store.RunErrorPreflight, "repository_kind",
			errors.New("local repository required"))
	}
	if err := d.config.Settings.ValidateRepositoryRoot(repository.Local.Root); err != nil {
		d.logEvent(ctx, run.ID, eventPreflightFailed, map[string]any{
			"check": "kek", "code": "kek_inside_repository",
		})
		return d.preChainFailure(ctx, run, store.RunErrorPreflight, "kek_inside_repository", err)
	}
	return nil
}

// admissionFailure implements decision 10: an AdmissionError with RequiresFull
// on an incremental queues one coalesced full with the admission idempotency
// key, and the incremental run fails with error_class requires_full.
func (d *Dispatcher) admissionFailure(ctx context.Context, run store.Run, policy store.Policy, admission *physicalstore.AdmissionError) error {
	if admission.RequiresFull && run.Kind == store.RunKindIncremental {
		d.queueAdmissionFull(ctx, run, policy)
		return d.failRun(ctx, run, store.RunErrorRequiresFull, admission.Code, admission)
	}
	return d.failRun(ctx, run, store.RunErrorBackupFailed, "admission_failed", admission)
}

// queueAdmissionFull queues the coalesced full one refused incremental asks for.
// At most one full is queued or running per source (HasActiveFull); the key is
// the contract's policy:<id>:full:admission:<scheduled_at>, so a previous
// admission full that already failed does not block the next one. No replicator
// is opened here: the completion path and the replication worker own that.
func (d *Dispatcher) queueAdmissionFull(ctx context.Context, run store.Run, policy store.Policy) {
	active, err := d.config.Database.HasActiveFull(ctx, policy.SourceID)
	if err != nil {
		d.logger.ErrorContext(ctx, "cannot check admission full", "run_id", run.ID, "error", err)
		return
	}
	if active {
		return
	}
	scheduledAt := d.now()
	created, err := d.config.Database.CreateRun(ctx, store.RunInput{
		PolicyID: policy.ID, SourceID: policy.SourceID, Kind: store.RunKindFull, Trigger: store.RunTriggerAdmission,
		IdempotencyKey: admissionKey(policy.ID, scheduledAt),
		RequestHash:    cronRequestHash(policy, store.RunKindFull),
		ScheduledAt:    scheduledAt,
	})
	if errors.Is(err, store.ErrConflict) {
		return
	}
	if err != nil {
		d.logger.ErrorContext(ctx, "cannot queue admission full", "run_id", run.ID, "error", err)
		return
	}
	d.logEvent(ctx, created.ID, eventQueued, nil)
}

// chainJob builds the physical chain Job for one admitted run. validateJob
// rechecks the KEK against the work root; the repository-root recheck happens
// through the preflight and the local repository open.
func (d *Dispatcher) chainJob(run store.Run, policy store.Policy, repo *local.Repository, admitted admission) chain.Job {
	return chain.Job{
		Store: physicalstore.New(d.config.Database.DB()), Repo: repo,
		Binary: d.config.Settings.XtrabackupPath, WorkRoot: d.config.Settings.WorkRoot,
		Connection: admitted.Connection,
		Identity:   admitted.Identity, ToolVersion: admitted.ToolVersion,
		InstanceID: d.config.Database.InstanceID(), SourceID: policy.SourceID,
		Kind: run.Kind, MaxChainLength: int(policy.MaxChainLength),
		KeyFile: d.config.Settings.KeyFile, Parallel: int(policy.Parallel),
		Throttle: int(policy.Throttle), PageTracking: policy.PageTracking, Now: d.now,
	}
}

// preflightConnection rebuilds the live MySQL connection and the server
// identity the fresh preflight just recorded. The sealed password is unsealed
// only in memory for the duration of the run; the identity keeps the
// admission request and the preflight on the same live server.
func (d *Dispatcher) preflightConnection(ctx context.Context, policy store.Policy) (physical.Connection, physical.Identity, error) {
	source, err := d.config.Database.Source(ctx, policy.SourceID)
	if err != nil {
		return physical.Connection{}, physical.Identity{}, err
	}
	if source.ServerUUID == nil || source.ServerVersion == nil {
		return physical.Connection{}, physical.Identity{}, errors.New("preflight did not record a server identity")
	}
	settings := d.config.Settings
	if !settings.Credentials() {
		return physical.Connection{}, physical.Identity{}, resources.ErrUnavailable
	}
	password, err := d.config.Database.SourceSecret(ctx, source.ID, settings.Sealer)
	if err != nil {
		return physical.Connection{}, physical.Identity{}, err
	}
	connection := physical.Connection{
		Socket: source.Socket, DataDir: source.Datadir, Username: source.Username, Password: string(password),
	}
	return connection, physical.Identity{UUID: *source.ServerUUID, Version: *source.ServerVersion}, nil
}

// failRun records a failed run with its class and code. It is the single
// failure exit of executeRun.
func (d *Dispatcher) failRun(ctx context.Context, run store.Run, class, code string, cause error) error {
	logger := d.logger
	if cause != nil {
		logger = logger.With(slog.String("cause", cause.Error()))
	}
	log := runLogWrite(eventFailed, map[string]any{"error_class": class, "code": code})
	if err := d.finishWithRetry(ctx, run.ID, func(writeCtx context.Context) error {
		return d.config.Database.FinishRunFailed(writeCtx, run.ID, class, code,
			store.SystemAudit("runs.failed", store.AuditDetail{}), log)
	}); err != nil {
		logger.ErrorContext(ctx, "cannot record failed run", "run_id", run.ID, "record_error", err)
		return err
	}
	logger.ErrorContext(ctx, "run failed", "run_id", run.ID, "class", class, "code", code)
	return nil
}

// cancelRun implements the frozen cancel semantics: the cancel request wins
// only when the physical chain did not succeed. A nil chain.Run keeps the run
// completed; a cancel never deletes a completed backup.
func (d *Dispatcher) cancelRun(ctx context.Context, run store.Run) error {
	log := runLogWrite(eventCanceled, nil)
	if err := d.finishWithRetry(ctx, run.ID, func(writeCtx context.Context) error {
		return d.config.Database.FinishRunCanceled(writeCtx, run.ID,
			store.SystemAudit("runs.canceled", store.AuditDetail{}), log)
	}); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil
		}
		return err
	}
	d.logger.InfoContext(ctx, "run canceled", "run_id", run.ID)
	return nil
}

// enqueueReplication is the handoff: after a local run completes, the
// dispatcher enqueues that backup on the policy's replica repository. The
// replicas table stays the only replication queue.
func (d *Dispatcher) enqueueReplication(ctx context.Context, run store.Run, policy store.Policy, backupID string) {
	if policy.ReplicaRepositoryID == nil {
		return
	}
	replicator := d.ensurePolicyReplicator(ctx, policy, run.ID)
	if replicator == nil {
		return
	}
	if err := replicator.Enqueue(ctx, backupID); err != nil {
		d.logger.ErrorContext(ctx, "cannot enqueue replication", "run_id", run.ID,
			"backup_id", backupID, "repository_id", *policy.ReplicaRepositoryID, "error", err)
		return
	}
	d.logEvent(ctx, run.ID, eventReplicationEnqueued, map[string]any{
		"repository_id": *policy.ReplicaRepositoryID,
	})
}

// RequestCancel asks a run to stop. A queued run becomes canceled without
// running; a running run's context is canceled, and the cancel only wins when
// the physical chain did not succeed.
func (d *Dispatcher) RequestCancel(ctx context.Context, runID string) error {
	run, err := d.config.Database.Run(ctx, runID)
	if err != nil {
		return err
	}
	if run.Status == store.RunStatusQueued {
		changed, err := d.config.Database.CancelQueuedRun(ctx, run.ID,
			store.SystemAudit("runs.canceled", store.AuditDetail{}), runLogWrite(eventCanceled, nil))
		if err != nil {
			return err
		}
		if changed {
			return nil
		}
		// A claim won the race; fall through to the running-run path.
	}
	if err := d.config.Database.RequestCancel(ctx, run.ID,
		store.SystemAudit("runs.cancel_requested", store.AuditDetail{})); err != nil {
		return err
	}
	d.mu.Lock()
	cancel, active := d.runningCancel, d.runningRunID == runID
	d.mu.Unlock()
	if active && cancel != nil {
		cancel()
	}
	return nil
}
