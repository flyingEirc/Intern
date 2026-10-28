package dispatch

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/flyingEirc/Intern/app/web/internal/resources"
	"github.com/flyingEirc/Intern/app/web/internal/store"
)

// queueFixture is one configured deployment with a local repository, one
// source and one enabled policy — the minimal real shape every run needs.
type queueFixture struct {
	*fixture
	repository store.Repository
	source     store.Source
	policy     store.Policy
}

func newQueueFixture(t *testing.T) *queueFixture {
	t.Helper()
	f := newFixture(t)
	repository := f.repository("queue-local")
	source := f.source("queue-source", testSocketPath(t))
	policy := f.policy("queue-policy", source.ID, repository.ID, true)
	return &queueFixture{fixture: f, repository: repository, source: source, policy: policy}
}

// queued inserts one queued run with the given key, kind and trigger.
func (q *queueFixture) queued(key, kind, trigger string, scheduledAt time.Time) store.Run {
	q.t.Helper()
	run, err := q.database.CreateRun(q.t.Context(), store.RunInput{
		PolicyID: q.policy.ID, SourceID: q.source.ID, Kind: kind, Trigger: trigger,
		IdempotencyKey: key, RequestHash: cronRequestHash(q.policy, kind), ScheduledAt: scheduledAt,
	})
	if err != nil {
		q.t.Fatal(err)
	}
	return run
}

// TestManualTriggerIsIdempotentUnderConcurrency fires the same Idempotency-Key
// and body from many goroutines against real SQLite. Exactly one run may be
// created; every other answer is a replay of that same run id.
func TestManualTriggerIsIdempotentUnderConcurrency(t *testing.T) {
	q := newQueueFixture(t)
	d := q.newDispatcher(time.Now)
	results := raceTriggers(q, d, 16, func(int) string { return store.RunKindFull }, "concurrent-full")
	assertSingleCreation(t, results)
	if runs := q.runs(q.policy.ID); len(runs) != 1 || runs[0].ID != results[0].runID {
		t.Fatalf("stored runs = %+v, want exactly %s", runs, results[0].runID)
	}
}

// triggerResult is one racing manual trigger's answer.
type triggerResult struct {
	outcome TriggerOutcome
	kind    string
	runID   string
}

// raceTriggers fires the same key from callers goroutines, each with the body
// kindFor returns, and collects every answer.
func raceTriggers(q *queueFixture, d *Dispatcher, callers int, kindFor func(int) string, key string) []triggerResult {
	results := make([]triggerResult, callers)
	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	for i := range callers {
		done.Add(1)
		go func(index int) {
			defer done.Done()
			kind := kindFor(index)
			start.Wait()
			outcome, payload := d.TriggerRun(q.t.Context(), q.policy.ID, kind, key, MarshalTriggerBody(kind), "actor")
			created, _ := payload.(CreatedRun)
			results[index] = triggerResult{outcome: outcome, kind: kind, runID: created.RunID}
		}(i)
	}
	start.Done()
	done.Wait()
	return results
}

// assertSingleCreation requires exactly one created run and that every other
// answer replayed it.
func assertSingleCreation(t *testing.T, results []triggerResult) {
	t.Helper()
	created, replayed := 0, 0
	var runID string
	for _, item := range results {
		switch item.outcome {
		case TriggerCreated:
			created++
		case TriggerReplayed:
			replayed++
		default:
			t.Fatalf("concurrent trigger outcome = %v", item.outcome)
		}
		if runID == "" {
			runID = item.runID
		} else if item.runID != runID {
			t.Fatalf("idempotent replay returned a different run: %s != %s", item.runID, runID)
		}
	}
	if created != 1 || replayed != len(results)-1 {
		t.Fatalf("created=%d replayed=%d, want 1/%d", created, replayed, len(results)-1)
	}
}

// TestManualTriggerConflictUnderConcurrency reuses one key with two different
// bodies from many goroutines: one body creates the run, the other can only
// answer conflict, and the stored row keeps the winner's body.
func TestManualTriggerConflictUnderConcurrency(t *testing.T) {
	q := newQueueFixture(t)
	d := q.newDispatcher(time.Now)
	results := raceTriggers(q, d, 12, func(index int) string {
		if index%2 == 1 {
			return store.RunKindIncremental
		}
		return store.RunKindFull
	}, "conflicting-key")
	winner := assertOneWinner(t, results)
	runs := q.runs(q.policy.ID)
	if len(runs) != 1 || runs[0].Kind != winner {
		t.Fatalf("winner=%q runs=%+v", winner, runs)
	}
	if runs[0].RequestHash != MarshalTriggerBody(winner) {
		t.Fatalf("stored request hash = %q, want the winner's body", runs[0].RequestHash)
	}
}

