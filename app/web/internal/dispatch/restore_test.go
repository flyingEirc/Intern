package dispatch

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flyingEirc/Intern/app/web/internal/store"
	"github.com/flyingEirc/Intern/app/web/internal/storetest"
)

// fakeRestoreEngine is the restore seam a unit test drives. Every function is
// optional; the zero engine completes immediately with an empty result.
type fakeRestoreEngine struct {
	mu sync.Mutex
	// plan is returned by Plan unless planErr is set.
	plan    store.RestorePlan
	planErr error
	// execute runs the claimed restore. nil completes with a published export.
	execute func(ctx context.Context, req RestoreRequest, progress RestoreProgress) (RestoreResult, error)
	// recover reclaims the exact ids. nil removes the two standard targets.
	recover func(ctx context.Context, ids []string) error
	// planned, executed and recovered record the calls.
	planned   []string
	executed  []RestoreRequest
	recovered [][]string
}

func (e *fakeRestoreEngine) Plan(_ context.Context, backupID string) (store.RestorePlan, error) {
	e.mu.Lock()
	e.planned = append(e.planned, backupID)
	e.mu.Unlock()
	if e.planErr != nil {
		return store.RestorePlan{}, e.planErr
	}
	return e.plan, nil
}

func (e *fakeRestoreEngine) Execute(ctx context.Context, req RestoreRequest, progress RestoreProgress) (RestoreResult, error) {
	e.mu.Lock()
	e.executed = append(e.executed, req)
	execute := e.execute
	e.mu.Unlock()
	if execute != nil {
		return execute(ctx, req, progress)
	}
	return RestoreResult{
		ExportPath: filepath.Join("/export", req.RestoreID), ExportBytes: 42,
		SideEffects:  store.RestoreSideEffects{WorkDirRemoved: true, ExportPublished: true},
		VerifyStatus: store.RestoreVerifyNotRequested,
	}, nil
}

func (e *fakeRestoreEngine) Recover(ctx context.Context, ids []string) error {
	e.mu.Lock()
	e.recovered = append(e.recovered, append([]string(nil), ids...))
	recover := e.recover
	e.mu.Unlock()
	if recover != nil {
		return recover(ctx, ids)
	}
	return nil
}

func (e *fakeRestoreEngine) executedRequests() []RestoreRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]RestoreRequest(nil), e.executed...)
}

// restorePlan is the plan a fake engine returns: one local link, a digest the
// test confirms.
func restorePlan(backupID string) store.RestorePlan {
	return store.RestorePlan{
		BackupID: backupID, ChainID: "chain-1", SourceID: "source-1",
		ServerUUID: "uuid-1", ServerVersion: "8.4.11", ToolVersion: "8.4.0-7",
		TargetToLSN: 500, Links: []store.RestorePlanLink{{
			BackupID: backupID, Seq: 1, Kind: store.RunKindFull, FromLSN: 0, ToLSN: 500,
			Source: "local", RepositoryID: "repository-1", PayloadSize: 100, MetadataSize: 10,
		}},
		RequiredWorkBytes: 200, RequiredExportBytes: 800, AvailableWorkBytes: 1000,
		AvailableExportBytes: 1000, ExportPath: "/export/planned", Risks: []string{},
		Digest: strings.Repeat("a", 64), RehearsalAvailable: true,
	}
}

// restoreUser creates the user a restore row's requested_by references.
func (f *fixture) restoreUser() store.User {
	f.t.Helper()
	user := storetest.SeedAdmin(f.t, f.database, "restore-admin", "hash")
	return user
}

