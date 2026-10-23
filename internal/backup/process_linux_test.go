package backup

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestExecuteToolLifecycle(t *testing.T) {
	t.Run("context cancellation", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		if err := executeTool(exec.CommandContext(ctx, "/bin/sleep", "60")); err == nil || ctx.Err() == nil {
			t.Fatalf("command did not report cancellation: command=%v context=%v", err, ctx.Err())
		}
	})
	t.Run("parent death", toolParentDeath)
}

func toolParentDeath(t *testing.T) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	parent := exec.CommandContext(ctx, executable, "-test.run=^TestExecuteToolParentHelper$")
	parent.Env = append(os.Environ(), "INTERN_TOOL_PARENT_HELPER=1")
	output, err := parent.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	if err = parent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = parent.Process.Kill(); _ = parent.Wait() })
	line, err := bufio.NewReader(output).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || pid <= 1 {
		t.Fatalf("invalid child PID %q: %v", line, err)
	}
	tool, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tool.Kill(); _ = tool.Release() })
	if err = parent.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err = parent.Wait(); err == nil {
		t.Fatal("killed parent unexpectedly succeeded")
	}
	awaitToolExit(t, pid)
}

func awaitToolExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		// Linux reports ESRCH when the process is reaped after /proc lookup.
		if os.IsNotExist(err) || errors.Is(err, syscall.ESRCH) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		fields := strings.Fields(string(state)[strings.LastIndexByte(string(state), ')')+1:])
		if len(fields) > 0 && fields[0] == "Z" {
			return // A terminated child may briefly await adoption/reaping by PID 1.
		}
		select {
		case <-deadline.C:
			t.Fatal("tool survived the death of its parent")
		case <-ticker.C:
		}
	}
}

func TestExecuteToolParentHelper(t *testing.T) {
	if os.Getenv("INTERN_TOOL_PARENT_HELPER") != "1" {
		return
	}
	// The PID is emitted only after exec's child setup has installed Pdeathsig.
	cmd := exec.Command("/bin/sh", "-c", "echo $$; exec /bin/sleep 60")
	cmd.Stdout = os.Stdout
	cmd.Stderr = io.Discard
	if err := executeTool(cmd); err != nil {
		t.Fatal(err)
	}
}
