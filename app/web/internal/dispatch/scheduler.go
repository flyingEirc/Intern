package dispatch

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/flyingEirc/Intern/app/web/internal/store"
)

// schedulerTickInterval is the exported tick interval for tests.
const SchedulerTick = schedulerTick

// cronKey builds the frozen cron idempotency key: policy_id:kind:scheduled_at.
func cronKey(policyID, kind string, scheduledAt time.Time) string {
	return fmt.Sprintf("policy:%s:%s:%s", policyID, kind, store.ScheduleStamp(scheduledAt))
}

// admissionKey builds the frozen admission idempotency key for the coalesced
// full a refused incremental queues: policy:<id>:full:admission:<latest chain
// id, or none>.
func admissionKey(policyID, chainID string) string {
	if chainID == "" {
		chainID = "none"
	}
	return fmt.Sprintf("policy:%s:full:admission:%s", policyID, chainID)
}

// schedule computes the next fire time of one standard cron expression in the
// policy's time zone. It never uses the CLI schedule package.
func schedule(expression, timezone string, after time.Time) (time.Time, error) {
	parser, err := cron.ParseStandard(expression)
	if err != nil {
		return time.Time{}, fmt.Errorf("dispatch: cron %q: %w", expression, err)
	}
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return time.Time{}, fmt.Errorf("dispatch: timezone %q: %w", timezone, err)
	}
	return parser.Next(after.In(location)), nil
}

