package testenv

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/moby/moby/client"
	tc "github.com/testcontainers/testcontainers-go"
)

// dockerProbeTimeout bounds the Docker API round trip that starts every suite.
// The client constructor of testcontainers already calls Info, so without a
// deadline of its own a socket that accepts but never answers would hold the
// 15-minute build context and report nothing until it expires.
const dockerProbeTimeout = 45 * time.Second

// refusalExitCode is the exit status of a deliberate business refusal:
// app/cli/main.go prints the error and exits 1. A panic exits 2, a signal
// leaves 128+signal, and timeout leaves 124 or 137, so none of those prove a
// refusal and none may satisfy Refuse.
const refusalExitCode = 1

func refusalExit(code int) bool { return code == refusalExitCode }

func probeDockerWithin(ctx context.Context, timeout time.Duration) (*tc.DockerClient, client.SystemInfoResult, error) {
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	docker, err := tc.NewDockerClientWithOpts(probeCtx)
	if err != nil {
		return nil, client.SystemInfoResult{}, fmt.Errorf("docker provider unavailable: %w", err)
	}
	info, err := docker.Info(probeCtx, client.InfoOptions{})
	if err != nil {
		return nil, client.SystemInfoResult{}, fmt.Errorf("docker provider unavailable: %w", errors.Join(err, docker.Close()))
	}
	return docker, info, nil
}

// errAwaitTimeout marks only the deadline branch of await, so a caller can give
// a timeout its own failure message and still propagate a check error unchanged.
var errAwaitTimeout = errors.New("observation deadline exceeded")

// await polls check every 100 ms until it reports ready, returns an error, or
// timeout expires. Await and the terminal-state waits of the fixtures share it,
// so every wait has the same cadence and deadline failure.
func await(ctx context.Context, timeout time.Duration, check func() (bool, error)) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		ready, err := check()
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w after %s: %w", errAwaitTimeout, timeout, ctx.Err())
		case <-ticker.C:
		}
	}
}
