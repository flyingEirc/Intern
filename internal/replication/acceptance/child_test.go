//go:build integration

package acceptance

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/flyingEirc/Intern/internal/replication"
)

// childCommand builds one separate worker process running a single test. Its
// output goes to this process's stderr so it is part of the same evidence.
func childCommand(t *testing.T, executable, name string) *exec.Cmd {
	t.Helper()
	command := exec.CommandContext(t.Context(), executable, "-test.v", "-test.run=^"+name+"$", "-test.timeout=4m")
	command.Env = os.Environ()
	command.Stdout = os.Stderr
	command.Stderr = os.Stderr
	return command
}

// waitMarker waits for the marker a child writes only after its real I/O.
func waitMarker(t *testing.T, marker string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		if raw, err := os.ReadFile(marker); err == nil && string(raw) == "written" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the separate worker never reached real transfer I/O")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// killChild requires a real SIGKILL of the separate worker process.
func killChild(t *testing.T, command *exec.Cmd) {
	t.Helper()
	must(t, command.Process.Kill())
	waitErr := command.Wait()
	var exit *exec.ExitError
	if !errors.As(waitErr, &exit) {
		t.Fatalf("the worker was not killed: %v", waitErr)
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("worker exit was not SIGKILL: %v", waitErr)
	}
}

// TestR1KillChildWorker is the separate process the kill scenario interrupts. It
// records the durable part, transfers real payload bytes through the real FTP
// sink, then blocks so the parent's SIGKILL lands mid-upload.
func TestR1KillChildWorker(t *testing.T) {
	repositoryID := os.Getenv("INTERN_R1_KILL_REPOSITORY")
	backupID := os.Getenv("INTERN_R1_KILL_BACKUP")
	marker := os.Getenv("INTERN_R1_KILL_MARKER")
	if repositoryID == "" || backupID == "" || !filepath.IsAbs(marker) {
		t.Fatal("kill child requires the owning fixture identity")
	}
	w := newWorker(t)
	w.loadManifest()
	r := w.replicator(repositoryID, replication.NewFTPRemote(w.ftp), w.paths.key, replication.Policy{})
	must(t, r.Enqueue(t.Context(), backupID))
	_, err := r.StepWithFault(t.Context(), func(point string) error {
		if point != "write" {
			return nil
		}
		must(t, os.WriteFile(marker, []byte("written"), 0o600))
		select {} // The parent must SIGKILL this process at real payload I/O.
	})
	must(t, err)
	t.Fatal("kill child unexpectedly completed")
}

// TestR1RestartChildWorker is the restarted process of the kill scenario. It runs
// Recover against the real FTP, requires the recorded part to be cleaned, then
// completes the item, so recovery is proven in a process that never saw the
// interrupted upload.
func TestR1RestartChildWorker(t *testing.T) {
	repositoryID := os.Getenv("INTERN_R1_KILL_REPOSITORY")
	backupID := os.Getenv("INTERN_R1_KILL_BACKUP")
	part := os.Getenv("INTERN_R1_KILL_PART")
	if repositoryID == "" || backupID == "" || part == "" {
		t.Fatal("the restart child requires the owning fixture identity")
	}
	w := newWorker(t)
	w.loadManifest()
	r := w.replicator(repositoryID, replication.NewFTPRemote(w.ftp), w.paths.key, replication.Policy{})
	must(t, r.Recover(t.Context()))
	if rows := w.partRows(repositoryID); rows != 0 {
		t.Fatalf("recovery kept %d part rows", rows)
	}
	w.rawNormal().absent(t, part)
	processed, err := r.Step(t.Context())
	if !processed {
		t.Fatalf("no item was attemptable after recovery: %v", err)
	}
	must(t, err)
	if status := w.replica(r, backupID).Status; status != replication.StatusVerified {
		t.Fatalf("the recovered item is %q, want verified", status)
	}
	must(t, r.Recover(t.Context()))
	t.Log("R1-A3 restart recovery completed second_recover=noop")
}
