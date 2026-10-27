package dispatch

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/flyingEirc/Intern/app/web/internal/resources"
	"github.com/flyingEirc/Intern/app/web/internal/store"
)

// blockingPreflight stands in for the real preflight's version probe: it holds
// the run inside admission until the context is canceled, then reports the
// canceled observation the real probe produces.
type blockingPreflight struct {
	entered chan struct{}
	// leaked records that the probe did not observe the cancellation, which is
	// the failure mode a cancel during preflight must never allow.
	leaked chan struct{}
}

func newBlockingPreflight() *blockingPreflight {
	return &blockingPreflight{entered: make(chan struct{}), leaked: make(chan struct{})}
}

func (p *blockingPreflight) PreflightSource(ctx context.Context, _, _ string) (resources.PreflightOutcome, error) {
	close(p.entered)
	select {
	case <-ctx.Done():
		return resources.PreflightOutcome{
			Failure: &resources.Failure{Check: "tool", Code: "canceled", Message: "Preflight was canceled."},
		}, ctx.Err()
	case <-time.After(30 * time.Second):
		close(p.leaked)
		return resources.PreflightOutcome{}, errors.New("probe never observed the cancellation")
	}
}

// successPreflight returns a passing observation after running hook, so a test
// can place a cancel request exactly between admission and chain.Run.
type successPreflight struct{ hook func() }

func (p successPreflight) PreflightSource(context.Context, string, string) (resources.PreflightOutcome, error) {
	if p.hook != nil {
		p.hook()
	}
	return resources.PreflightOutcome{
		Checks: []string{"identity", "compatibility"}, ServerUUID: preflightUUID,
		ServerVersion: "8.4.11", ToolVersion: "8.4.0-7",
	}, nil
}

// TestCancelDuringPreflightEndsCanceled is N1: a cancel request that arrives
// while the run is inside preflight must end the run canceled, with no backup.
// Before the fix the cancel function was only registered around chain.Run, so
// the request set the flag and nothing observed it: the preflight finished, the
// chain ran, and the run was reported completed.
func TestCancelDuringPreflightEndsCanceled(t *testing.T) {
	q := newQueueFixture(t)
	probe := newBlockingPreflight()
	// The run needs a live server identity to reach the connection check, so the
	// fixture records one; the blocking preflight is what holds the run.
	recordServerIdentity(t, q)
	run := q.queued("cancel-in-preflight", store.RunKindFull, store.RunTriggerManual, time.Now().UTC())
	d := startWithPreflight(t, q, probe)

	select {
	case <-probe.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the run never reached preflight")
	}
	if err := d.RequestCancel(q.t.Context(), run.ID); err != nil {
		t.Fatalf("cancel during preflight: %v", err)
	}
	assertCanceledWithoutBackup(t, q, run.ID)
	select {
	case <-probe.leaked:
		t.Fatal("the preflight probe never observed the cancellation; the process would still be running")
	default:
	}
}

// startWithPreflight starts the real dispatcher with one injected preflight.
func startWithPreflight(t *testing.T, q *queueFixture, preflight PreflightRunner) *Dispatcher {
	t.Helper()
	d, err := Start(q.t.Context(), Config{
		Database: q.database, Settings: q.runtime, Logger: q.logger(), Preflight: preflight,
	})
	if err != nil {
		t.Fatal(err)
	}
	q.t.Cleanup(func() { _ = d.Stop() })
	return d
}

// assertCanceledWithoutBackup requires the terminal canceled state a cancel
// produces: finished, no backup id, no published backup, one canceled event and
// one canceled audit.
func assertCanceledWithoutBackup(t *testing.T, q *queueFixture, runID string) {
	t.Helper()
	final := q.waitForRunState(runID, store.RunStatusCanceled, "")
	if final.FinishedAt == nil {
		t.Fatal("canceled run has no finished_at")
	}
	if final.BackupID != nil {
		t.Fatalf("canceled run recorded backup %v", *final.BackupID)
	}
	assertNoPhysicalBackupForRun(t, q, runID)
	names := q.eventNames(runID)
	if names[len(names)-1] != eventCanceled {
		t.Fatalf("run log = %v, want a final canceled event", names)
	}
	if got := q.systemAuditCount("runs.canceled"); got != 1 {
		t.Fatalf("runs.canceled system audits = %d, want 1", got)
	}
}

