package dispatch

import (
	"testing"
	"time"

	"github.com/flyingEirc/Intern/app/web/internal/store"
)

// TestSchedulerCoalescesMissedTriggersToOneRun proves the frozen missed-trigger
// rule against real SQLite: a schedule column far in the past yields exactly one
// run of that kind, the stored next_*_at becomes the coalesced scheduled_at, and
// it advances to the next future fire time from Next(now).
func TestSchedulerCoalescesMissedTriggersToOneRun(t *testing.T) {
	f := newFixture(t)
	repository := f.repository("coalesce-local")
	source := f.source("coalesce-source", testSocketPath(t))
	policy := f.policy("coalesce-policy", source.ID, repository.ID, true)

	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	missed := now.Add(-6 * time.Hour)
	// The incremental schedule is still in the future, so only the full fires.
	f.schedule(policy.ID, missed, now.Add(30*time.Minute))

	d := f.newDispatcher(fixedClock(now))
	d.tick(t.Context())
	// A second tick must not enqueue the same trigger again: the stored
	// schedule already advanced past now.
	d.tick(t.Context())

	runs := f.runs(policy.ID)
	if len(runs) != 1 {
		t.Fatalf("coalesced runs = %d (%+v), want exactly 1", len(runs), runs)
	}
	assertCoalescedFull(t, runs[0], policy.ID, missed)
	full, _ := f.policySchedule(policy.ID)
	if full == nil || !full.After(now) {
		t.Fatalf("next_full_at = %v, want a future time from Next(now)", full)
	}
	if want := nextHourly(now, 0); !full.Equal(want) {
		t.Fatalf("next_full_at = %s, want %s", full, want)
	}
	// The incremental schedule that was not due is untouched.
	assertScheduleIs(t, f, policy.ID, now.Add(30*time.Minute))
	if names := f.eventNames(runs[0].ID); len(names) != 1 || names[0] != "queued" {
		t.Fatalf("run log = %v, want one queued event", names)
	}
}

// assertCoalescedFull checks the run the coalescing rule produces.
func assertCoalescedFull(t *testing.T, run store.Run, policyID string, scheduledAt time.Time) {
	t.Helper()
	if run.Kind != store.RunKindFull || run.Trigger != store.RunTriggerCron || run.Status != store.RunStatusQueued {
		t.Fatalf("run = %+v", run)
	}
	if run.ScheduledAt != store.ScheduleStamp(scheduledAt) {
		t.Fatalf("scheduled_at = %q, want the coalesced trigger %q", run.ScheduledAt, store.ScheduleStamp(scheduledAt))
	}
	if want := cronKey(policyID, store.RunKindFull, scheduledAt); run.IdempotencyKey != want {
		t.Fatalf("idempotency key = %q, want %q", run.IdempotencyKey, want)
	}
}

// assertScheduleIs requires the untouched incremental schedule.
func assertScheduleIs(t *testing.T, f *fixture, policyID string, want time.Time) {
	t.Helper()
	_, incremental := f.policySchedule(policyID)
	if incremental == nil || !incremental.Equal(want) {
		t.Fatalf("next_incremental_at = %v, want the untouched %s", incremental, want)
	}
}

// TestSchedulerFullWinsWhenBothAreDue advances both schedules together and
// proves one full wins: exactly one run is created, it is the full, and both
// next_* columns advance from Next(now) — the due incremental is not queued
// behind the full.
func TestSchedulerFullWinsWhenBothAreDue(t *testing.T) {
	f := newFixture(t)
	repository := f.repository("both-local")
	source := f.source("both-source", testSocketPath(t))
	policy := f.policy("both-policy", source.ID, repository.ID, true)

	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	f.schedule(policy.ID, now.Add(-time.Hour), now.Add(-time.Minute))

	d := f.newDispatcher(fixedClock(now))
	d.tick(t.Context())

	runs := f.runs(policy.ID)
	if len(runs) != 1 || runs[0].Kind != store.RunKindFull {
		t.Fatalf("simultaneous due runs = %+v, want exactly one full", runs)
	}
	full, incremental := f.policySchedule(policy.ID)
	if full == nil || incremental == nil {
		t.Fatalf("schedule columns = %v, %v", full, incremental)
	}
	if !full.After(now) || !incremental.After(now) {
		t.Fatalf("both schedules must advance past now: full=%s incremental=%s", full, incremental)
	}
	if want := nextHourly(now, 0); !full.Equal(want) {
		t.Fatalf("next_full_at = %s, want %s", full, want)
	}
	if want := nextHourly(now, 30); !incremental.Equal(want) {
		t.Fatalf("next_incremental_at = %s, want %s", incremental, want)
	}
}

