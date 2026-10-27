package dispatch

import (
	"context"
	"time"
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

// stepReplication runs at most one FIFO-eligible item across every replicator.
func (d *Dispatcher) stepReplication(ctx context.Context) bool {
	for _, replicator := range d.replicas {
		attempted, err := replicator.Step(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return false
			}
			d.logger.ErrorContext(ctx, "replication step failed",
				"repository_id", replicator.RepositoryID, "error", err)
			continue
		}
		if attempted {
			return true
		}
	}
	return false
}

// stop closes the repository handles the workers opened. The database itself
// stays owned by the Store; serve closes it after Stop returns.
func (d *Dispatcher) stop() {
	for _, closer := range d.closers {
		if err := closer(); err != nil {
			d.logger.Error("cannot close replication repository", "error", err)
		}
	}
	d.closers = nil
}