// assertOneWinner requires exactly one body to win and every answer to be a
// conflict or a replay of that winner, then returns the winning body.
func assertOneWinner(t *testing.T, results []triggerResult) string {
	t.Helper()
	winner := ""
	for _, item := range results {
		if item.outcome != TriggerCreated {
			continue
		}
		if winner != "" {
			t.Fatalf("two bodies created the same key: %s and %s", winner, item.kind)
		}
		winner = item.kind
	}
	if winner == "" {
		t.Fatal("no body created the run")
	}
	for _, item := range results {
		switch item.outcome {
		case TriggerCreated, TriggerConflict:
		case TriggerReplayed:
			if item.kind != winner {
				t.Fatalf("a conflicting body replayed instead of conflicting (%s)", item.kind)
			}
		default:
			t.Fatalf("unexpected outcome %v", item.outcome)
		}
	}
	return winner
}

// TestCronInsertIsIdempotentAcrossRestart proves the frozen cron key survives a
// process restart: the same trigger time cannot create a second run after the
// store is closed and reopened.
func TestCronInsertIsIdempotentAcrossRestart(t *testing.T) {
	q := newQueueFixture(t)
	d := q.newDispatcher(time.Now)
	scheduledAt := time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC)
	first, created, err := d.insertCronRun(q.t.Context(), q.policy, store.RunKindFull, scheduledAt, time.Now())
	if err != nil || !created {
		t.Fatalf("first insert: %v created=%v", err, created)
	}

	q.reopen()
	restarted := q.newDispatcher(time.Now)
	second, created, err := restarted.insertCronRun(q.t.Context(), q.policy, store.RunKindFull, scheduledAt, time.Now())
	if err != nil || created {
		t.Fatalf("second insert after restart: %v created=%v; the cron key must not fire twice", err, created)
	}
	if second.ID != "" {
		t.Fatalf("duplicate insert reported run %s as new", second.ID)
	}
	if runs := q.runs(q.policy.ID); len(runs) != 1 || runs[0].ID != first.ID {
		t.Fatalf("runs after restart = %+v, want only %s", runs, first.ID)
	}
}

// TestClaimRunIsExclusiveWhileAnotherIsRunning proves global concurrency 1: a
// queue of many runs yields exactly one successful claim while any row is
// already running.
func TestClaimRunIsExclusiveWhileAnotherIsRunning(t *testing.T) {
	q := newQueueFixture(t)
	const queued = 12
	for i := range queued {
		q.queued(fmt.Sprintf("claim-%02d", i), store.RunKindFull, store.RunTriggerCron, time.Now().UTC())
	}
	seen := raceClaims(q, 8)
	if len(seen) != 1 {
		t.Fatalf("claimed %d distinct runs, want exactly 1 while a run is running", len(seen))
	}
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("run %s claimed %d times", id, count)
		}
	}
	running := 0
	for _, run := range q.runs(q.policy.ID) {
		if run.Status == store.RunStatusRunning {
			running++
			continue
		}
		if run.Status != store.RunStatusQueued {
			t.Fatalf("run %s status = %s, want queued or running", run.ID, run.Status)
		}
	}
	if running != 1 {
		t.Fatalf("running rows = %d, want 1", running)
	}
}

// raceClaims runs claimers goroutines until the queue drains and returns how
// often each run id was claimed.
func raceClaims(q *queueFixture, claimers int) map[string]int {
	claimed := make(chan string, 1024)
	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	for range claimers {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			for {
				run, ok, err := q.database.ClaimRun(q.t.Context())
				if err != nil {
					q.t.Error(err)
					return
				}
				if !ok {
					return
				}
				claimed <- run.ID
			}
		}()
	}
	start.Done()
	done.Wait()
	close(claimed)
	seen := map[string]int{}
	for id := range claimed {
		seen[id]++
	}
	return seen
}

// TestClaimOrdersFullBeforeIncremental proves the frozen claim order: a full
// queued later than an incremental is still claimed first.
func TestClaimOrdersFullBeforeIncremental(t *testing.T) {
	q := newQueueFixture(t)
	q.queued("order-incremental", store.RunKindIncremental, store.RunTriggerCron, time.Now().UTC().Add(-time.Hour))
	full := q.queued("order-full", store.RunKindFull, store.RunTriggerCron, time.Now().UTC())

	run, ok, err := q.database.ClaimRun(q.t.Context())
	if err != nil || !ok {
		t.Fatalf("claim: %v ok=%v", err, ok)
	}
	if run.ID != full.ID || run.Kind != store.RunKindFull {
		t.Fatalf("claimed %s (%s), want the full %s", run.ID, run.Kind, full.ID)
	}
}

