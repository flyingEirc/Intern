//go:build integration

package restore

import "context"

// ExecuteWithFetchFault runs one real Execute and observes every fetch attempt.
// fault is called after the remote reader for that attempt is open and before
// any byte is copied; returning a positive byte count and an error makes that
// attempt fail after the count has been written, exactly like a transport that
// dies mid-stream. The next attempt must resume at the recorded offset through
// RetrFrom. The hook is absent from production builds, and Execute without it is
// unchanged.
func ExecuteWithFetchFault(ctx context.Context, opts Options, req Request, progress Progress, fault func(attempt int, key string, offset int64) (int64, error)) (Result, error) {
	original := opts.fetchFault
	opts.fetchFault = fault
	defer func() { opts.fetchFault = original }()
	return Execute(ctx, opts, req, progress)
}
