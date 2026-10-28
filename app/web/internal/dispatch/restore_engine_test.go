package dispatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/flyingEirc/Intern/app/web/internal/store"
	"github.com/flyingEirc/Intern/internal/restore"
)

// TestLocalRootsPagesEveryRepository proves the restricted export locations
// include every configured local repository, not only the first page: a
// deployment with more repositories than one page must not leave any of their
// roots unchecked when a restore export is judged.
func TestLocalRootsPagesEveryRepository(t *testing.T) {
	f := newFixture(t)
	root := t.TempDir()
	want := repositoryPageSize + 7
	created := make(map[string]bool, want)
	for index := range want {
		name := fmt.Sprintf("repo-%03d", index)
		repositoryRoot := filepath.Join(root, name)
		if _, err := f.database.CreateRepository(f.t.Context(), store.RepositoryInput{
			Name: name, Kind: store.RepositoryLocal, TLSMode: store.TLSModeNone, Root: repositoryRoot,
		}, nil, store.SystemAudit("repositories.create", store.AuditDetail{})); err != nil {
			t.Fatal(err)
		}
		created[repositoryRoot] = true
	}
	// One FTP repository must never become a restricted root.
	f.ftpRepository("repo-ftp")

	adapter := &RestoreEngineAdapter{database: f.database}
	roots, err := adapter.localRoots(f.t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != want {
		t.Fatalf("roots = %d, want every one of %d local repositories examined", len(roots), want)
	}
	for _, listed := range roots {
		if !created[listed] {
			t.Fatalf("unexpected root %q", listed)
		}
		delete(created, listed)
	}
	if len(created) != 0 {
		t.Fatalf("%d local repository roots were never listed, first page only", len(created))
	}
}

// TestRestoreExecutionFailureObservesCleanupTheEngineDidNotReport proves a
// failure the engine did not classify never answers "nothing was removed" from
// an assumption: the two exact targets and the published export are read from
// the filesystem, the same way the success path reads them.
func TestRestoreExecutionFailureObservesCleanupTheEngineDidNotReport(t *testing.T) {
	work, export := t.TempDir(), t.TempDir()
	options := restore.Options{WorkRoot: work, ExportRoot: export}
	gone := "0000000000000000000000000000aaaa"
	kept := "0000000000000000000000000000bbbb"
	published := "0000000000000000000000000000cccc"
	// kept: both private targets are still on disk and nothing was published.
	mustDir(t, filepath.Join(work, "restore-"+kept))
	mustWrite(t, filepath.Join(export, ".pending-"+kept))
	// published: the export is in place and the work directory is gone.
	mustWrite(t, filepath.Join(export, published))

	for _, test := range []struct {
		name      string
		restoreID string
		err       error
		code      string
		want      store.RestoreSideEffects
	}{
		{
			"reclaimed", gone, fmt.Errorf("disk went away"), "work_failed",
			store.RestoreSideEffects{WorkDirRemoved: true, PendingExportRemoved: true},
		},
		{"still there", kept, fmt.Errorf("disk went away"), "work_failed", store.RestoreSideEffects{}},
		{"published", published, fmt.Errorf("disk went away"), "work_failed", store.RestoreSideEffects{
			WorkDirRemoved: true, PendingExportRemoved: true, ExportPublished: true,
		}},
		// A context error is observed the same way; only its code says canceled.
		{"canceled", kept, context.Canceled, "canceled", store.RestoreSideEffects{}},
	} {
		failure := restoreExecutionFailure(test.err, options, test.restoreID)
		execErr := requireExecFailure(t, failure)
		if execErr.Code != test.code {
			t.Fatalf("%s: code = %s, want %s", test.name, execErr.Code, test.code)
		}
		if execErr.SideEffects != test.want {
			t.Fatalf("%s: side_effects = %+v, want the observed %+v", test.name, execErr.SideEffects, test.want)
		}
	}
}

// TestRestoreExecutionFailureKeepsEngineObservation proves the adapter never
// overwrites the cleanup observation the engine reported for its own typed
// failure: that observation is the exact one.
func TestRestoreExecutionFailureKeepsEngineObservation(t *testing.T) {
	work, export := t.TempDir(), t.TempDir()
	options := restore.Options{WorkRoot: work, ExportRoot: export}
	kept := "0000000000000000000000000000bbbb"
	mustDir(t, filepath.Join(work, "restore-"+kept))
	mustWrite(t, filepath.Join(export, ".pending-"+kept))
	engineErr := &restore.ExecError{
		Code: "prepare_failed", Phase: "preparing",
		SideEffects: restore.SideEffects{WorkDirRemoved: true},
	}
	execErr := requireExecFailure(t, restoreExecutionFailure(engineErr, options, kept))
	if want := (store.RestoreSideEffects{WorkDirRemoved: true}); execErr.SideEffects != want {
		t.Fatalf("side_effects = %+v, want the engine's own %+v", execErr.SideEffects, want)
	}
	if execErr.Code != "prepare_failed" || execErr.Phase != "preparing" {
		t.Fatalf("code/phase = %s/%s", execErr.Code, execErr.Phase)
	}
}

// TestRestoreExecutionFailureKeepsPlanRefusal proves a plan refusal stays a plan
// refusal: it wrote nothing, so it is not a cleanup claim at all.
func TestRestoreExecutionFailureKeepsPlanRefusal(t *testing.T) {
	options := restore.Options{WorkRoot: t.TempDir(), ExportRoot: t.TempDir()}
	failure := restoreExecutionFailure(&restore.PlanError{Code: "plan_changed", BackupID: "backup"}, options, "x")
	var refusal *RestorePlanError
	if !errors.As(failure, &refusal) {
		t.Fatalf("failure = %T, want *RestorePlanError", failure)
	}
	if refusal.Code != "plan_changed" {
		t.Fatalf("code = %s", refusal.Code)
	}
}

// requireExecFailure asserts one conversion produced the W5 execution error.
func requireExecFailure(t *testing.T, failure error) *RestoreExecError {
	t.Helper()
	var execErr *RestoreExecError
	if !errors.As(failure, &execErr) {
		t.Fatalf("failure = %T, want *RestoreExecError", failure)
	}
	return execErr
}

func mustDir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}
