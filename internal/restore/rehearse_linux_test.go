//go:build linux

package restore

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRehearsalEarlyExitIsNotReady requires a server that exits during startup to
// be reported not_ready. Killing and reaping a process that already exited can
// report an error, but that is the teardown of a server that never ran; it must not
// relabel the failure as shutdown_failed.
func TestRehearsalEarlyExitIsNotReady(t *testing.T) {
	work := t.TempDir()
	mustNoError(t, os.Mkdir(filepath.Join(work, "data"), 0o700))
	script := filepath.Join(t.TempDir(), "mysqld-exits")
	mustNoError(t, os.WriteFile(script, []byte("#!/bin/sh\nexit 3\n"), 0o700))
	e := &execution{opts: Options{MysqldPath: script}, req: Request{Rehearse: true}, work: work}
	result := e.rehearse(t.Context())
	if result == nil {
		t.Fatal("a requested rehearsal reported no result")
	}
	// A root process is refused before any server is launched, so that is the only
	// classification a root run can reach.
	wantCode := "not_ready"
	if os.Geteuid() == 0 {
		wantCode = "root_refused"
	}
	if result.Status != "failed" || result.FailureCode != wantCode {
		t.Fatalf("verify = %+v, want status failed and code %s", result, wantCode)
	}
}