// queueRestore inserts one queued restore row for the fake plan.
func (f *fixture) queueRestore(backupID, userID string, key string) store.Restore {
	f.t.Helper()
	id, err := newRestoreID()
	if err != nil {
		f.t.Fatal(err)
	}
	plan := restorePlan(backupID)
	restore, err := f.database.CreateRestore(f.t.Context(), store.RestoreInput{
		ID: id, BackupID: backupID, Plan: plan, PlanDigest: plan.Digest,
		IdempotencyKey: key, RequestHash: "hash-" + key, RequestedBy: userID,
		Audit: store.SystemAudit("restores.execute", store.AuditDetail{}),
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return restore
}

func (f *fixture) restoreRow(id string) store.Restore {
	f.t.Helper()
	restore, err := f.database.Restore(f.t.Context(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return restore
}

func (f *fixture) restoreLogRows(id string) []store.RestoreLog {
	f.t.Helper()
	logs, err := f.database.RestoreLogsAfter(f.t.Context(), id, 0, store.MaxRestoreLogRows+1)
	if err != nil {
		f.t.Fatal(err)
	}
	return logs
}

// startRestores launches the dispatcher with the fake restore engine on the
// fixture runtime. The caller must have set any runtime directories it needs.
func (f *fixture) startRestores(engine RestoreEngine) *Dispatcher {
	f.t.Helper()
	d, err := Start(f.t.Context(), Config{
		Database: f.database, Settings: f.runtime, Logger: f.logger(), Restore: engine,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = d.Stop() })
	return d
}

// waitForRestoreState polls one restore until it reaches status.
func (f *fixture) waitForRestoreState(id, status string) store.Restore {
	f.t.Helper()
	var last store.Restore
	waitFor(f.t, "restore "+id+" to reach "+status, func() bool {
		last = f.restoreRow(id)
		return last.Status == status
	})
	return last
}

// TestRestoreQueueSharesOneExecutionSlot proves the frozen global concurrency:
// a restore claim is refused while a run is running and the reverse, and both
// refusals are read from the stored rows.
func TestRestoreQueueSharesOneExecutionSlot(t *testing.T) {
	f := newFixture(t)
	user := f.restoreUser()
	repository := f.repository("slot-repository")
	source := f.source("slot-source", testSocketPath(t))
	policy := f.policy("slot-policy", source.ID, repository.ID, false)

	first := f.queueRestore("00000000-0000-7000-8000-000000000001", user.ID, "")
	f.requireClaimedRestore(first.ID)
	run := f.queueRun(policy.ID, source.ID, "slot-run")
	f.requireRunClaimRefused("while a restore was running")

	f.requireRestoreCompleted(first.ID)
	f.requireClaimedRun(run.ID)
	second := f.queueRestore("00000000-0000-7000-8000-000000000002", user.ID, "")
	f.requireRestoreClaimRefused("while a run was running")

	f.requireRunFailed(run.ID)
	f.requireClaimedRestore(second.ID)
}

func (f *fixture) queueRun(policyID, sourceID, key string) store.Run {
	f.t.Helper()
	run, err := f.database.CreateRun(f.t.Context(), store.RunInput{
		PolicyID: policyID, SourceID: sourceID, Kind: store.RunKindFull, Trigger: store.RunTriggerManual,
		IdempotencyKey: key, RequestHash: "slot", ScheduledAt: time.Now(),
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return run
}

func (f *fixture) requireClaimedRestore(id string) {
	f.t.Helper()
	claimed, ok, err := f.database.ClaimRestore(f.t.Context())
	if err != nil || !ok || claimed.ID != id {
		f.t.Fatalf("claim restore = %+v, %v, %v", claimed, ok, err)
	}
}

func (f *fixture) requireClaimedRun(id string) {
	f.t.Helper()
	claimed, ok, err := f.database.ClaimRun(f.t.Context())
	if err != nil || !ok || claimed.ID != id {
		f.t.Fatalf("claim run = %+v, %v, %v", claimed, ok, err)
	}
}

func (f *fixture) requireRunClaimRefused(when string) {
	f.t.Helper()
	_, claimed, err := f.database.ClaimRun(f.t.Context())
	if err != nil || claimed {
		f.t.Fatalf("run claimed %s: %v, %v", when, claimed, err)
	}
}

func (f *fixture) requireRestoreClaimRefused(when string) {
	f.t.Helper()
	_, claimed, err := f.database.ClaimRestore(f.t.Context())
	if err != nil || claimed {
		f.t.Fatalf("restore claimed %s: %v, %v", when, claimed, err)
	}
}

func (f *fixture) requireRestoreCompleted(id string) {
	f.t.Helper()
	err := f.database.FinishRestoreCompleted(f.t.Context(), id, store.RestoreOutcome{
		ExportPath: "/export/first", ExportBytes: 1, VerifyStatus: store.RestoreVerifyNotRequested,
	}, store.SystemAudit("restores.completed", store.AuditDetail{}), nil)
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) requireRunFailed(id string) {
	f.t.Helper()
	err := f.database.FinishRunFailed(f.t.Context(), id, store.RunErrorBackupFailed, "backup_failed",
		store.SystemAudit("runs.failed", store.AuditDetail{}), nil)
	if err != nil {
		f.t.Fatal(err)
	}
}

// TestRestoreWorkerCompletesAndRecordsProgress proves a claimed restore runs
// through the engine, persists its phases and bounded log, and records the
// published export with a system audit.
func TestRestoreWorkerCompletesAndRecordsProgress(t *testing.T) {
	f := newFixture(t)
	f.runtime.ExportRoot = t.TempDir()
	user := f.restoreUser()
	backupID := "00000000-0000-7000-8000-000000000010"
	engine := &fakeRestoreEngine{
		plan: restorePlan(backupID),
		execute: func(_ context.Context, req RestoreRequest, progress RestoreProgress) (RestoreResult, error) {
			if req.RestoreID == "" {
				return RestoreResult{}, errors.New("missing restore id")
			}
			progress("fetching", map[string]any{"seq": 1, "source": "local"})
			progress("preparing", map[string]any{"link": 1})
			progress("exporting", nil)
			return RestoreResult{
				ExportPath: filepath.Join(f.runtime.ExportRoot, req.RestoreID), ExportBytes: 4096,
				SideEffects:  store.RestoreSideEffects{WorkDirRemoved: true, PendingExportRemoved: true, ExportPublished: true},
				VerifyStatus: store.RestoreVerifyPassed,
				VerifyResult: map[string]any{"status": "passed", "schemas": []any{}},
			}, nil
		},
	}
	f.startRestores(engine)
	done := f.waitForRestoreState(f.queueRestore(backupID, user.ID, "").ID, store.RestoreStatusCompleted)
	requireCompletedRestoreRow(t, f, done)
	requireRestorePhasesLogged(t, f, done.ID, []string{"fetching", "preparing", "exporting"})
	for _, action := range []string{"restores.claimed", "restores.completed"} {
		if count := f.systemAuditCount(action); count == 0 {
			t.Fatalf("missing %s system audit", action)
		}
	}
}

func requireCompletedRestoreRow(t *testing.T, f *fixture, done store.Restore) {
	t.Helper()
	export := filepath.Join(f.runtime.ExportRoot, done.ID)
	work := filepath.Join(f.runtime.WorkRoot, "restore-"+done.ID)
	if done.ExportPath == nil || *done.ExportPath != export {
		t.Fatalf("export_path = %v", done.ExportPath)
	}
	if done.WorkDir == nil || *done.WorkDir != work {
		t.Fatalf("work_dir = %v", done.WorkDir)
	}
	requireCompletedRestoreOutcome(t, done)
}

// requireCompletedRestoreOutcome checks the sizes, phase, rehearsal fields and
// cleanup observation a completed restore must carry.
func requireCompletedRestoreOutcome(t *testing.T, done store.Restore) {
	t.Helper()
	if done.ExportBytes == nil || *done.ExportBytes != 4096 {
		t.Fatalf("export_bytes = %v", done.ExportBytes)
	}
	if done.Phase != nil {
		t.Fatalf("completed restore kept phase %q", *done.Phase)
	}
	if done.VerifyStatus != store.RestoreVerifyPassed || done.VerifyResult == nil {
		t.Fatalf("verify = %q, %v", done.VerifyStatus, done.VerifyResult)
	}
	if done.SideEffects == nil || !done.SideEffects.ExportPublished {
		t.Fatalf("side effects = %+v", done.SideEffects)
	}
}

// requireRestorePhasesLogged reads the bounded restore log back and requires
// every phase the engine reported to be present.
func requireRestorePhasesLogged(t *testing.T, f *fixture, id string, want []string) {
	t.Helper()
	seen := map[string]bool{}
	for _, row := range f.restoreLogRows(id) {
		if phase, found := strings.CutPrefix(row.Message, "restore phase "); found {
			seen[phase] = true
		}
	}
	for _, phase := range want {
		if !seen[phase] {
			t.Fatalf("missing phase log %q in %+v", phase, seen)
		}
	}
}

// TestRestoreCancelQueued proves a queued restore is canceled without reaching
// the engine.
func TestRestoreCancelQueued(t *testing.T) {
	f := newFixture(t)
	user := f.restoreUser()
	engine := &fakeRestoreEngine{}
	// A dispatcher that is not started: the cancel path must not need a worker.
	d := f.newDispatcher(nil)
	d.config.Restore = engine
	queued := f.queueRestore("00000000-0000-7000-8000-000000000020", user.ID, "")
	if err := d.RequestRestoreCancel(t.Context(), queued.ID); err != nil {
		t.Fatal(err)
	}
	canceled := f.restoreRow(queued.ID)
	if canceled.Status != store.RestoreStatusCanceled {
		t.Fatalf("status = %s", canceled.Status)
	}
	if canceled.SideEffects == nil || *canceled.SideEffects != (store.RestoreSideEffects{}) {
		t.Fatalf("side_effects = %v, want the explicit all-absent observation", canceled.SideEffects)
	}
	if len(engine.executedRequests()) != 0 {
		t.Fatal("engine executed a canceled restore")
	}
	if count := f.systemAuditCount("restores.canceled"); count == 0 {
		t.Fatal("missing restores.canceled system audit")
	}
	if err := d.RequestRestoreCancel(t.Context(), queued.ID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second cancel = %v, want ErrConflict", err)
	}
}

// TestRestoreCancelRunning proves a running restore's context is canceled and
// the engine's cleanup observation is persisted with the canceled status.
func TestRestoreCancelRunning(t *testing.T) {
	f := newFixture(t)
	f.runtime.ExportRoot = t.TempDir()
	user := f.restoreUser()
	started := make(chan struct{})
	engine := &fakeRestoreEngine{
		plan: restorePlan("00000000-0000-7000-8000-000000000030"),
		execute: func(ctx context.Context, req RestoreRequest, _ RestoreProgress) (RestoreResult, error) {
			close(started)
			<-ctx.Done()
			return RestoreResult{}, &RestoreExecError{
				Code: "canceled", Phase: "preparing",
				SideEffects: store.RestoreSideEffects{WorkDirRemoved: true},
			}
		},
	}
	d := f.startRestores(engine)
	queued := f.queueRestore("00000000-0000-7000-8000-000000000030", user.ID, "")
	f.waitForRestoreState(queued.ID, store.RestoreStatusRunning)
	<-started
	if err := d.RequestRestoreCancel(t.Context(), queued.ID); err != nil {
		t.Fatal(err)
	}
	canceled := f.waitForRestoreState(queued.ID, store.RestoreStatusCanceled)
	if canceled.ErrorClass == nil || *canceled.ErrorClass != store.RestoreErrorCanceled {
		t.Fatalf("error_class = %v", canceled.ErrorClass)
	}
	if canceled.ErrorCode == nil || *canceled.ErrorCode != "canceled" {
		t.Fatalf("error_code = %v", canceled.ErrorCode)
	}
	if canceled.SideEffects == nil || !canceled.SideEffects.WorkDirRemoved {
		t.Fatalf("side_effects = %+v", canceled.SideEffects)
	}
}

// TestRestoreExecutionFailureClasses pins the closed error_class set the API
// documents: an engine execution failure is restore_failed with its own code,
// a plan refusal that only surfaces at execution is refused (the catalog moved
// after the digest was confirmed), and an untyped failure keeps a stable
// generic code instead of a blank one.
func TestRestoreExecutionFailureClasses(t *testing.T) {
	cases := []struct {
		name     string
		backupID string
		failure  error
		class    string
		code     string
	}{
		{
			"exec", "00000000-0000-7000-8000-000000000051",
			&RestoreExecError{
				Code: "prepare_failed", Phase: "preparing",
				SideEffects: store.RestoreSideEffects{WorkDirRemoved: true, PendingExportRemoved: true},
			},
			store.RestoreErrorFailed, "prepare_failed",
		},
		{
			"plan", "00000000-0000-7000-8000-000000000052",
			&RestorePlanError{Code: RestoreCodePlanChanged}, store.RestoreErrorRefused, RestoreCodePlanChanged,
		},
		{
			"other", "00000000-0000-7000-8000-000000000053",
			errors.New("disk went away"), store.RestoreErrorFailed, "work_failed",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)
			f.runtime.ExportRoot = t.TempDir()
			user := f.restoreUser()
			engine := &fakeRestoreEngine{
				plan: restorePlan(test.backupID),
				execute: func(context.Context, RestoreRequest, RestoreProgress) (RestoreResult, error) {
					return RestoreResult{}, test.failure
				},
			}
			f.startRestores(engine)
			queued := f.queueRestore(test.backupID, user.ID, "")
			failed := f.waitForRestoreState(queued.ID, store.RestoreStatusFailed)
			if failed.ErrorClass == nil || *failed.ErrorClass != test.class {
				t.Fatalf("error_class = %v, want %s", failed.ErrorClass, test.class)
			}
			if failed.ErrorCode == nil || *failed.ErrorCode != test.code {
				t.Fatalf("error_code = %v, want %s", failed.ErrorCode, test.code)
			}
			if failed.SideEffects == nil {
				t.Fatal("side_effects are missing on a failed restore")
			}
			if failed.SideEffects.ExportPublished {
				t.Fatalf("a failed restore reported a published export: %+v", failed.SideEffects)
			}
		})
	}
}

// TestRestorePublishWinsOverCancel proves a restore whose export was published
// stays completed even when a cancel request arrived first.
func TestRestorePublishWinsOverCancel(t *testing.T) {
	f := newFixture(t)
	f.runtime.ExportRoot = t.TempDir()
	user := f.restoreUser()
	started := make(chan struct{})
	engine := &fakeRestoreEngine{
		plan: restorePlan("00000000-0000-7000-8000-000000000040"),
		execute: func(ctx context.Context, req RestoreRequest, _ RestoreProgress) (RestoreResult, error) {
			close(started)
			<-ctx.Done()
			return RestoreResult{
				ExportPath: filepath.Join(f.runtime.ExportRoot, req.RestoreID), ExportBytes: 7,
				SideEffects:  store.RestoreSideEffects{WorkDirRemoved: true, ExportPublished: true},
				VerifyStatus: store.RestoreVerifyNotRequested,
			}, nil
		},
	}
	d := f.startRestores(engine)
	queued := f.queueRestore("00000000-0000-7000-8000-000000000040", user.ID, "")
	f.waitForRestoreState(queued.ID, store.RestoreStatusRunning)
	<-started
	if err := d.RequestRestoreCancel(t.Context(), queued.ID); err != nil {
		t.Fatal(err)
	}
	completed := f.waitForRestoreState(queued.ID, store.RestoreStatusCompleted)
	if completed.SideEffects == nil || !completed.SideEffects.ExportPublished {
		t.Fatalf("side_effects = %+v", completed.SideEffects)
	}
	if !completed.CancelRequested {
		t.Fatal("the cancel request flag was lost")
	}
}

// TestRestoreInterruptedRecovery proves startup recovery fails a running
// restore, reclaims only its exact targets, persists the side effects and
// leaves a published export and another restore's directories untouched.
func TestRestoreInterruptedRecovery(t *testing.T) {
	f := newFixture(t)
	f.runtime.ExportRoot = t.TempDir()
	user := f.restoreUser()
	interrupted := f.queueRestore("00000000-0000-7000-8000-000000000050", user.ID, "")
	f.requireClaimedRestore(interrupted.ID)
	targets := f.createRestoreTargets(interrupted.ID)

	f.startRestores(&fakeRestoreEngine{recover: func(_ context.Context, ids []string) error {
		for _, id := range ids {
			_ = os.RemoveAll(filepath.Join(f.runtime.WorkRoot, "restore-"+id))
			_ = os.RemoveAll(filepath.Join(f.runtime.ExportRoot, ".pending-"+id))
		}
		return nil
	}})

	failed := f.waitForRestoreState(interrupted.ID, store.RestoreStatusFailed)
	requireInterruptedOutcome(t, failed)
	requireReclaimScope(t, f, targets)
	if count := f.systemAuditCount("restores.interrupted"); count == 0 {
		t.Fatal("missing restores.interrupted system audit")
	}
}

// restoreTargets are the directories one interrupted restore may own, plus the
// neighbour's directories that must survive.
type restoreTargets struct {
	work, pending, published, otherWork, otherPending string
}

func (f *fixture) createRestoreTargets(id string) restoreTargets {
	f.t.Helper()
	targets := restoreTargets{
		work:         filepath.Join(f.runtime.WorkRoot, "restore-"+id),
		pending:      filepath.Join(f.runtime.ExportRoot, ".pending-"+id),
		published:    filepath.Join(f.runtime.ExportRoot, id),
		otherWork:    filepath.Join(f.runtime.WorkRoot, "restore-other"),
		otherPending: filepath.Join(f.runtime.ExportRoot, ".pending-other"),
	}
	for _, dir := range []string{targets.work, targets.pending, targets.published, targets.otherWork, targets.otherPending} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			f.t.Fatal(err)
		}
	}
	return targets
}

func requireInterruptedOutcome(t *testing.T, failed store.Restore) {
	t.Helper()
	if failed.ErrorClass == nil || *failed.ErrorClass != store.RestoreErrorInterrupted {
		t.Fatalf("error_class = %v", failed.ErrorClass)
	}
	effects := failed.SideEffects
	if effects == nil || !effects.WorkDirRemoved || !effects.PendingExportRemoved || !effects.ExportPublished {
		t.Fatalf("side_effects = %+v", effects)
	}
}

func requireReclaimScope(t *testing.T, f *fixture, targets restoreTargets) {
	t.Helper()
	if pathExists(targets.work) || pathExists(targets.pending) {
		t.Fatal("exact targets survived recovery")
	}
	if !pathExists(targets.published) {
		t.Fatal("recovery removed a published export")
	}
	if !pathExists(targets.otherWork) || !pathExists(targets.otherPending) {
		t.Fatal("recovery touched another restore's directories")
	}
}

// TestRestoreExecuteIdempotencyAndRefusals proves the execute contract: a
// replayed key returns the same id, a changed request refuses, a stale digest
// is plan_changed, a rehearsal without mysqld is validation, and a refused plan
// maps onto the frozen outcome classes.
func TestRestoreExecuteIdempotencyAndRefusals(t *testing.T) {
	f := newFixture(t)
	user := f.restoreUser()
	plan := restorePlan("00000000-0000-7000-8000-000000000060")
	engine := &fakeRestoreEngine{plan: plan}
	d := f.newDispatcher(nil)
	d.config.Restore = engine
	request := RestoreExecute{
		BackupID: plan.BackupID, PlanDigest: plan.Digest, IdempotencyKey: "restore-once",
		RequestHash: MarshalRestoreRequest(plan.BackupID, plan.Digest, false, nil), ActorID: user.ID,
		Audit: store.SystemAudit("restores.execute", store.AuditDetail{}),
	}
	created := requireRestoreCreated(t, d, request)
	requireRestoreReplayed(t, d, request, created)
	requireRestoreKeyConflict(t, d, request)
	requireRestoreStaleDigest(t, d, request)
	requireRestoreRehearsalRefused(t, d, engine, request)
	requireRestorePlanErrorMapping(t, d, engine, request)
}

func requireRestoreCreated(t *testing.T, d *Dispatcher, request RestoreExecute) string {
	t.Helper()
	outcome, decision := d.ExecuteRestore(t.Context(), request)
	if outcome != RestoreCreated || decision.RestoreID == "" {
		t.Fatalf("create = %v, %+v", outcome, decision)
	}
	if err := requireRestoreID(decision.RestoreID); err != nil {
		t.Fatalf("restore id %q is not 32 hex: %v", decision.RestoreID, err)
	}
	return decision.RestoreID
}

func requireRestoreReplayed(t *testing.T, d *Dispatcher, request RestoreExecute, created string) {
	t.Helper()
	outcome, decision := d.ExecuteRestore(t.Context(), request)
	if outcome != RestoreReplayed || decision.RestoreID != created {
		t.Fatalf("replay = %v, %+v", outcome, decision)
	}
}

func requireRestoreKeyConflict(t *testing.T, d *Dispatcher, request RestoreExecute) {
	t.Helper()
	changed := request
	changed.RequestHash = MarshalRestoreRequest(request.BackupID, request.PlanDigest, true, nil)
	if outcome, decision := d.ExecuteRestore(t.Context(), changed); outcome != RestoreRefused || decision.Code != "conflict" {
		t.Fatalf("changed request = %v, %+v", outcome, decision)
	}
}

func requireRestoreStaleDigest(t *testing.T, d *Dispatcher, request RestoreExecute) {
	t.Helper()
	stale := request
	stale.IdempotencyKey = ""
	stale.PlanDigest = strings.Repeat("b", 64)
	if outcome, decision := d.ExecuteRestore(t.Context(), stale); outcome != RestorePlanChanged || decision.Code != RestoreCodePlanChanged {
		t.Fatalf("stale digest = %v, %+v", outcome, decision)
	}
}

func requireRestoreRehearsalRefused(t *testing.T, d *Dispatcher, engine *fakeRestoreEngine, request RestoreExecute) {
	t.Helper()
	withoutRehearsal := restorePlan(request.BackupID)
	withoutRehearsal.RehearsalAvailable = false
	withoutRehearsal.Digest = request.PlanDigest
	engine.plan = withoutRehearsal
	rehearse := request
	rehearse.IdempotencyKey = ""
	rehearse.Rehearse = true
	if outcome, decision := d.ExecuteRestore(t.Context(), rehearse); outcome != RestoreValidation || decision.Code != "rehearsal_unavailable" {
		t.Fatalf("rehearsal = %v, %+v", outcome, decision)
	}
}

// requireRestorePlanErrorMapping drives every refusal code through both the
// execute and the plan path.
func requireRestorePlanErrorMapping(t *testing.T, d *Dispatcher, engine *fakeRestoreEngine, request RestoreExecute) {
	t.Helper()
	cases := []struct {
		code    string
		outcome RestoreOutcome
	}{
		{RestoreCodeUnconfigured, RestoreUnavailable},
		{RestoreCodeBackupNotFound, RestoreNotFound},
		{RestoreCodeMatrixUnsupported, RestoreRejected},
		{"chain_broken", RestoreRefused},
	}
	for _, test := range cases {
		engine.planErr = &RestorePlanError{Code: test.code, BackupID: request.BackupID}
		probe := request
		probe.IdempotencyKey = ""
		if outcome, _ := d.ExecuteRestore(t.Context(), probe); outcome != test.outcome {
			t.Fatalf("plan error %s = %v, want %v", test.code, outcome, test.outcome)
		}
		var planErr *RestorePlanError
		if _, err := d.PlanRestore(t.Context(), request.BackupID); !errors.As(err, &planErr) {
			t.Fatalf("PlanRestore error %s = %v", test.code, err)
		}
	}
	engine.planErr = nil
}

// TestRestoreProgressDropsUnvettedData proves the persisted progress contains
// only allowlisted scalar values: an engine map is not a trusted wire format.
func TestRestoreProgressDropsUnvettedData(t *testing.T) {
	f := newFixture(t)
	f.runtime.ExportRoot = t.TempDir()
	user := f.restoreUser()
	secret := "{{SECRET_restore_progress}}"
	engine := &fakeRestoreEngine{
		plan: restorePlan("00000000-0000-7000-8000-000000000070"),
		execute: func(_ context.Context, req RestoreRequest, progress RestoreProgress) (RestoreResult, error) {
			progress("preparing", map[string]any{
				"seq": 1, "password": secret, "status": secret, "nested": map[string]any{"token": secret},
			})
			return RestoreResult{
				ExportPath: "/export/ok", ExportBytes: 1,
				SideEffects:  store.RestoreSideEffects{ExportPublished: true},
				VerifyStatus: store.RestoreVerifyNotRequested,
			}, nil
		},
	}
	f.startRestores(engine)
	queued := f.queueRestore("00000000-0000-7000-8000-000000000070", user.ID, "")
	f.waitForRestoreState(queued.ID, store.RestoreStatusCompleted)
	for _, row := range f.restoreLogRows(queued.ID) {
		encoded, err := json.Marshal(row.Data)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("secret reached restore log: %s", encoded)
		}
	}
}

// requireRestoreID accepts a 32 lowercase hex restore id and reports anything
// else.
func requireRestoreID(value string) error {
	if len(value) != 32 {
		return errors.New("wrong length")
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return errors.New("not lowercase hex")
		}
	}
	return nil
}

