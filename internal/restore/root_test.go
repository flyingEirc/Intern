package restore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckExportRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "export")
	mustNoError(t, os.Mkdir(root, 0o700))
	metadata := filepath.Join(base, "metadata")
	mustNoError(t, os.Mkdir(metadata, 0o700))
	work := filepath.Join(base, "work")
	mustNoError(t, os.Mkdir(work, 0o700))
	if err := CheckExportRoot(root, work, metadata); err != nil {
		t.Fatalf("valid export root refused: %v", err)
	}
	file := filepath.Join(base, "file")
	mustNoError(t, os.WriteFile(file, []byte("x"), 0o600))
	link := filepath.Join(base, "link")
	mustNoError(t, os.Symlink(root, link))
	insideWork := filepath.Join(work, "export")
	mustNoError(t, os.Mkdir(insideWork, 0o700))
	insideMetadata := filepath.Join(metadata, "export")
	mustNoError(t, os.Mkdir(insideMetadata, 0o700))
	cases := []struct {
		name      string
		root      string
		forbidden []string
	}{
		{name: "relative", root: "export"},
		{name: "missing", root: filepath.Join(base, "missing")},
		{name: "regular file", root: file},
		{name: "symlink", root: link},
		{name: "inside work", root: insideWork, forbidden: []string{work}},
		{name: "equal to work", root: work, forbidden: []string{work}},
		{name: "inside metadata", root: insideMetadata, forbidden: []string{metadata}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if err := CheckExportRoot(test.root, test.forbidden...); err == nil {
				t.Fatal("invalid export root accepted")
			}
		})
	}
	// A protected directory below the export root is not a containment of the
	// export root, so it stays allowed.
	if err := CheckExportRoot(base, filepath.Join(base, "below")); err != nil {
		t.Fatalf("a nested protected root refused a valid export root: %v", err)
	}
}

func TestValidateKeyOutsideExport(t *testing.T) {
	base := t.TempDir()
	export := filepath.Join(base, "export")
	mustNoError(t, os.Mkdir(export, 0o700))
	inside := filepath.Join(export, "kek.json")
	mustNoError(t, os.WriteFile(inside, []byte("x"), 0o600))
	outside := filepath.Join(base, "kek.json")
	mustNoError(t, os.WriteFile(outside, []byte("x"), 0o600))
	if err := validateKeyOutsideExport(outside, export); err != nil {
		t.Fatalf("valid KEK location refused: %v", err)
	}
	if err := validateKeyOutsideExport(inside, export); err == nil {
		t.Fatal("KEK inside the export root accepted")
	}
	if err := validateKeyOutsideExport("", export); err != nil {
		t.Fatal(err)
	}
}

func TestValidatePrivateWorkRoot(t *testing.T) {
	base := t.TempDir()
	private := filepath.Join(base, "work")
	mustNoError(t, os.Mkdir(private, 0o700))
	if err := validatePrivateWorkRoot(private); err != nil {
		t.Fatalf("valid work root refused: %v", err)
	}
	public := filepath.Join(base, "public")
	mustNoError(t, os.Mkdir(public, 0o755))
	mustNoError(t, os.Chmod(public, 0o755))
	if err := validatePrivateWorkRoot(public); err == nil {
		t.Fatal("world-readable work root accepted")
	}
	link := filepath.Join(base, "link")
	mustNoError(t, os.Symlink(private, link))
	if err := validatePrivateWorkRoot(link); err == nil {
		t.Fatal("symlinked work root accepted")
	}
	if err := validatePrivateWorkRoot("relative"); err == nil {
		t.Fatal("relative work root accepted")
	}
}

func TestValidateExecutable(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "tool")
	mustNoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"), 0o500))
	if err := validateExecutable(path, "tool"); err != nil {
		t.Fatalf("valid executable refused: %v", err)
	}
	if err := validateExecutable("tool", "tool"); err == nil {
		t.Fatal("relative tool accepted")
	}
	if err := validateExecutable(filepath.Join(base, "missing"), "tool"); err == nil {
		t.Fatal("missing tool accepted")
	}
	plain := filepath.Join(base, "plain")
	mustNoError(t, os.WriteFile(plain, []byte("x"), 0o600))
	if err := validateExecutable(plain, "tool"); err == nil || !strings.Contains(err.Error(), "executable") {
		t.Fatal("non-executable file accepted")
	}
}
