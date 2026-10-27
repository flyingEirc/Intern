package dispatch

import (
	"context"
	"errors"
	"time"

	"github.com/flyingEirc/Intern/app/web/internal/store"
)

// replicationWorker steps every replicator round-robin, one item at a time.
// The replicas table is the only replication queue and the concurrency is
// frozen at 1, so one goroutine is the whole channel.
func (d *Dispatcher) replicationWorker(ctx context.Context) {
	for {
		attempted := d.stepReplication(ctx)
		delay := replicationPoll
		if attempted {
			// Drain a backlog without waiting a full interval.
			delay = time.Millisecond
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

// stepReplication runs at most one FIFO-eligible item, starting after the last
// replicator that did work so a busy first repository cannot starve the rest.
func (d *Dispatcher) stepReplication(ctx context.Context) bool {
	d.retryFailedReplicators(ctx)
	d.mu.Lock()
	slots := append([]replicaHandle(nil), d.replicas...)
	start := d.replicaCursor
	d.mu.Unlock()
	next, attempted, err := roundRobin(len(slots), start, func(i int) (bool, error) {
		slot := slots[i]
		did, stepErr := slot.replicator.Step(ctx)
		if stepErr != nil {
			if ctx.Err() != nil {
				return false, stepErr
			}
			d.logger.ErrorContext(ctx, "replication step failed",
				"repository_id", slot.replicaID, "error", stepErr)
			return false, nil
		}
		if did {
			d.logReplicationStep(ctx, slot)
		}
		return did, nil
	})
	d.mu.Lock()
	d.replicaCursor = next
	d.mu.Unlock()
	return err == nil && attempted
}

// roundRobin walks items from start and returns the index after the first that
// did work. A busy first item therefore yields to the rest on the next call.
func roundRobin(n, start int, step func(int) (bool, error)) (int, bool, error) {
	if n == 0 {
		return 0, false, nil
	}
	if start < 0 || start >= n {
		start = 0
	}
	for offset := range n {
		i := (start + offset) % n
		did, err := step(i)
		if err != nil {
			return start, false, err
		}
		if did {
			return (i + 1) % n, true, nil
		}
	}
	return start, false, nil
}

func (d *Dispatcher) logReplicationStep(ctx context.Context, slot replicaHandle) {
	var backupID, status, code string
	err := d.config.Database.DB().QueryRowContext(ctx,
		`SELECT backup_id, status, COALESCE(last_error_code, '') FROM replicas WHERE repository_id = ? ORDER BY updated_at DESC LIMIT 1`,
		slot.replicaID,
	).Scan(&backupID, &status, &code)
	if err != nil {
		return
	}
	run, err := d.config.Database.RunByBackupID(ctx, backupID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			d.logger.ErrorContext(ctx, "cannot load originating run for replication step",
				"backup_id", backupID, "error", err)
		}
		return
	}
	d.logEvent(ctx, run.ID, eventReplicationStep, map[string]any{
		"backup_id": backupID, "repository_id": slot.replicaID, "status": status, "code": code,
	})
}

// stop closes the repository handles the workers opened. The database itself
// stays owned by the Store; serve closes it after Stop returns.
func (d *Dispatcher) stop() {
	d.mu.Lock()
	closers := d.closers
	d.closers = nil
	d.replicas = nil
	d.replicatorFailed = nil
	d.mu.Unlock()
	for _, closer := range closers {
		if err := closer(); err != nil {
			d.logger.Error("cannot close replication repository", "error", err)
		}
	}
}
