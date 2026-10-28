//go:build integration

package replication

import (
	"context"
	"fmt"
)

// StepWithFault runs one real Step and observes its named boundaries. Each
// boundary is reached only after that boundary's real I/O, so an injected error
// or a cancellation describes observed state: "part" after the recorded
// temporary key, "write" after the member bytes reached the part, "uploaded"
// after the durable status, and "before_verify" after every commit and before
// the readback. The callback may inject a failure, cancel the caller's context,
// or observe state; it must not call back into the Replicator. It is absent from
// production builds, and production behaviour with no hook attached is
// unchanged.
func (r *Replicator) StepWithFault(ctx context.Context, fault func(point string) error) (bool, error) {
	if r == nil {
		return false, fmt.Errorf("replication: no replicator")
	}
	original := r.hook
	r.hook = stepHook(fault)
	processed, err := r.Step(ctx)
	r.hook = original
	return processed, err
}
