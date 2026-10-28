//go:build linux

package physical

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// signaledExit returns the exit error of a real process this test ended with one
// signal, so the classification sees the same wait status production does.
func signaledExit(t *testing.T, signal syscall.Signal) error {
	t.Helper()
	command := exec.Command("/bin/sh", "-c", "exec sleep 30")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if err := command.Process.Signal(signal); err != nil {
		t.Fatal(err)
	}
	err := command.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("wait = %v, want an exit error", err)
	}
	return err
}

// exitedWithCode returns the exit error of a real process that exited with code,
// so the supervisor teardown branch sees the same wait status production does.
func exitedWithCode(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command("/bin/sh", "-c", "exit "+strconv.Itoa(code)).Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("wait = %v, want an exit error", err)
	}
	return err
}

// TestRequestedKillExit pins which wait results a kill requested by this process
// accepts: its own SIGKILL, or the supervisor's teardown exit (125) when the
// lifetime-pipe close won the race. Any other signal, exit status or joined error
// stays a failure, so a real teardown problem is never hidden behind the kill.
func TestRequestedKillExit(t *testing.T) {
	killed := signaledExit(t, syscall.SIGKILL)
	terminated := signaledExit(t, syscall.SIGTERM)
	teardown := exitedWithCode(t, 125)
	failed := exitedWithCode(t, 1)
	accepted := []error{
		killed,
		errors.Join(killed, nil),
		teardown,
		errors.Join(teardown, nil),
	}
	for index, err := range accepted {
		if !requestedKillExit(err) {
			t.Fatalf("accepted[%d] = %v was not recognized as a requested teardown", index, err)
		}
	}
	rejected := []struct {
		name string
		err  error
	}{
		{"nil", nil},
		{"other signal", terminated},
		{"failure exit", failed},
		{"plain error", errors.New("physical: native tool failed to start, finish, or drain its pipes")},
		{"kill next to another failure", errors.Join(killed, errors.New("second failure"))},
		{"teardown next to another failure", errors.Join(teardown, errors.New("second failure"))},
	}
	for _, test := range rejected {
		t.Run(test.name, func(t *testing.T) {
			if requestedKillExit(test.err) {
				t.Fatalf("%v was accepted as a requested teardown", test.err)
			}
		})
	}
}

// TestKillAcceptsSupervisorTeardownExit reproduces the #39 ordering
// deterministically: the lifetime-pipe close reaches the supervisor first, it
// reaps the tool group and exits with its teardown code, and the kill then finds
// the supervisor already gone. That is still the teardown this process requested,
// so WaitExit must report it as clean; returning the raw exit status labels a
// never-ready rehearsal shutdown_failed instead of not_ready.
func TestKillAcceptsSupervisorTeardownExit(t *testing.T) {
	work := t.TempDir()
	pidFile := filepath.Join(work, "pid")
	tool := fakeTool(t, "printf '%s' $$ > "+shellQuote(pidFile)+"\nexec sleep 30\n")
	process, err := startSupervisedProcess(t.Context(), processSpec{
		binary: tool, dir: work, stdout: io.Discard, stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = process.kill() })
	waitForCondition(t, "supervised tool running", func() bool {
		_, statErr := os.Stat(pidFile)
		return statErr == nil
	})
	// Closing the lifetime pipe is the first step of kill; waiting for the exit
	// before requesting the kill forces the supervisor-first ordering the race
	// can produce, rather than observing whichever side happens to win.
	closePipe(process.writeEnd)
	select {
	case <-process.exited():
	case <-time.After(4 * time.Second):
		t.Fatal("supervisor did not exit after the lifetime pipe closed")
	}
	if err = process.kill(); err != nil {
		t.Fatalf("kill = %v, want the already-exited supervisor to be accepted", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	if err = process.waitExit(ctx); err != nil {
		t.Fatalf("waitExit = %v, want the requested teardown to be clean", err)
	}
	// Prove the ordering: the supervisor exited by itself with its own teardown
	// code (125), not because the kill signal reached it.
	var exit *exec.ExitError
	if !errors.As(process.waitErr, &exit) {
		t.Fatalf("wait error = %v, want a supervisor exit status", process.waitErr)
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok || !status.Exited() || status.ExitStatus() != 125 {
		t.Fatalf("supervisor status = %v, want exit 125", exit)
	}
}
