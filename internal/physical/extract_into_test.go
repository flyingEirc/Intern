package physical

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func privateDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "extract")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func requireExtractRefused(t *testing.T, directory string) {
	t.Helper()
	err := ExtractInto(t.Context(), ExtractIntoOptions{Binary: "/bin/xbstream", Directory: directory}, strings.NewReader(""))
	if err == nil {
		t.Fatalf("directory %q was accepted", directory)
	}
}

func TestExtractIntoRejectsRelativeDirectory(t *testing.T) {
	requireExtractRefused(t, "extract")
}

func TestExtractIntoRejectsMissingDirectory(t *testing.T) {
	requireExtractRefused(t, filepath.Join(t.TempDir(), "missing"))
}

func TestExtractIntoRejectsPublicDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "extract")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	requireExtractRefused(t, dir)
}

func TestExtractIntoRejectsNonEmptyDirectory(t *testing.T) {
	dir := privateDir(t)
	if err := os.WriteFile(filepath.Join(dir, "stale"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	requireExtractRefused(t, dir)
}

func TestExtractIntoRejectsSymlinkedDirectory(t *testing.T) {
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(privateDir(t), link); err != nil {
		t.Fatal(err)
	}
	requireExtractRefused(t, link)
}

func TestExtractIntoRequiresInputAndLimits(t *testing.T) {
	dir := privateDir(t)
	if err := ExtractInto(t.Context(), ExtractIntoOptions{Binary: "", Directory: dir}, strings.NewReader("")); err == nil {
		t.Fatal("empty binary accepted")
	}
	if err := ExtractInto(t.Context(), ExtractIntoOptions{Binary: "/bin/xbstream", Directory: dir, Parallel: -1}, strings.NewReader("")); err == nil {
		t.Fatal("negative parallel accepted")
	}
	if err := ExtractInto(t.Context(), ExtractIntoOptions{Binary: "/bin/xbstream", Directory: dir, Timeout: -1}, strings.NewReader("")); err == nil {
		t.Fatal("negative timeout accepted")
	}
}

// ExtractInto never removes the caller's directory, even when the tool fails.
func TestExtractIntoKeepsCallerDirectory(t *testing.T) {
	dir := privateDir(t)
	if err := ExtractInto(t.Context(), ExtractIntoOptions{Binary: "/bin/xbstream", Directory: dir}, bytes.NewReader(nil)); err == nil {
		t.Fatal("a missing extraction tool reported success")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("extract failure removed the caller's directory")
	}
}
