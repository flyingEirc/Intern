// Package schedule runs a foreground, single-owner logical backup schedule.
package schedule

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/flyingEirc/IotProject/base/backup"
)

type Config struct {
	FullAt           string        `mapstructure:"full_at"`
	IncrementalEvery time.Duration `mapstructure:"incremental_every"`
	Timezone         string        `mapstructure:"timezone"`
}

func Parse(c Config) (cron.Schedule, *time.Location, error) {
	clock, err := time.Parse("15:04", c.FullAt)
	if err != nil || len(c.FullAt) != 5 {
		return nil, nil, errors.New("full-at must be HH:MM")
	}
	if c.IncrementalEvery < time.Second {
		return nil, nil, errors.New("incremental-every must be at least 1s")
	}
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return nil, nil, errors.New("invalid IANA schedule timezone")
	}
	daily, err := cron.ParseStandard(fmt.Sprintf("%d %d * * *", clock.Minute(), clock.Hour()))
	return daily, loc, err
}

type runner struct {
	engine                    *backup.Engine
	config                    Config
	logger                    *slog.Logger
	emit                      func(backup.Result) error
	daily                     cron.Schedule
	loc                       *time.Location
	identity                  string
	nextFull, nextIncremental time.Time
}

func Run(ctx context.Context, engine *backup.Engine, opts backup.Options, c Config, logger *slog.Logger, emit func(backup.Result) error) error {
	daily, loc, err := Parse(c)
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%s|%d|%s|%s|%s|%d|%s", opts.Project, opts.Directory, opts.Connection.Host, opts.Connection.Socket, opts.Connection.Port, opts.Connection.DB, c.FullAt, c.Timezone, c.IncrementalEvery, opts.StateDirectory)))
	r := runner{engine: engine, config: c, logger: logger, emit: emit, daily: daily, loc: loc, identity: fmt.Sprintf("schedule:%x", sum[:16])}
	logger.Info("scheduler owns foreground process; stopping it stops backups", "full_at", c.FullAt, "timezone", c.Timezone, "incremental_every", c.IncrementalEvery.String())
	if err = r.bootstrap(ctx); err != nil {
		return err
	}
	now := time.Now()
	r.nextFull = daily.Next(now.In(loc))
	r.nextIncremental = now.Truncate(c.IncrementalEvery).Add(c.IncrementalEvery)
	return r.run(ctx)
}
func requiresBaseline(err error) bool {
	return errors.Is(err, backup.ErrNoBaseline) || errors.Is(err, backup.ErrNewBaseline)
}
func (r *runner) bootstrap(ctx context.Context) error {
	first, err := r.engine.Incremental(ctx, "")
	if requiresBaseline(err) {
		r.logger.Warn("starting new full baseline; earlier chain was absent or cannot continue", "reason", err)
		first, err = r.engine.Full(ctx, "")
	}
	if err != nil {
		return err
	}
	return r.emit(first)
}
func (r *runner) run(ctx context.Context) error {
	for {
		if err := r.waitNext(ctx); err != nil {
			return err
		}
		result, jobErr := r.backupDue(ctx, time.Now())
		// Coalesce ticks missed during a long job; never overlap source operations.
		r.nextIncremental = time.Now().Truncate(r.config.IncrementalEvery).Add(r.config.IncrementalEvery)
		if err := ctx.Err(); err != nil {
			return err
		}
		if jobErr != nil {
			r.logger.Error("scheduled backup failed; protected watermark was not advanced", "error", jobErr)
			continue
		}
		if err := r.emit(result); err != nil {
			return err
		}
	}
}
func (r *runner) waitNext(ctx context.Context) error {
	next := r.nextIncremental
	if r.nextFull.Before(next) {
		next = r.nextFull
	}
	timer := time.NewTimer(time.Until(next))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func (r *runner) backupDue(ctx context.Context, now time.Time) (backup.Result, error) {
	if !now.Before(r.nextFull) {
		scheduled := r.nextFull
		r.nextFull = r.daily.Next(now.In(r.loc))
		return r.engine.Full(ctx, r.identity+":full:"+scheduled.UTC().Format(time.RFC3339Nano))
	}
	scheduled := r.nextIncremental.UTC().Format(time.RFC3339Nano)
	result, err := r.engine.Incremental(ctx, r.identity+":binlog:"+scheduled)
	if requiresBaseline(err) {
		r.logger.Error("recovery window interrupted; creating new full baseline", "error", err)
		return r.engine.Full(ctx, r.identity+":rebaseline:"+scheduled)
	}
	return result, err
}
