package dispatch

import (
	"testing"
	"time"

	"github.com/flyingEirc/Intern/app/web/internal/store"
)

// previewPolicy is one policy the preview cases compute against. The crons
// fire hourly so the expected times stay obvious.
func previewPolicy(timezone string, enabled bool) store.Policy {
	return store.Policy{
		ID: "policy-preview", FullCron: "0 * * * *", IncrementalCron: "30 * * * *",
		Timezone: timezone, Enabled: enabled,
	}
}

// at is a UTC instant written out in full, so a shifted expectation reads as
// the wrong wall time instead of a wrong offset.
func at(year int, month time.Month, day, hour, minute int) time.Time {
	return time.Date(year, month, day, hour, minute, 0, 0, time.UTC)
}

// wallTime is the same wall time in a named zone, which is how a cron fire
// is expressed before it is converted to the UTC the API returns.
func wallTime(timezone string, year int, month time.Month, day, hour, minute int) time.Time {
	location, err := time.LoadLocation(timezone)
	if err != nil {
		panic(err)
	}
	return time.Date(year, month, day, hour, minute, 0, 0, location)
}

func preview(t *testing.T, policy store.Policy, full, incremental *time.Time, now time.Time, count int) SchedulePreview {
	t.Helper()
	got, err := PreviewSchedule(policy, full, incremental, now, count)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// assertFires requires the exact chronological sequence, including kinds and
// the missed marks, and that every instant is UTC.
func assertFires(t *testing.T, got SchedulePreview, want []ScheduleFire) {
	t.Helper()
	if len(got.Fires) != len(want) {
		t.Fatalf("fires = %d, want %d (%v)", len(got.Fires), len(want), got.Fires)
	}
	var previous time.Time
	for i, fire := range got.Fires {
		if fire.At.Location() != time.UTC {
			t.Fatalf("fire %d location = %s, want UTC", i, fire.At.Location())
		}
		if !previous.IsZero() && !fire.At.After(previous) {
			t.Fatalf("fires are not strictly chronological: %s then %s", previous, fire.At)
		}
		previous = fire.At
		wanted := want[i]
		if fire.Kind != wanted.Kind || fire.Missed != wanted.Missed || !fire.At.Equal(wanted.At) {
			t.Fatalf("fire %d = %+v, want %+v", i, fire, wanted)
		}
	}
}

func fire(kind string, when time.Time, missed bool) ScheduleFire {
	return ScheduleFire{Kind: kind, At: when.UTC(), Missed: missed}
}

// TestPreviewUsesStoredScheduleForEnabledPolicy covers the ordinary case: an
// enabled policy with stored times in the future previews exactly those
// times, and the following fires come from the cron expressions.
func TestPreviewUsesStoredScheduleForEnabledPolicy(t *testing.T) {
	now := at(2026, 3, 1, 12, 0)
	policy := previewPolicy("UTC", true)
	full := at(2026, 3, 1, 13, 0)
	incremental := at(2026, 3, 1, 12, 30)
	got := preview(t, policy, &full, &incremental, now, 4)
	if !got.Enabled || got.Timezone != "UTC" || got.PolicyID != policy.ID {
		t.Fatalf("preview identity = %+v", got)
	}
	assertFires(t, got, []ScheduleFire{
		fire(store.RunKindIncremental, incremental, false),
		fire(store.RunKindFull, full, false),
		fire(store.RunKindIncremental, at(2026, 3, 1, 13, 30), false),
		fire(store.RunKindFull, at(2026, 3, 1, 14, 0), false),
	})
}

// TestPreviewMarksMissedStoredTimeAndCoalesces covers a stored time that is
// not after now: it is emitted once with missed set, and every following fire
// of that kind is computed from now, so a schedule hours behind still yields
// exactly one missed fire.
func TestPreviewMarksMissedStoredTimeAndCoalesces(t *testing.T) {
	now := at(2026, 3, 1, 12, 0)
	policy := previewPolicy("UTC", true)
	// Six hours behind, and exactly now: both are not after now, so both are
	// missed exactly once.
	full := now.Add(-6 * time.Hour)
	incremental := now
	got := preview(t, policy, &full, &incremental, now, 5)
	assertFires(t, got, []ScheduleFire{
		fire(store.RunKindFull, full, true),
		fire(store.RunKindIncremental, incremental, true),
		fire(store.RunKindIncremental, at(2026, 3, 1, 12, 30), false),
		fire(store.RunKindFull, at(2026, 3, 1, 13, 0), false),
		fire(store.RunKindIncremental, at(2026, 3, 1, 13, 30), false),
	})
}

// TestPreviewCollisionOfFullAndIncrementalYieldsOneFull covers both kinds due
// at the same instant: one full fire is emitted and both schedules advance,
// so the incremental at that instant never appears.
func TestPreviewCollisionOfFullAndIncrementalYieldsOneFull(t *testing.T) {
	now := at(2026, 3, 1, 12, 0)
	policy := previewPolicy("UTC", true)
	shared := at(2026, 3, 1, 13, 0)
	got := preview(t, policy, &shared, &shared, now, 4)
	assertFires(t, got, []ScheduleFire{
		fire(store.RunKindFull, shared, false),
		fire(store.RunKindIncremental, at(2026, 3, 1, 13, 30), false),
		fire(store.RunKindFull, at(2026, 3, 1, 14, 0), false),
		fire(store.RunKindIncremental, at(2026, 3, 1, 14, 30), false),
	})

	// The same rule holds when the shared instant is already missed.
	missed := preview(t, policy, &now, &now, now, 1)
	assertFires(t, missed, []ScheduleFire{fire(store.RunKindFull, now, true)})
}

// TestPreviewDisabledPolicyIgnoresStoredSchedule covers a disabled policy: the
// fires are computed from now with missed false, even when the stored times
// are in the past, because they describe what enabling would schedule.
func TestPreviewDisabledPolicyIgnoresStoredSchedule(t *testing.T) {
	now := at(2026, 3, 1, 12, 0)
	policy := previewPolicy("UTC", false)
	past := now.Add(-6 * time.Hour)
	got := preview(t, policy, &past, &past, now, 3)
	if got.Enabled {
		t.Fatal("disabled policy preview reports enabled")
	}
	assertFires(t, got, []ScheduleFire{
		fire(store.RunKindIncremental, at(2026, 3, 1, 12, 30), false),
		fire(store.RunKindFull, at(2026, 3, 1, 13, 0), false),
		fire(store.RunKindIncremental, at(2026, 3, 1, 13, 30), false),
	})
}

// TestPreviewUninitializedEnabledPolicyComputesFromNow covers an enabled
// policy whose schedule columns are still NULL: the fires come from now with
// missed false, exactly like a disabled policy's preview.
func TestPreviewUninitializedEnabledPolicyComputesFromNow(t *testing.T) {
	now := at(2026, 3, 1, 12, 15)
	policy := previewPolicy("UTC", true)
	got := preview(t, policy, nil, nil, now, 2)
	assertFires(t, got, []ScheduleFire{
		fire(store.RunKindIncremental, at(2026, 3, 1, 12, 30), false),
		fire(store.RunKindFull, at(2026, 3, 1, 13, 0), false),
	})
}

// TestPreviewOneKindStoredOtherMissing covers a schedule that is only half
// initialized: the stored kind starts from its column, the missing kind is
// computed from now.
func TestPreviewOneKindStoredOtherMissing(t *testing.T) {
	now := at(2026, 3, 1, 12, 0)
	policy := previewPolicy("UTC", true)
	full := at(2026, 3, 1, 15, 0)
	got := preview(t, policy, &full, nil, now, 4)
	assertFires(t, got, []ScheduleFire{
		fire(store.RunKindIncremental, at(2026, 3, 1, 12, 30), false),
		fire(store.RunKindIncremental, at(2026, 3, 1, 13, 30), false),
		fire(store.RunKindIncremental, at(2026, 3, 1, 14, 30), false),
		fire(store.RunKindFull, full, false),
	})
}

// TestPreviewHonoursAsiaShanghai covers a non-UTC zone without daylight
// saving: 02:00 Asia/Shanghai is 18:00 UTC the previous day.
func TestPreviewHonoursAsiaShanghai(t *testing.T) {
	policy := previewPolicy("Asia/Shanghai", true)
	policy.FullCron = "0 2 * * *"
	policy.IncrementalCron = "0 3 * * *"
	now := at(2026, 3, 1, 12, 0)
	got := preview(t, policy, nil, nil, now, 2)
	assertFires(t, got, []ScheduleFire{
		fire(store.RunKindFull, wallTime("Asia/Shanghai", 2026, 3, 2, 2, 0), false),
		fire(store.RunKindIncremental, wallTime("Asia/Shanghai", 2026, 3, 2, 3, 0), false),
	})
	if want := at(2026, 3, 1, 18, 0); !got.Fires[0].At.Equal(want) {
		t.Fatalf("02:00 Asia/Shanghai = %s, want %s", got.Fires[0].At, want)
	}
}

// TestPreviewNewYorkSpringForward covers the 2026-03-08 spring-forward in
// America/New_York, when local times from 02:00 to 02:59 do not exist. A
// daily 02:30 full therefore has no fire that day and its first fire is
// 02:30 EDT on the 9th, while the 03:30 incremental still fires on the 8th
// and sorts first. Both come back through the scheduler's own Next.
func TestPreviewNewYorkSpringForward(t *testing.T) {
	policy := previewPolicy("America/New_York", true)
	policy.FullCron = "30 2 * * *"
	policy.IncrementalCron = "30 3 * * *"
	// 2026-03-08 01:00 EST, one hour before the jump to 03:00 EDT.
	now := wallTime("America/New_York", 2026, 3, 8, 1, 0)
	got := preview(t, policy, nil, nil, now, 4)
	assertFires(t, got, []ScheduleFire{
		fire(store.RunKindIncremental, wallTime("America/New_York", 2026, 3, 8, 3, 30), false),
		fire(store.RunKindFull, wallTime("America/New_York", 2026, 3, 9, 2, 30), false),
		fire(store.RunKindIncremental, wallTime("America/New_York", 2026, 3, 9, 3, 30), false),
		fire(store.RunKindFull, wallTime("America/New_York", 2026, 3, 10, 2, 30), false),
	})
	// 03:30 EDT on the transition day is 07:30 UTC, an hour later than the
	// same wall time reads the day before.
	if got.Fires[0].At.Hour() != 7 {
		t.Fatalf("spring-forward incremental = %s, want 07:30Z", got.Fires[0].At)
	}
}

// TestPreviewNewYorkFallBack covers the 2026-11-01 fall-back in
// America/New_York, when 01:30 local happens twice. The scheduler's cron
// fires both occurrences, the EDT one and then the EST one an hour later, and
// the following day settles on the single EST occurrence. Both kinds share the
// expression, so each instant collapses to one full.
func TestPreviewNewYorkFallBack(t *testing.T) {
	policy := previewPolicy("America/New_York", true)
	policy.FullCron = "30 1 * * *"
	policy.IncrementalCron = "30 1 * * *"
	// 2026-11-01 00:30 EDT, before the repeated hour.
	now := wallTime("America/New_York", 2026, 11, 1, 0, 30)
	got := preview(t, policy, nil, nil, now, 3)
	edt := wallTime("America/New_York", 2026, 11, 1, 1, 30)
	est := edt.Add(time.Hour)
	assertFires(t, got, []ScheduleFire{
		fire(store.RunKindFull, edt, false),
		fire(store.RunKindFull, est, false),
		fire(store.RunKindFull, wallTime("America/New_York", 2026, 11, 2, 1, 30), false),
	})
	if _, offset := edt.Zone(); offset != -4*60*60 {
		t.Fatalf("first 01:30 offset = %d, want EDT (-14400)", offset)
	}
	if _, offset := est.Zone(); offset != -5*60*60 {
		t.Fatalf("second 01:30 offset = %d, want EST (-18000)", offset)
	}
	// The two occurrences of the repeated hour are one hour apart, and the next
	// day's single 01:30 EST is 24 hours after the EST occurrence.
	if gap := got.Fires[1].At.Sub(got.Fires[0].At); gap != time.Hour {
		t.Fatalf("repeated-hour gap = %s, want 1h", gap)
	}
	if gap := got.Fires[2].At.Sub(got.Fires[1].At); gap != 24*time.Hour {
		t.Fatalf("next-day gap = %s, want 24h", gap)
	}
}

// TestPreviewStoredMissedTimeInShanghai resumes from now in the policy's own
// zone: a missed 02:00 fire is followed by the next 02:00 computed from now,
// not by replaying the missed days in between.
func TestPreviewStoredMissedTimeInShanghai(t *testing.T) {
	policy := previewPolicy("Asia/Shanghai", true)
	policy.FullCron = "0 2 * * *"
	policy.IncrementalCron = "0 3 * * *"
	now := at(2026, 6, 15, 12, 0)
	missedFull := wallTime("Asia/Shanghai", 2026, 6, 10, 2, 0)
	futureIncremental := wallTime("Asia/Shanghai", 2026, 6, 16, 3, 0)
	got := preview(t, policy, &missedFull, &futureIncremental, now, 3)
	assertFires(t, got, []ScheduleFire{
		fire(store.RunKindFull, missedFull, true),
		// The coalesced next 02:00, computed from now, lands the same evening and
		// sorts before the stored 03:00 incremental, which is still in the future.
		fire(store.RunKindFull, wallTime("Asia/Shanghai", 2026, 6, 16, 2, 0), false),
		fire(store.RunKindIncremental, futureIncremental, false),
	})
}
