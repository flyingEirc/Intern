package physical

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSupervisorSignalStopsOwnedGroup(t *testing.T) {
	root := t.TempDir()
	dir, err := newWorkDir(root)
	if err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(root, "pids")
	t.Cleanup(func() { killRecordedTestProcesses(pidFile) })
	tool := fakeTool(t, grandchildScript(pidFile, "wait\n"))
	payload, err := json.Marshal(supervisorConfig{Parent: os.Getpid(), Cleanup: dir, Binary: tool})
	if err != nil {
		t.Fatal(err)
	}
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer closePipe(readEnd)
	defer closePipe(writeEnd)
	cmd := exec.Command("/proc/self/exe", supervisorArg)
	cmd.ExtraFiles = []*os.File{readEnd}
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	if err = writeSupervisorConfig(writeEnd, payload); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, "owned tool group running", func() bool { return len(readToolPIDs(pidFile)) == 2 })
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	if err = cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	assertSupervisorInterrupted(t, done)
	assertPIDsGone(t, pidFile)
	if _, err = os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("signal did not remove owned work directory")
	}
	// writeEnd is deliberately still open: only the supervisor signal can have
	// interrupted this run, not parent cancellation or lifetime-pipe EOF.
}

func assertSupervisorInterrupted(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 125 {
			t.Fatalf("supervisor result=%v, want exit125", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("supervisor signal did not interrupt its owned process group")
	}
}

func TestEscapedDescendantHelper(t *testing.T) {
	if os.Getenv("INTERN_PHYSICAL_ESCAPE_HELPER") != "1" {
		return
	}
	cmd := exec.Command("/bin/sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("INTERN_PHYSICAL_ESCAPE_PIDFILE"), []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	// Deliberately exit without waiting: the supervisor must adopt and reap the
	// child even though it has escaped the original process group.
}

func TestEscapedDescendantIsReaped(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "escaped-pid")
	t.Cleanup(func() { killRecordedTestProcesses(pidFile) })
	t.Setenv("INTERN_PHYSICAL_ESCAPE_HELPER", "1")
	t.Setenv("INTERN_PHYSICAL_ESCAPE_PIDFILE", pidFile)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err := runProcess(ctx, processSpec{binary: "/proc/self/exe", args: []string{"-test.run=^TestEscapedDescendantHelper$"}, stdout: io.Discard, stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(raw))
	if err != nil || pid < 2 {
		t.Fatal("escaped child fixture missing")
	}
	if !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatal("escaped descendant survived successful tool teardown")
	}
}

func killRecordedTestProcesses(path string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, field := range strings.Fields(string(raw)) {
		pid, parseErr := strconv.Atoi(field)
		if parseErr == nil && pid > 1 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}
