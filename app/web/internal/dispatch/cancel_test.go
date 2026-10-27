package dispatch

import (
	"context"
	"errors"
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