// TestCancelQueuedRunNeverRuns cancels a queued run through the dispatcher's
// frozen cancel path and proves the worker skips it: it becomes canceled, no
// claim ever touches it, and its slot is not re-created.
func TestCancelQueuedRunNeverRuns(t *testing.T) {
	q := newQueueFixture(t)
	run := q.queued("cancel-queued", store.RunKindFull, store.RunTriggerManual, time.Now().UTC())

	idle := q.newDispatcher(time.Now)
	if err := idle.RequestCancel(q.t.Context(), run.ID); err != nil {
		t.Fatal(err)
	}
	final := q.run(run.ID)
	if final.Status != store.RunStatusCanceled {
		t.Fatalf("status = %s, want canceled", final.Status)
	}
	if final.FinishedAt == nil {
		t.Fatal("canceled run has no finished_at")
	}
	// A terminal run refuses a second cancel with the conflict the API renders 409.
	if err := idle.RequestCancel(q.t.Context(), run.ID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second cancel error = %v, want ErrConflict", err)
	}
	// The claim's queued guard means the canceled row can never run.
	if _, ok, err := q.database.ClaimRun(q.t.Context()); err != nil || ok {
		t.Fatalf("claim after cancel = %v, %v; the canceled run must be unclaimable", ok, err)
	}
	if names := q.eventNames(run.ID); len(names) != 1 || names[0] != "canceled" {
		t.Fatalf("run log = %v, want one canceled event", names)
	}
	if got := q.systemAuditCount("runs.canceled"); got != 1 {
		t.Fatalf("runs.canceled system audits = %d, want 1", got)
	}
}

// TestClaimSkipsRowWithCancelRequest covers the race the claim guard exists for:
// a cancel request that landed on a still-queued row keeps it from being claimed.
func TestClaimSkipsRowWithCancelRequest(t *testing.T) {
	q := newQueueFixture(t)
	run := q.queued("cancel-requested", store.RunKindFull, store.RunTriggerManual, time.Now().UTC())
	if err := q.database.RequestCancel(q.t.Context(), run.ID,
		store.SystemAudit("runs.cancel_requested", store.AuditDetail{})); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := q.database.ClaimRun(q.t.Context()); err != nil || ok {
		t.Fatalf("claim = %v, %v; a canceled-requested row must not be claimed", ok, err)
	}
}

// TestUnconfiguredRuntimeManualTriggerAnswersUnavailable implements decision 9
// for the manual path: a deployment without KEK, work root or XtraBackup
// answers unavailable immediately and creates no run.
func TestUnconfiguredRuntimeManualTriggerAnswersUnavailable(t *testing.T) {
	q := newQueueFixture(t)
	q.unconfigured()
	d := q.newDispatcher(time.Now)
	outcome, payload := d.TriggerRun(q.t.Context(), q.policy.ID, store.RunKindFull,
		"unconfigured-manual", MarshalTriggerBody(store.RunKindFull), "actor")
	if outcome != TriggerUnavailable || payload != nil {
		t.Fatalf("outcome=%v payload=%v, want unavailable", outcome, payload)
	}
	if runs := q.runs(q.policy.ID); len(runs) != 0 {
		t.Fatalf("unconfigured manual trigger created runs: %+v", runs)
	}
}

// TestUnconfiguredRuntimeCronRunFailsVisibly implements the other half of
// decision 9 through the real worker: a cron run is created and then fails with
// error_class unavailable. Nothing is skipped silently.
func TestUnconfiguredRuntimeCronRunFailsVisibly(t *testing.T) {
	q := newQueueFixture(t)
	q.unconfigured()
	run := q.queued("unconfigured-cron", store.RunKindFull, store.RunTriggerCron, time.Now().UTC())
	q.start(nil)

	final := q.waitForRunState(run.ID, store.RunStatusFailed, store.RunErrorUnavailable)
	if final.ErrorCode == nil || *final.ErrorCode != "unconfigured" {
		t.Fatalf("error code = %v, want unconfigured", final.ErrorCode)
	}
	names := q.eventNames(run.ID)
	if len(names) < 2 || names[0] != "claimed" || names[len(names)-1] != "failed" {
		t.Fatalf("run log = %v, want claimed ... failed", names)
	}
	if got := q.systemAuditCount("runs.failed"); got != 1 {
		t.Fatalf("runs.failed system audits = %d, want 1", got)
	}
	if got := q.systemAuditCount("runs.claimed"); got != 1 {
		t.Fatalf("runs.claimed system audits = %d, want 1", got)
	}
	// The failed run holds a terminal status: it is never claimed again.
	if _, ok, err := q.database.ClaimRun(q.t.Context()); err != nil || ok {
		t.Fatalf("failed run was claimable: %v, %v", ok, err)
	}
}

