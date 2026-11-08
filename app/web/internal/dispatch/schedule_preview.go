package dispatch

import (
	"time"

	"github.com/flyingEirc/Intern/app/web/internal/store"
)

// ScheduleFire is one upcoming trigger of a policy. At is the scheduled_at the
// scheduler would give the run, in UTC.
type ScheduleFire struct {
	Kind   string    `json:"kind"`
	At     time.Time `json:"at"`
	Missed bool      `json:"missed"`
}

// SchedulePreview is the read-only view of a policy's next fires. Enabled is
// false when the fires describe what enabling the policy would schedule.
type SchedulePreview struct {
	PolicyID string         `json:"policy_id"`
	Enabled  bool           `json:"enabled"`
	Timezone string         `json:"timezone"`
	Fires    []ScheduleFire `json:"fires"`
}

// previewCursor walks one kind's future fires. A stored time that is not after
// now is emitted once with missed set; every later fire of that kind is
// computed from now, which is how the scheduler coalesces missed triggers.
// after is the instant the following fire is computed from, and it stays at
// the fire most recently emitted: schedule's Next is strictly after its input,
// so the base must not move onto the fire it just computed.
type previewCursor struct {
	kind   string
	next   time.Time
	missed bool
	after  time.Time
}

// PreviewSchedule computes the next count fires of one policy with the same
// schedule function the scheduler ticks use. It never writes the stored
// next_* columns: previewing an uninitialized policy does not initialize it,
// and a disabled policy's stored schedule is left untouched. count must be
// between 1 and 50; the caller validates it.
func PreviewSchedule(policy store.Policy, nextFull, nextIncremental *time.Time, now time.Time, count int) (SchedulePreview, error) {
	full, err := newPreviewCursor(store.RunKindFull, policy.FullCron, policy.Timezone, nextFull, policy.Enabled, now)
	if err != nil {
		return SchedulePreview{}, err
	}
	incremental, err := newPreviewCursor(store.RunKindIncremental, policy.IncrementalCron, policy.Timezone, nextIncremental, policy.Enabled, now)
	if err != nil {
		return SchedulePreview{}, err
	}
	preview := SchedulePreview{
		PolicyID: policy.ID, Enabled: policy.Enabled, Timezone: policy.Timezone,
		Fires: make([]ScheduleFire, 0, count),
	}
	for range count {
		preview.Fires = append(preview.Fires, nextPreviewFire(&full, &incremental, policy))
	}
	return preview, nil
}

// newPreviewCursor places one kind's cursor on its first fire. An enabled
// policy with a stored time starts there; anything else starts at the first
// fire strictly after now, which is what enabling the policy would schedule.
func newPreviewCursor(kind, expression, timezone string, stored *time.Time, enabled bool, now time.Time) (previewCursor, error) {
	if enabled && stored != nil {
		return previewCursor{kind: kind, next: stored.UTC(), missed: !stored.After(now), after: now}, nil
	}
	next, err := schedule(expression, timezone, now)
	if err != nil {
		return previewCursor{}, err
	}
	return previewCursor{kind: kind, next: next.UTC(), after: now}, nil
}

// nextPreviewFire emits the earlier cursor and advances it. Both cursors due
// at the same instant yield one full fire and both advance, matching the
// scheduler's rule that one full wins and the incremental is not queued.
func nextPreviewFire(full, incremental *previewCursor, policy store.Policy) ScheduleFire {
	if incremental.next.Before(full.next) {
		fire := incremental.take()
		incremental.advance(policy.IncrementalCron, policy.Timezone)
		return fire
	}
	fire := full.take()
	full.advance(policy.FullCron, policy.Timezone)
	if !incremental.next.After(fire.At) {
		// The incremental is not queued, but its schedule still advances past
		// this instant, exactly as the scheduler's advance does. A cursor that
		// has not fired yet is still based on now, which is behind this fire, so
		// the base moves here first; otherwise its next fire would land before
		// the full it just lost to.
		if fire.At.After(incremental.after) {
			incremental.after = fire.At
		}
		incremental.advance(policy.IncrementalCron, policy.Timezone)
	}
	return fire
}

// take returns this cursor's current fire. It moves the cursor's base forward
// when the fire is later than it, which is what makes the next fire come
// after this one; a missed fire stays based on now, because the scheduler
// coalesces it into one run and resumes from Next(now).
func (c *previewCursor) take() ScheduleFire {
	fire := ScheduleFire{Kind: c.kind, At: c.next, Missed: c.missed}
	if fire.At.After(c.after) {
		c.after = fire.At
	}
	return fire
}

// advance moves the cursor to the next fire strictly after its current base
// and leaves the base there, so the step after this one moves on again.
func (c *previewCursor) advance(expression, timezone string) {
	next, err := schedule(expression, timezone, c.after)
	if err != nil {
		// The expressions and time zone were already parsed to place the
		// cursor, so a later failure can only come from a clock that moved
		// backwards across the location database. Keep the cursor where it is
		// rather than inventing a time.
		return
	}
	c.missed = false
	c.next = next.UTC()
}