// scheduler advances the stored schedules and enqueues due runs. It runs every
// tick and, when the process has just started, immediately.
func (d *Dispatcher) scheduler(ctx context.Context) {
	ticker := time.NewTicker(schedulerTick)
	defer ticker.Stop()
	for {
		d.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// duePolicy is one enabled policy whose full or incremental schedule fired.
type duePolicy struct {
	store.Policy
	// due holds the fired trigger time per kind. Both may fire together; one
	// full then wins and both schedules advance.
	full, incremental *time.Time
}

// tick computes due policies in their timezone and inserts one coalesced run
// per policy per kind.
func (d *Dispatcher) tick(ctx context.Context) {
	policies, err := d.config.Database.EnabledPolicies(ctx)
	if err != nil {
		d.logger.ErrorContext(ctx, "scheduler cannot list policies", "error", err)
		return
	}
	now := d.now()
	due := make([]duePolicy, 0, len(policies))
	for _, policy := range policies {
		item, err := d.duePolicy(ctx, policy, now)
		if err != nil {
			d.logger.ErrorContext(ctx, "scheduler cannot compute schedule",
				"policy_id", policy.ID, "error", err)
			continue
		}
		if item.full != nil || item.incremental != nil {
			due = append(due, item)
		}
	}
	for _, item := range due {
		d.enqueueDue(ctx, item, now)
	}
}

// duePolicy compares the stored schedule columns with now. A NULL column on an
// enabled policy is initialized from its cron expressions instead of firing.
func (d *Dispatcher) duePolicy(ctx context.Context, policy store.Policy, now time.Time) (duePolicy, error) {
	nextFull, nextIncremental, err := d.config.Database.PolicySchedule(ctx, policy.ID)
	if err != nil {
		return duePolicy{}, err
	}
	item := duePolicy{Policy: policy}
	if nextFull == nil || nextIncremental == nil {
		initializedFull, initializedIncremental, initErr := d.initializeSchedule(ctx, policy, now, nextFull, nextIncremental)
		if initErr != nil {
			return item, initErr
		}
		nextFull, nextIncremental = initializedFull, initializedIncremental
	}
	if nextFull != nil && !nextFull.After(now) {
		item.full = nextFull
	}
	if nextIncremental != nil && !nextIncremental.After(now) {
		item.incremental = nextIncremental
	}
	return item, nil
}

// initializeSchedule fills missing schedule columns. It never fires: the next
// tick sees the fresh future timestamps.
func (d *Dispatcher) initializeSchedule(ctx context.Context, policy store.Policy, now time.Time, nextFull, nextIncremental *time.Time) (*time.Time, *time.Time, error) {
	if nextFull == nil {
		computed, err := schedule(policy.FullCron, policy.Timezone, now)
		if err != nil {
			return nil, nil, err
		}
		nextFull = &computed
	}
	if nextIncremental == nil {
		computed, err := schedule(policy.IncrementalCron, policy.Timezone, now)
		if err != nil {
			return nil, nil, err
		}
		nextIncremental = &computed
	}
	if err := d.config.Database.InitializeSchedule(ctx, policy.ID, *nextFull, *nextIncremental); err != nil {
		return nil, nil, err
	}
	return nextFull, nextIncremental, nil
}

// enqueueDue inserts the runs one policy is due for. When full and incremental
// are due together, one full wins and both schedules advance; the due
// incremental is not queued behind that full. Missed triggers coalesce to one
// run per kind because the stored next_*_at becomes scheduled_at and then
// advances to Next(now).
func (d *Dispatcher) enqueueDue(ctx context.Context, item duePolicy, now time.Time) {
	if item.full != nil {
		_, _, err := d.insertCronRun(ctx, item.Policy, store.RunKindFull, *item.full, now)
		if err != nil {
			d.logger.ErrorContext(ctx, "scheduler cannot enqueue full run",
				"policy_id", item.ID, "error", err)
			return
		}
		if err := d.advance(ctx, item.Policy, now, item.full, item.incremental); err != nil {
			d.logger.ErrorContext(ctx, "scheduler cannot advance schedule",
				"policy_id", item.ID, "error", err)
		}
		return
	}
	if item.incremental != nil {
		created, _, err := d.insertCronRun(ctx, item.Policy, store.RunKindIncremental, *item.incremental, now)
		if err != nil {
			d.logger.ErrorContext(ctx, "scheduler cannot enqueue incremental run",
				"policy_id", item.ID, "error", err)
			return
		}
		_ = created
		if err := d.advance(ctx, item.Policy, now, item.full, item.incremental); err != nil {
			d.logger.ErrorContext(ctx, "scheduler cannot advance schedule",
				"policy_id", item.ID, "error", err)
		}
	}
}

// advance sets both next_* columns to their next fire times with Next(now).
func (d *Dispatcher) advance(ctx context.Context, policy store.Policy, now time.Time, full, incremental *time.Time) error {
	nextFull, err := schedule(policy.FullCron, policy.Timezone, now)
	if err != nil {
		return err
	}
	nextIncremental, err := schedule(policy.IncrementalCron, policy.Timezone, now)
	if err != nil {
		return err
	}
	return d.config.Database.InitializeSchedule(ctx, policy.ID, nextFull, nextIncremental)
}

// insertCronRun creates one cron run with its frozen idempotency key. A
// duplicate key is a no-op: the run of that exact trigger already exists.
func (d *Dispatcher) insertCronRun(ctx context.Context, policy store.Policy, kind string, scheduledAt, now time.Time) (store.Run, bool, error) {
	run, err := d.config.Database.CreateRun(ctx, store.RunInput{
		PolicyID: policy.ID, SourceID: policy.SourceID, Kind: kind, Trigger: store.RunTriggerCron,
		IdempotencyKey: cronKey(policy.ID, kind, scheduledAt), RequestHash: cronRequestHash(policy, kind),
		ScheduledAt: scheduledAt,
	})
	if errors.Is(err, store.ErrConflict) {
		return store.Run{}, false, nil
	}
	if err != nil {
		return store.Run{}, false, err
	}
	d.ensurePolicyReplicator(ctx, policy, run.ID)
	d.logEvent(ctx, run.ID, eventQueued, nil)
	return run, true, nil
}

// cronRequestHash is a stable, secret-safe body fingerprint for cron runs. The
// schedule columns and policy identity are the effective body. The optional
// replica repository prints as an empty field when unset, so a changed replica
// changes the hash.
func cronRequestHash(policy store.Policy, kind string) string {
	hasher := fnv.New128a()
	replica := ""
	if policy.ReplicaRepositoryID != nil {
		replica = *policy.ReplicaRepositoryID
	}
	// fnv's Write never returns an error, so this write cannot fail.
	_, _ = fmt.Fprintf(hasher, "policy:%s\nkind:%s\nlocal:%s\nreplica:%s\nfull:%s\nincremental:%s\nmax:%d\nparallel:%d\nthrottle:%d\nzstd:%d\npage:%v\n", //nolint:errcheck // hash writes cannot fail
		policy.ID, kind, policy.LocalRepositoryID, replica,
		policy.FullCron, policy.IncrementalCron, policy.MaxChainLength, policy.Parallel,
		policy.Throttle, policy.ZstdLevel, policy.PageTracking)
	return fmt.Sprintf("%x", hasher.Sum(nil))
}