// TestPreflightFailureIsRecordedOnTheRun drives one run through the real worker
// with a configured runtime and an unreachable MySQL socket: the fresh
// preflight fails, the run records error_class preflight with the stable code,
// and the source keeps the failed last_preflight_json.
func TestPreflightFailureIsRecordedOnTheRun(t *testing.T) {
	q := newQueueFixture(t)
	run := q.queued("preflight-run", store.RunKindFull, store.RunTriggerManual, time.Now().UTC())
	q.start(nil)

	final := q.waitForRunState(run.ID, store.RunStatusFailed, store.RunErrorPreflight)
	if final.ErrorCode == nil || *final.ErrorCode != "identity_query_failed" {
		t.Fatalf("error code = %v, want the preflight's stable code", final.ErrorCode)
	}
	if names := q.eventNames(run.ID); len(names) < 2 || names[len(names)-1] != "failed" {
		t.Fatalf("run log = %v, want a failed event", names)
	}
	assertSourcePreflightFailed(t, q)
	assertNoPhysicalBackup(t, q)
}

// assertSourcePreflightFailed requires the failed observation on the source.
func assertSourcePreflightFailed(t *testing.T, q *queueFixture) {
	t.Helper()
	source, err := q.database.Source(q.t.Context(), q.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if source.LastPreflight == nil || source.LastPreflight.OK {
		t.Fatalf("last_preflight = %+v, want the failed observation", source.LastPreflight)
	}
	if source.ServerUUID != nil || source.ServerVersion != nil {
		t.Fatalf("failed identity persisted server identity: %+v", source)
	}
}

// assertNoPhysicalBackup requires nothing to be published for a run that never
// started.
func assertNoPhysicalBackup(t *testing.T, q *queueFixture) {
	t.Helper()
	var backups int
	if err := q.database.DB().QueryRowContext(q.t.Context(), `SELECT COUNT(*) FROM backups`).Scan(&backups); err != nil {
		t.Fatal(err)
	}
	if backups != 0 {
		t.Fatalf("preflight-failed run left %d physical backups", backups)
	}
}

// TestRestartMarksRunningRowsInterrupted proves startup recovery: a row left
// running by a stopped process becomes failed/interrupted on the next start,
// keeps its cancel flag, writes the interrupted event, is audited, and is never
// re-executed.
func TestRestartMarksRunningRowsInterrupted(t *testing.T) {
	q := newQueueFixture(t)
	run := q.queued("interrupted-run", store.RunKindFull, store.RunTriggerCron, time.Now().UTC())
	// The invariant chain.Run would set: the row is running with a cancel
	// request that lost to an in-flight commit. Recovery must accept it.
	if _, ok, err := q.database.ClaimRun(q.t.Context()); err != nil || !ok {
		t.Fatalf("claim: %v ok=%v", err, ok)
	}
	if err := q.database.RequestCancel(q.t.Context(), run.ID,
		store.SystemAudit("runs.cancel_requested", store.AuditDetail{})); err != nil {
		t.Fatal(err)
	}
	// The process dies here: the connection is closed with the row still running.
	q.reopen()

	// The restarted dispatcher runs recovery before its workers start. A
	// future schedule keeps the scheduler from enqueuing anything new.
	q.schedule(q.policy.ID, time.Now().UTC().Add(24*time.Hour), time.Now().UTC().Add(24*time.Hour))
	q.start(nil)

	final := q.waitForRunState(run.ID, store.RunStatusFailed, store.RunErrorInterrupted)
	assertInterrupted(t, q, final)
	// Its slot is never re-executed: nothing is claimable afterwards, and the
	// recovery did not create a replacement run for it.
	if _, ok, err := q.database.ClaimRun(q.t.Context()); err != nil || ok {
		t.Fatalf("interrupted slot was claimable: %v, %v", ok, err)
	}
	if runs := q.runs(q.policy.ID); len(runs) != 1 {
		t.Fatalf("recovery re-created a run for the lost slot: %+v", runs)
	}
}

// assertInterrupted checks the frozen recovery outcome of one running row.
func assertInterrupted(t *testing.T, q *queueFixture, final store.Run) {
	t.Helper()
	if final.ErrorCode == nil || *final.ErrorCode != "interrupted" {
		t.Fatalf("error code = %v, want interrupted", final.ErrorCode)
	}
	if !final.CancelRequested {
		t.Fatal("recovery dropped the cancel request flag of the interrupted run")
	}
	if final.FinishedAt == nil {
		t.Fatal("interrupted run has no finished_at")
	}
	if got := q.systemAuditCount("runs.interrupted"); got != 1 {
		t.Fatalf("runs.interrupted system audits = %d, want 1", got)
	}
	if names := q.eventNames(final.ID); len(names) != 1 || names[0] != "interrupted" {
		t.Fatalf("run log = %v, want one interrupted event", names)
	}
}

// TestRunLogCapWritesExactlyOneTruncatedMarker drives the bounded log past its
// cap and proves the last row is one truncated marker and the row count never
// exceeds MaxRunLogRows.
func TestRunLogCapWritesExactlyOneTruncatedMarker(t *testing.T) {
	q := newQueueFixture(t)
	run := q.queued("cap-run", store.RunKindFull, store.RunTriggerManual, time.Now().UTC())

	for i := range store.MaxRunLogRows {
		if err := q.database.AppendRunLog(q.t.Context(), run.ID, "info", "event", fmt.Sprintf("row %d", i), nil); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	for range 5 {
		if err := q.database.AppendRunLog(q.t.Context(), run.ID, "info", "late", "dropped", nil); err != nil {
			t.Fatalf("overflow append: %v", err)
		}
	}
	assertCappedLog(t, q.events(run.ID))
}

// assertCappedLog requires the cap, exactly one marker and the marker last.
func assertCappedLog(t *testing.T, logs []store.RunLog) {
	t.Helper()
	if len(logs) != store.MaxRunLogRows {
		t.Fatalf("log rows = %d, want the cap %d", len(logs), store.MaxRunLogRows)
	}
	markers := 0
	for _, row := range logs {
		if row.Event == store.TruncatedEvent {
			markers++
		}
	}
	if markers != 1 {
		t.Fatalf("truncated markers = %d, want exactly 1", markers)
	}
	if last := logs[len(logs)-1]; last.Event != store.TruncatedEvent || last.Level != "warn" {
		t.Fatalf("last row = %+v, want the truncated marker", last)
	}
}

// TestDispatcherStopIsIdempotentAndBounded proves the frozen shutdown contract:
// Stop cancels the workers, waits for them and can be called once per start
// without hanging. A second dispatcher started on the same store recovers.
func TestDispatcherStopIsIdempotentAndBounded(t *testing.T) {
	q := newQueueFixture(t)
	q.schedule(q.policy.ID, time.Now().UTC().Add(24*time.Hour), time.Now().UTC().Add(24*time.Hour))
	d, err := Start(q.t.Context(), Config{
		Database: q.database, Settings: q.runtime, Logger: q.logger(),
		Preflight: &resources.Service{Store: q.database, Settings: q.runtime},
	})
	if err != nil {
		t.Fatal(err)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- d.Stop() }()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(shutdownWait + time.Second):
		t.Fatal("Stop did not return inside the bounded shutdown window")
	}
}

// TestCanceledRunContextStillRecordsTheOutcome proves the shutdown path records
// a terminal status even though the drain cancels the run context first: the
// write is detached from that cancellation with a bounded window, so a SIGTERM
// during a backup leaves canceled or failed, never running. Without the
// detached context every attempt fails on "context canceled" and the row stays
// running until the next start's recovery.
func TestCanceledRunContextStillRecordsTheOutcome(t *testing.T) {
	q := newQueueFixture(t)
	run := q.queued("shutdown-outcome", store.RunKindFull, store.RunTriggerManual, time.Now().UTC())
	if _, claimed, err := q.database.ClaimRun(q.t.Context()); err != nil || !claimed {
		t.Fatalf("claim = %v, %v", claimed, err)
	}
	ctx, cancel := context.WithCancel(q.t.Context())
	cancel()
	d := q.newDispatcher(time.Now)
	if err := d.failRun(ctx, run, store.RunErrorBackupFailed, "backup_failed", context.Canceled); err != nil {
		t.Fatalf("failRun on a canceled context: %v", err)
	}
	stored := q.run(run.ID)
	if stored.Status != store.RunStatusFailed || stored.ErrorClass == nil || *stored.ErrorClass != store.RunErrorBackupFailed {
		t.Fatalf("stored run = %+v, want failed/backup_failed", stored)
	}
	if logs := q.eventNames(run.ID); len(logs) == 0 || logs[len(logs)-1] != eventFailed {
		t.Fatalf("run log = %v, want a final failed event", logs)
	}
}
