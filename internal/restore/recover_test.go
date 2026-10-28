package restore

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func recoveryRoots(t *testing.T) (string, string) {
	t.Helper()
	work := filepath.Join(t.TempDir(), "work")
	export := filepath.Join(t.TempDir(), "export")
	mustNoError(t, os.Mkdir(work, 0o700))
	mustNoError(t, os.Mkdir(export, 0o700))
	return work, export
}

func seedRestore(t *testing.T, work, export, id string) (workDir, pending, published string) {
	t.Helper()
	workDir = filepath.Join(work, "restore-"+id)
	pending = filepath.Join(export, ".pending-"+id)
	published = filepath.Join(export, id)
	for _, dir := range []string{workDir, pending, filepath.Join(published, "data")} {
		mustNoError(t, os.MkdirAll(dir, 0o700))
	}
	for _, dir := range []string{workDir, pending, filepath.Join(published, "data")} {
		mustNoError(t, os.WriteFile(filepath.Join(dir, "sentinel"), []byte(id), 0o600))
	}
	return workDir, pending, published
}

func TestRecoverExactTargets(t *testing.T) {
	work, export := recoveryRoots(t)
	first := "0123456789abcdef0123456789abcdef"
	second := "fedcba9876543210fedcba9876543210"
	firstWork, firstPending, firstPublished := seedRestore(t, work, export, first)
	secondWork, secondPending, secondPublished := seedRestore(t, work, export, second)
	mustNoError(t, Recover(t.Context(), work, export, []string{first}))
	for _, target := range []string{firstWork, firstPending} {
		if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s survived recovery", target)
		}
	}
	for _, target := range []string{secondWork, secondPending, secondPublished, firstPublished} {
		if _, err := os.Lstat(target); err != nil {
			t.Fatalf("%s was removed by another restore's recovery", target)
		}
	}
	mustNoError(t, Recover(t.Context(), work, export, []string{first}))
	if _, err := os.Lstat(secondWork); err != nil {
		t.Fatal("a second recovery changed another restore")
	}
	if _, err := os.Lstat(firstPublished); err != nil {
		t.Fatal("recovery removed a published export")
	}
}

func TestRecoverRefusesInvalidIDs(t *testing.T) {
	work, export := recoveryRoots(t)
	valid := "0123456789abcdef0123456789abcdef"
	workDir, pending, _ := seedRestore(t, work, export, valid)
	err := Recover(t.Context(), work, export, []string{"../escape", valid})
	if err == nil {
		t.Fatal("invalid restore id accepted")
	}
	for _, target := range []string{workDir, pending} {
		if _, err := os.Lstat(target); err != nil {
			t.Fatal("valid target was removed after an invalid id in the same call")
		}
	}
}

func TestRecoverRefusesSymlinkTarget(t *testing.T) {
	work, export := recoveryRoots(t)
	id := "0123456789abcdef0123456789abcdef"
	other := filepath.Join(t.TempDir(), "elsewhere")
	mustNoError(t, os.Mkdir(other, 0o700))
	mustNoError(t, os.WriteFile(filepath.Join(other, "sentinel"), []byte("x"), 0o600))
	mustNoError(t, os.Symlink(other, filepath.Join(work, "restore-"+id)))
	if err := Recover(t.Context(), work, export, []string{id}); err == nil {
		t.Fatal("symlinked work target accepted")
	}
	if _, err := os.Lstat(filepath.Join(other, "sentinel")); err != nil {
		t.Fatal("recovery followed a symlink into an unrelated directory")
	}
}

func TestRestoreTargets(t *testing.T) {
	work, export := recoveryRoots(t)
	id := "0123456789abcdef0123456789abcdef"
	workDir, pending, ok := RestoreTargets(work, export, id)
	if !ok || workDir != filepath.Join(work, "restore-"+id) || pending != filepath.Join(export, ".pending-"+id) {
		t.Fatal("restore targets differ from the recovery contract")
	}
	if _, _, ok = RestoreTargets(work, export, "not-an-id"); ok {
		t.Fatal("invalid restore id produced targets")
	}
}