// TestCancelBetweenPreflightAndChainEndsCanceled covers the remaining window:
// the preflight succeeded, the chain has not started, and the cancel request
// lands in between. The re-read before chain.Run must end the run canceled.
func TestCancelBetweenPreflightAndChainEndsCanceled(t *testing.T) {
	q := newQueueFixture(t)
	recordServerIdentity(t, q)
	run := q.queued("cancel-before-chain", store.RunKindFull, store.RunTriggerManual, time.Now().UTC())

	var d *Dispatcher
	probe := successPreflight{hook: func() {
		// The cancel arrives after the preflight's checks pass, exactly in the
		// window between admission and the chain.
		if err := d.config.Database.RequestCancel(q.t.Context(), run.ID,
			store.SystemAudit("runs.cancel_requested", store.AuditDetail{})); err != nil {
			t.Errorf("request cancel: %v", err)
		}
	}}
	d = startWithPreflight(t, q, probe)
	assertCanceledWithoutBackup(t, q, run.ID)
	for _, name := range q.eventNames(run.ID) {
		if name == eventStarted {
			t.Fatalf("the chain started after a cancel landed before it: %v", q.eventNames(run.ID))
		}
	}
}

// TestCancelDuringPostClaimReadsEndsCanceled is X3: a cancel request that lands
// while the run is inside the post-claim control-plane reads must end the run
// canceled, not failed with policy_missing or repository_unavailable.
//
// The probe holds the single SQLite connection so the next read blocks, then
// does exactly what RequestCancel does for a running run: write the flag, then
// cancel the registered run context. Before the fix the blocked read inherited
// the cancelable context, returned context.Canceled and was recorded as a
// repository or policy failure even though nothing was wrong with either.
func TestCancelDuringPostClaimReadsEndsCanceled(t *testing.T) {
	q := newQueueFixture(t)
	d := q.newDispatcher(time.Now)
	q.queued("cancel-in-reads", store.RunKindFull, store.RunTriggerManual, time.Now().UTC())
	run := q.claim(t)
	// The flag is set before the blocked read: that is exactly the read the
	// cancel request would have to wait for.
	q.setCancelFlag(t, run.ID)
	release, done := q.blockControlPlane(t, func() error { return d.executeRun(t.Context(), run) })
	d.cancelRegisteredRun(t, run.ID)
	release()
	requireRunResult(t, done)
	assertCanceledWithoutBackup(t, q, run.ID)
}

// claim claims the one queued run the fixture holds.
func (q *queueFixture) claim(t *testing.T) store.Run {
	t.Helper()
	run, claimed, err := q.database.ClaimRun(t.Context())
	if err != nil || !claimed {
		t.Fatalf("claim = %+v %v %v, want the queued run", run, claimed, err)
	}
	return run
}

// setCancelFlag writes the cancel request flag directly, which is the first half
// of what RequestCancel does for a running run.
func (q *queueFixture) setCancelFlag(t *testing.T, runID string) {
	t.Helper()
	if _, err := q.database.DB().ExecContext(t.Context(),
		`UPDATE runs SET cancel_requested = 1 WHERE id = ?`, runID); err != nil {
		t.Fatal(err)
	}
}

// blockControlPlane holds the single database connection so the work's next
// control-plane read waits. The returned release lets the read proceed; done
// carries the work's own result.
func (q *queueFixture) blockControlPlane(t *testing.T, work func() error) (release func(), done <-chan error) {
	t.Helper()
	tx, err := q.database.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- work() }()
	// The connection is now held by the worker's blocked read.
	time.Sleep(100 * time.Millisecond)
	select {
	case workErr := <-result:
		t.Fatalf("the run finished while its first control-plane read was blocked: %v", workErr)
	default:
	}
	return func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil {
			t.Errorf("rollback: %v", rollbackErr)
		}
	}, result
}

// cancelRegisteredRun cancels the context the dispatcher registered for one run,
// which is the second half of what RequestCancel does for a running run.
func (d *Dispatcher) cancelRegisteredRun(t *testing.T, runID string) {
	t.Helper()
	waitFor(t, "the run context to be registered", func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.runningRunID == runID && d.runningCancel != nil
	})
	d.mu.Lock()
	cancel := d.runningCancel
	d.mu.Unlock()
	cancel()
}

// requireRunResult requires the work to finish without an error.
func requireRunResult(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the run did not finish after the cancel")
	}
}

// recordServerIdentity persists the identity a preflight would have observed, so
// the run reaches the connection check instead of failing on a missing identity.
func recordServerIdentity(t *testing.T, q *queueFixture) {
	t.Helper()
	record := store.PreflightRecord{
		OK: true, Checks: []string{"identity"}, Warnings: []store.PreflightWarning{},
		ServerUUID: preflightUUID, ServerVersion: "8.4.11",
	}
	if _, err := q.database.RecordSourcePreflight(q.t.Context(), q.source.ID, record,
		store.SystemAudit("sources.preflight", store.AuditDetail{})); err != nil {
		t.Fatal(err)
	}
}