// TestRestoreIDsAreTimeOrdered proves the id contract the API documents: 32
// lowercase hex bytes of a UUIDv7, so id order is creation order and the
// descending-id page starts with the newest restore instead of a random one.
func TestRestoreIDsAreTimeOrdered(t *testing.T) {
	f := newFixture(t)
	user := f.restoreUser()
	backupID := "00000000-0000-7000-8000-000000000070"
	ids := make([]string, 0, 6)
	for index := range 6 {
		id, err := newRestoreID()
		if err != nil {
			t.Fatal(err)
		}
		if err = requireRestoreID(id); err != nil {
			t.Fatalf("restore id %q: %v", id, err)
		}
		plan := restorePlan(backupID)
		if _, err = f.database.CreateRestore(f.t.Context(), store.RestoreInput{
			ID: id, BackupID: backupID, Plan: plan, PlanDigest: plan.Digest,
			IdempotencyKey: "ordered-" + strconv.Itoa(index), RequestHash: "hash-ordered-" + strconv.Itoa(index),
			RequestedBy: user.ID, Audit: store.SystemAudit("restores.execute", store.AuditDetail{}),
		}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		// One millisecond is the UUIDv7 clock unit: a later id must sort after
		// every earlier one.
		time.Sleep(2 * time.Millisecond)
	}
	page, err := f.database.Restores(f.t.Context(), store.RestoresFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != len(ids) {
		t.Fatalf("page has %d rows, want %d", len(page), len(ids))
	}
	for index, restore := range page {
		want := ids[len(ids)-1-index]
		if restore.ID != want {
			t.Fatalf("position %d = %s, want the newest %s", index, restore.ID, want)
		}
	}
}

// TestRestoreCancelLandedBeforeContextRegistration proves the narrow window the
// worker used to lose: the request arrived after the claim committed and before
// any cancelable context existed, so nothing could act on it. Re-reading
// cancel_requested once the context is registered must still cancel the restore
// instead of running it to completion.
func TestRestoreCancelLandedBeforeContextRegistration(t *testing.T) {
	f := newFixture(t)
	f.runtime.ExportRoot = t.TempDir()
	user := f.restoreUser()
	backupID := "00000000-0000-7000-8000-000000000071"
	engine := &fakeRestoreEngine{
		plan:    restorePlan(backupID),
		execute: publishUnlessCanceled(f.runtime.ExportRoot),
	}
	// A dispatcher that is not started: only this test claims and executes, so
	// the window is reproduced exactly rather than raced for.
	d := f.newDispatcher(nil)
	d.config.Restore = engine
	f.queueRestore(backupID, user.ID, "")
	restore, claimed, err := f.database.ClaimRestore(f.t.Context())
	if err != nil || !claimed {
		t.Fatalf("claimed = %v, err = %v", claimed, err)
	}
	if err = d.RequestRestoreCancel(f.t.Context(), restore.ID); err != nil {
		t.Fatal(err)
	}
	if !d.restoreCancelRequested(f.t.Context(), restore.ID) {
		t.Fatal("the cancel request did not reach the stored row")
	}
	if err = d.executeRestore(f.t.Context(), restore, ""); err != nil {
		t.Fatal(err)
	}
	requireCanceledInWindow(t, f.restoreRow(restore.ID))
}

// publishUnlessCanceled is the fake engine of the cancel window: a context that
// is already canceled when the engine starts means the request was honored, and
// anything else publishes an export, which is the bug this case refuses.
func publishUnlessCanceled(exportRoot string) func(context.Context, RestoreRequest, RestoreProgress) (RestoreResult, error) {
	return func(ctx context.Context, req RestoreRequest, _ RestoreProgress) (RestoreResult, error) {
		if ctx.Err() != nil {
			return RestoreResult{}, &RestoreExecError{Code: "canceled", Phase: "fetching"}
		}
		return RestoreResult{
			ExportPath: filepath.Join(exportRoot, req.RestoreID), ExportBytes: 42,
			SideEffects: store.RestoreSideEffects{ExportPublished: true},
		}, nil
	}
}

// requireCanceledInWindow requires the row the window produced: canceled, with
// the engine's own code and no export.
func requireCanceledInWindow(t *testing.T, canceled store.Restore) {
	t.Helper()
	if canceled.Status != store.RestoreStatusCanceled {
		t.Fatalf("status = %s, want canceled: the request was lost", canceled.Status)
	}
	if canceled.ExportPath != nil {
		t.Fatalf("a canceled restore published an export: %v", canceled.ExportPath)
	}
	if canceled.ErrorCode == nil || *canceled.ErrorCode != "canceled" {
		t.Fatalf("error_code = %v", canceled.ErrorCode)
	}
}

// TestRestoreRehearsalWithoutResultIsNotRun proves a row that asked for a
// rehearsal never claims none was wanted: an execution that ended before a
// rehearsal result existed is not_run, while a row without a rehearsal stays
// not_requested.
func TestRestoreRehearsalWithoutResultIsNotRun(t *testing.T) {
	if status, _ := restoreVerify(nil, true); status != store.RestoreVerifyNotRun {
		t.Fatalf("a rehearsal without a result = %s, want not_run", status)
	}
	if status, _ := restoreVerify(nil, false); status != store.RestoreVerifyNotRequested {
		t.Fatalf("no rehearsal = %s, want not_requested", status)
	}
	f := newFixture(t)
	f.runtime.ExportRoot = t.TempDir()
	user := f.restoreUser()
	backupID := "00000000-0000-7000-8000-0000000000d1"
	engine := &fakeRestoreEngine{
		plan: restorePlan(backupID),
		execute: func(context.Context, RestoreRequest, RestoreProgress) (RestoreResult, error) {
			return RestoreResult{}, errors.New("disk went away")
		},
	}
	f.startRestores(engine)
	plan := restorePlan(backupID)
	queued, err := f.database.CreateRestore(f.t.Context(), store.RestoreInput{
		ID: "ccccccccccccccccccccccccccccccc1", BackupID: backupID, Plan: plan, PlanDigest: plan.Digest,
		Rehearse: true, RequestHash: "hash-rehearse", RequestedBy: user.ID,
		Audit: store.SystemAudit("restores.execute", store.AuditDetail{}),
	})
	if err != nil {
		t.Fatal(err)
	}
	failed := f.waitForRestoreState(queued.ID, store.RestoreStatusFailed)
	if failed.VerifyStatus != store.RestoreVerifyNotRun {
		t.Fatalf("a failed rehearsal row reports %s, want not_run", failed.VerifyStatus)
	}
}
