package restore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// publishFixture prepares one execution whose prepared work copy holds a small
// datadir, ready to be published below a fresh export root.
func publishFixture(t *testing.T) *execution {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, "work")
	exportRoot := filepath.Join(root, "export")
	mustNoError(t, os.Mkdir(work, 0o700))
	mustNoError(t, os.Mkdir(exportRoot, 0o750))
	data := filepath.Join(work, "data")
	mustNoError(t, os.MkdirAll(filepath.Join(data, "nested"), 0o700))
	mustNoError(t, os.WriteFile(filepath.Join(data, "ibdata1"), []byte("body"), 0o600))
	mustNoError(t, os.WriteFile(filepath.Join(data, "nested", "undo"), []byte("undo"), 0o600))
	id := strings.Repeat("b", 32)
	return &execution{
		opts: Options{ExportRoot: exportRoot}, work: work,
		pending: filepath.Join(exportRoot, ".pending-"+id),
		export:  filepath.Join(exportRoot, id),
	}
}

func requireMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	mustNoError(t, err)
	if info.Mode().Perm() != want {
		t.Fatalf("%s mode = %o, want %o", path, info.Mode().Perm(), want)
	}
}

// TestPublishSetsTheContractModes requires the published export to carry the
// documented modes on the top directory as well as on the data directory and the
// files: an operator mounts <export>/<id>/data into a server that must traverse
// the top directory, so 0750 there is part of the contract rather than an
// implementation detail.
func TestPublishSetsTheContractModes(t *testing.T) {
	e := publishFixture(t)
	mustNoError(t, e.publish(t.Context()))
	requireMode(t, e.export, exportDirMode)
	requireMode(t, filepath.Join(e.export, "data"), exportDirMode)
	requireMode(t, filepath.Join(e.export, "data", "nested"), exportDirMode)
	requireMode(t, filepath.Join(e.export, "data", "ibdata1"), exportFileMode)
	requireMode(t, filepath.Join(e.export, "data", "nested", "undo"), exportFileMode)
	if _, err := os.Lstat(e.pending); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the pending export survived publication")
	}
	if !e.side.ExportPublished || e.exportBytes <= 0 {
		t.Fatalf("publication reported published=%v bytes=%d", e.side.ExportPublished, e.exportBytes)
	}
}

// TestPublishSetsTheModeBeforeTheRename requires the pending directory to carry
// the published mode already at the publication point. A pre-existing export path
// makes the non-replacing rename fail, which leaves the pending directory exactly
// as it was when the rename was attempted; the private 0700 writing mode must not
// survive to that moment.
func TestPublishSetsTheModeBeforeTheRename(t *testing.T) {
	e := publishFixture(t)
	mustNoError(t, os.Mkdir(e.export, exportDirMode))
	err := e.publish(t.Context())
	var staged *stageError
	if !errors.As(err, &staged) || staged.code != CodeExportFailed {
		t.Fatalf("publish over an existing export = %v, want export_failed", err)
	}
	requireMode(t, e.pending, exportDirMode)
	if e.side.ExportPublished {
		t.Fatal("a failed publication reported a published export")
	}
}