// TestSchedulerIncrementalFiresAlone covers the other branch: an incremental
// due without a full creates one incremental run and still advances both
// columns, because a fire always resets the pair.
func TestSchedulerIncrementalFiresAlone(t *testing.T) {
	f := newFixture(t)
	repository := f.repository("incr-local")
	source := f.source("incr-source", testSocketPath(t))
	policy := f.policy("incr-policy", source.ID, repository.ID, true)

	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	due := now.Add(-time.Minute)
	f.schedule(policy.ID, now.Add(time.Hour), due)

	d := f.newDispatcher(fixedClock(now))
	d.tick(t.Context())

	runs := f.runs(policy.ID)
	if len(runs) != 1 || runs[0].Kind != store.RunKindIncremental {
		t.Fatalf("runs = %+v, want one incremental", runs)
	}
	if runs[0].ScheduledAt != store.ScheduleStamp(due) {
		t.Fatalf("scheduled_at = %q", runs[0].ScheduledAt)
	}
	full, incremental := f.policySchedule(policy.ID)
	if want := nextHourly(now, 0); full == nil || !full.Equal(want) {
		t.Fatalf("next_full_at = %v, want %s", full, want)
	}
	if want := nextHourly(now, 30); incremental == nil || !incremental.Equal(want) {
		t.Fatalf("next_incremental_at = %v, want %s", incremental, want)
	}
}

// TestSchedulerInitializesNullScheduleWithoutFiring covers a policy row created
// before the schedule columns existed: a NULL column is filled from the cron
// expression and does not fire on that tick.
func TestSchedulerInitializesNullScheduleWithoutFiring(t *testing.T) {
	f := newFixture(t)
	repository := f.repository("null-local")
	source := f.source("null-source", testSocketPath(t))
	policy := f.policy("null-policy", source.ID, repository.ID, true)

	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	if full, incremental := f.policySchedule(policy.ID); full != nil || incremental != nil {
		t.Fatalf("fresh policy has schedules: %v %v", full, incremental)
	}
	d := f.newDispatcher(fixedClock(now))
	d.tick(t.Context())
	if runs := f.runs(policy.ID); len(runs) != 0 {
		t.Fatalf("initialization fired runs: %+v", runs)
	}
	full, incremental := f.policySchedule(policy.ID)
	if want := nextHourly(now, 0); full == nil || !full.Equal(want) {
		t.Fatalf("next_full_at = %v, want %s", full, want)
	}
	if want := nextHourly(now, 30); incremental == nil || !incremental.Equal(want) {
		t.Fatalf("next_incremental_at = %v, want %s", incremental, want)
	}
}

// TestSchedulerSkipsDisabledPolicies proves a disabled policy never fires even
// when its stored schedule is due.
func TestSchedulerSkipsDisabledPolicies(t *testing.T) {
	f := newFixture(t)
	repository := f.repository("disabled-local")
	source := f.source("disabled-source", testSocketPath(t))
	policy := f.policy("disabled-policy", source.ID, repository.ID, false)

	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	f.schedule(policy.ID, now.Add(-time.Hour), now.Add(-time.Hour))

	d := f.newDispatcher(fixedClock(now))
	d.tick(t.Context())
	if runs := f.runs(policy.ID); len(runs) != 0 {
		t.Fatalf("disabled policy created runs: %+v", runs)
	}
}

// TestSchedulerTimeZoneIsHonoured checks the expression is evaluated in the
// policy's time zone, not UTC: 02:00 in Asia/Shanghai is 18:00 UTC.
func TestSchedulerTimeZoneIsHonoured(t *testing.T) {
	f := newFixture(t)
	repository := f.repository("tz-local")
	source := f.source("tz-source", testSocketPath(t))
	policy, err := f.database.CreatePolicy(t.Context(), store.PolicyInput{
		Name: "tz-policy", SourceID: source.ID, LocalRepositoryID: repository.ID,
		FullCron: "0 2 * * *", IncrementalCron: "0 3 * * *", Timezone: "Asia/Shanghai",
		MaxChainLength: 3, Parallel: 1, ZstdLevel: 3,
	}, store.SystemAudit("policies.create", store.AuditDetail{}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.database.EnablePolicy(t.Context(), policy.ID, true,
		&store.PreflightRecord{OK: true, Checks: []string{"identity"}, Warnings: []store.PreflightWarning{}},
		store.SystemAudit("policies.enable", store.AuditDetail{})); err != nil {
		t.Fatal(err)
	}
	// 2026-03-01T12:00Z is 20:00 in Shanghai; the next 02:00 local fire is
	// 2026-03-02T02:00+08:00 = 2026-03-01T18:00Z.
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	f.schedule(policy.ID, now.Add(-time.Minute), now.Add(time.Hour))
	d := f.newDispatcher(fixedClock(now))
	d.tick(t.Context())

	full, _ := f.policySchedule(policy.ID)
	want := time.Date(2026, 3, 1, 18, 0, 0, 0, time.UTC)
	if full == nil || !full.Equal(want) {
		t.Fatalf("next_full_at = %v, want %s (02:00 Asia/Shanghai)", full, want)
	}
	if runs := f.runs(policy.ID); len(runs) != 1 {
		t.Fatalf("runs = %+v, want one coalesced full", runs)
	}
}

// fixedClock is the injectable clock the scheduler cases freeze time with.
func fixedClock(now time.Time) func() time.Time {
	return func() time.Time { return now }
}

// nextHourly is the expected Next(now) of an "M * * * *" expression.
func nextHourly(now time.Time, minute int) time.Time {
	next := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), minute, 0, 0, time.UTC)
	if !next.After(now) {
		next = next.Add(time.Hour)
	}
	return next
}
