package physical

import (
	"context"
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

func TestPhysicalParentHelper(t *testing.T) {
	if os.Getenv("INTERN_PHYSICAL_PARENT_HELPER") != "1" {
		return
	}
	root := os.Getenv("INTERN_PHYSICAL_HELPER_ROOT")
	dir, err := newWorkDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "client.cnf"), []byte("private-test-credential"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = runProcess(context.Background(), processSpec{binary: os.Getenv("INTERN_PHYSICAL_HELPER_TOOL"), args: []string{root}, cleanup: dir, stdout: io.Discard, stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
}

func waitForCondition(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func TestParentDeathCleansProcessGroupAndCredentials(t *testing.T) {
	root := t.TempDir()
	tool := fakeTool(t, "sleep 30 &\nchild=$!\nprintf '%s %s' \"$$\" \"$child\" > \"$1/pids\"\nwait\n")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestPhysicalParentHelper$")
	cmd.Env = append(os.Environ(), "INTERN_PHYSICAL_PARENT_HELPER=1", "INTERN_PHYSICAL_HELPER_ROOT="+root, "INTERN_PHYSICAL_HELPER_TOOL="+tool)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill() })
	var pids []int
	waitForCondition(t, "tool and grandchild running", func() bool {
		pids = readToolPIDs(filepath.Join(root, "pids"))
		return len(pids) == 2
	})
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("killed parent succeeded")
	}
	waitForCondition(t, "process group reaped", func() bool {
		for _, pid := range pids {
			if !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
				return false
			}
		}
		return true
	})
	waitForCondition(t, "private credential directory removed", func() bool {
		entries, err := os.ReadDir(root)
		return err == nil && len(entries) == 1 && entries[0].Name() == "pids"
	})
}

func TestSupervisorArgvCannotDeleteOrdinaryDirectory(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "keep")
	os.WriteFile(marker, []byte("keep"), 0o600)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, supervisorArg)
	if err := cmd.Run(); err == nil {
		t.Fatal("supervisor accepted invocation without inherited handshake")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("ordinary argv deleted unrelated directory")
	}
}

func TestCancellationCannotBecomeSuccess(t *testing.T) {
	source := testSource(t, fakeTool(t, backupFixtureBody()))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := source.Run(ctx, cancelOnWrite{cancel: cancel}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled success-looking backup returned %v", err)
	}
	assertWorkEmpty(t, source.WorkRoot)
}

func TestSuccessfulLeaderCannotLeakGrandchild(t *testing.T) {
	root := t.TempDir()
	tool := fakeTool(t, "sleep 30 &\nprintf '%s' \"$!\" > \"$1/pid\"\nexit 0\n")
	if err := runProcess(context.Background(), processSpec{binary: tool, args: []string{root}, stdout: io.Discard, stderr: io.Discard}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(raw))
	if err != nil || !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		t.Fatalf("grandchild not reaped: pid=%d parse=%v", pid, err)
	}
}

func readToolPIDs(path string) []int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	fields := strings.Fields(string(raw))
	if len(fields) != 2 {
		return nil
	}
	pids := make([]int, 0, 2)
	for _, field := range fields {
		pid, err := strconv.Atoi(field)
		if err != nil || pid < 2 {
			return nil
		}
		pids = append(pids, pid)
	}
	return pids
}
