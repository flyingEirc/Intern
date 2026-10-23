//go:build integration

package chain

import "context"

// RunWithFault observes named boundaries of the real Run, without replacing any
// component. The callback may inject an error after real I/O, or cancel the
// caller's context after publication. It is absent from production builds and
// held only by this run; tests must assert that their boundary was reached.
func RunWithFault(ctx context.Context, job Job, fault func(point string) error) (Result, error) {
	return run(ctx, job, runHook(fault))
}
