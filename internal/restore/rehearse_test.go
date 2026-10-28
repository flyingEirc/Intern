package restore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateVerifySQL(t *testing.T) {
	accepted := []string{"SELECT 1", "select * from fixture.records;", "SELECT (1)"}
	if err := validateVerifySQL(accepted); err != nil {
		t.Fatalf("valid statements refused: %v", err)
	}
	rejected := [][]string{
		{""},
		{"UPDATE fixture.records SET value = 0"},
		{"SELECT 1; DROP TABLE fixture.records"},
		{"SELECT 1\x00"},
		{"  ;"},
		{strings.Repeat("SELECT 1", 1000)},
		make([]string, maxVerifySQLStatements+1),
	}
	for index, queries := range rejected {
		if err := validateVerifySQL(queries); err == nil {
			t.Fatalf("case %d accepted an invalid verify statement", index)
		}
	}
}

func TestQuoteIdentifier(t *testing.T) {
	if got := quoteIdentifier("weird`name"); got != "`weird``name`" {
		t.Fatalf("quoted identifier = %s", got)
	}
}

func TestStringify(t *testing.T) {
	if got := stringify(nil); got != "NULL" {
		t.Fatalf("nil = %q", got)
	}
	if got := stringify([]byte("bytes")); got != "bytes" {
		t.Fatalf("bytes = %q", got)
	}
	if got := stringify(int64(7)); got != "7" {
		t.Fatalf("int = %q", got)
	}
}

func TestCopyTreeModesAndContent(t *testing.T) {
	source := filepath.Join(t.TempDir(), "data")
	mustNoError(t, os.MkdirAll(filepath.Join(source, "nested"), 0o700))
	mustNoError(t, os.WriteFile(filepath.Join(source, "ibdata1"), []byte("body"), 0o600))
	mustNoError(t, os.WriteFile(filepath.Join(source, "nested", "undo"), []byte("undo"), 0o600))
	target := filepath.Join(t.TempDir(), "export")
	copied, err := copyTree(t.Context(), source, target)
	mustNoError(t, err)
	if copied != int64(len("body")+len("undo")) {
		t.Fatalf("copied bytes = %d", copied)
	}
	for _, path := range []string{target, filepath.Join(target, "nested")} {
		info, err := os.Stat(path)
		mustNoError(t, err)
		if info.Mode().Perm() != 0o750 {
			t.Fatalf("%s mode = %o, want 0750", path, info.Mode().Perm())
		}
	}
	for _, path := range []string{filepath.Join(target, "ibdata1"), filepath.Join(target, "nested", "undo")} {
		info, err := os.Stat(path)
		mustNoError(t, err)
		if info.Mode().Perm() != 0o640 {
			t.Fatalf("%s mode = %o, want 0640", path, info.Mode().Perm())
		}
	}
}

func TestCopyTreeRefusesSymlink(t *testing.T) {
	source := filepath.Join(t.TempDir(), "data")
	mustNoError(t, os.Mkdir(source, 0o700))
	mustNoError(t, os.Symlink("/etc/passwd", filepath.Join(source, "passwd")))
	_, err := copyTree(t.Context(), source, filepath.Join(t.TempDir(), "target"))
	if err == nil {
		t.Fatal("symlink in the prepared datadir was copied")
	}
}

func TestRenameNoReplace(t *testing.T) {
	base := t.TempDir()
	from := filepath.Join(base, "from")
	to := filepath.Join(base, "to")
	mustNoError(t, os.Mkdir(from, 0o700))
	mustNoError(t, renameNoReplace(from, to))
	if _, err := os.Stat(to); err != nil {
		t.Fatal("rename did not publish the directory")
	}
	another := filepath.Join(base, "another")
	mustNoError(t, os.Mkdir(another, 0o700))
	err := renameNoReplace(another, to)
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("non-replacing rename error = %v, want ErrExist", err)
	}
	if _, err = os.Stat(another); err != nil {
		t.Fatal("a refused rename removed the source")
	}
}
