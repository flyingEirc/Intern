package testenv

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func TestRefusalExitAcceptsOnlyTheBusinessRefusal(t *testing.T) {
	if !refusalExit(refusalExitCode) {
		t.Fatalf("refusalExit(%d) is false; the CLI's own refusal exit must be accepted", refusalExitCode)
	}
	for _, code := range []int{0, 2, 124, 130, 137, 139} {
		if refusalExit(code) {
			t.Errorf("refusalExit(%d) is true; only the business-refusal exit %d proves a refusal", code, refusalExitCode)
		}
	}
}

func TestDockerProbeTimeoutIsBounded(t *testing.T) {
	if dockerProbeTimeout < 30*time.Second || dockerProbeTimeout > time.Minute {
		t.Fatalf("dockerProbeTimeout is %s; a silent socket must be cut off within 30-60 seconds", dockerProbeTimeout)
	}
}

// TestProbeDockerDeadlineWithSilentSocket proves the info probe gives up on a
// socket that accepts the connection but never answers, instead of waiting for
// the caller's 15-minute build deadline.
func TestProbeDockerDeadlineWithSilentSocket(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				<-done
				_ = conn.Close()
			}()
		}
	}()
	t.Setenv("DOCKER_TLS_VERIFY", "")
	t.Setenv("DOCKER_CERT_PATH", "")
	t.Setenv("DOCKER_HOST", "tcp://"+listener.Addr().String())
	t.Setenv("TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE", "tcp://"+listener.Addr().String())
	started := time.Now()
	results := make(chan error, 1)
	go func() {
		docker, _, probeErr := probeDockerWithin(context.Background(), 300*time.Millisecond)
		if docker != nil {
			_ = docker.Close()
		}
		results <- probeErr
	}()
	select {
	case err = <-results:
	case <-time.After(5 * time.Second):
		t.Fatalf("probe did not return after %s on a silent socket; the short deadline did not apply", time.Since(started))
	}
	if err == nil || !strings.Contains(err.Error(), "docker provider unavailable") {
		t.Fatalf("probe error is %v, want the explicit docker provider unavailable failure", err)
	}
}

func TestAwaitReturnsWhenReady(t *testing.T) {
	calls := 0
	if err := await(context.Background(), 2*time.Second, func() (bool, error) {
		calls++
		return calls == 3, nil
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("check ran %d times, want 3", calls)
	}
}

func TestAwaitTimesOut(t *testing.T) {
	started := time.Now()
	err := await(context.Background(), 250*time.Millisecond, func() (bool, error) { return false, nil })
	if !errors.Is(err, errAwaitTimeout) {
		t.Fatalf("await error is %v, want the timeout sentinel", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("await timed out after %s, want about 250ms", elapsed)
	}
}

func TestAwaitPropagatesCheckError(t *testing.T) {
	boom := errors.New("state query failed")
	err := await(context.Background(), time.Second, func() (bool, error) { return false, boom })
	if !errors.Is(err, boom) {
		t.Fatalf("await error is %v, want the check error unchanged", err)
	}
}