// assertNoPhysicalBackupForRun requires the run to have published nothing.
func assertNoPhysicalBackupForRun(t *testing.T, q *queueFixture, runID string) {
	t.Helper()
	var backups int
	if err := q.database.DB().QueryRowContext(q.t.Context(),
		`SELECT COUNT(*) FROM backups WHERE run_id = ?`, runID).Scan(&backups); err != nil {
		t.Fatal(err)
	}
	if backups != 0 {
		t.Fatalf("canceled run %s left %d physical backups", runID, backups)
	}
}

// TestShutdownDuringThePreChainReadsEndsFailed is the advisor's Y3 and
// mutation MI: a shutdown — the worker's parent context canceled, no cancel
// request — that lands while the run is inside its pre-chain reads must end the
// run with the same terminal write failChain uses, failed with class and code
// backup_failed. Classifying the shutdown as a cancel would report a canceled
// run, and returning the failed read without a terminal write would leave the
// run running until restart recovery reclaims it.
func TestShutdownDuringThePreChainReadsEndsFailed(t *testing.T) {
	q := newQueueFixture(t)
	q.queued("shutdown-in-reads", store.RunKindFull, store.RunTriggerManual, time.Now().UTC())
	run := q.claim(t)
	d := q.newDispatcher(time.Now)
	parent, cancel := context.WithCancel(q.t.Context())
	release, done := q.blockControlPlane(t, func() error { return d.executeRun(parent, run) })
	cancel()
	release()
	requireRunResult(t, done)
	assertShutdownOutcome(t, q, run.ID)
}

// TestShutdownSweepNeverLeavesARunRunning drives the same rule across the whole
// pre-chain window: the parent is canceled at fine-grained delays while the run
// moves from its claim through admission towards the refuse read, and every run
// must reach the terminal failed state. Before the refuse read's error was
// routed through preChainFailure, a shutdown that landed exactly on that
// boundary returned without any terminal write and left the run running.
func TestShutdownSweepNeverLeavesARunRunning(t *testing.T) {
	q := newQueueFixture(t)
	recordServerIdentity(t, q)
	d := q.newDispatcher(time.Now)
	for i := range 150 {
		q.queued(fmt.Sprintf("shutdown-sweep-%d", i), store.RunKindFull, store.RunTriggerManual, time.Now().UTC())
		run := q.claim(t)
		parent, cancel := context.WithCancel(q.t.Context())
		result := make(chan error, 1)
		go func() { result <- d.executeRun(parent, run) }()
		// The sweep cancels while the run climbs from its claim through
		// admission towards the refuse read, so every pre-chain boundary is
		// crossed at one of the delays.
		time.Sleep(time.Duration(i) * 10 * time.Microsecond)
		cancel()
		if err := <-result; err != nil {
			t.Fatalf("sweep %d: executeRun = %v, want a terminal write and no error", i, err)
		}
		assertTerminalFailure(t, q, run.ID)
	}
}

// assertShutdownOutcome requires the terminal failed state a shutdown produces
// on a pre-chain read: failed with class and code backup_failed, finished, no
// backup and no canceled audit.
func assertShutdownOutcome(t *testing.T, q *queueFixture, runID string) {
	t.Helper()
	final := q.waitForRunState(runID, store.RunStatusFailed, store.RunErrorBackupFailed)
	if final.ErrorCode == nil || *final.ErrorCode != "backup_failed" {
		t.Fatalf("failure code = %v, want backup_failed", final.ErrorCode)
	}
	if final.FinishedAt == nil || final.BackupID != nil {
		t.Fatalf("failed run = %+v, want finished with no backup", final)
	}
	assertNoPhysicalBackupForRun(t, q, runID)
	if got := q.systemAuditCount("runs.canceled"); got != 0 {
		t.Fatalf("runs.canceled system audits = %d, want 0 for a shutdown", got)
	}
}

// assertTerminalFailure requires the sweep's weaker, still-load-bearing
// outcome: the run is terminal and failed. A cancel that lands after the run
// already failed for its own reason (an unreachable socket preflight) is not a
// shutdown outcome, so the class is not asserted here; what must never happen
// is a run left running by the shutdown, or a canceled run without a request.
func assertTerminalFailure(t *testing.T, q *queueFixture, runID string) {
	t.Helper()
	final := q.waitForRunState(runID, store.RunStatusFailed, "")
	if final.FinishedAt == nil {
		t.Fatalf("sweep run = %+v, want finished", final)
	}
	if got := q.systemAuditCount("runs.canceled"); got != 0 {
		t.Fatalf("runs.canceled system audits = %d, want 0 without a cancel request", got)
	}
}
