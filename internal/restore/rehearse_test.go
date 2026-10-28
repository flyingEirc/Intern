package restore

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestValidateVerifySQL(t *testing.T) {
	longest := "SELECT " + strings.Repeat("x", MaxVerifySQLBytes-len("SELECT "))
	accepted := [][]string{
		{"SELECT 1"},
		{"select * from fixture.records;"},
		{"SELECT (1)"},
		{longest},
		makeStatements(MaxVerifySQLStatements, "SELECT 1"),
	}
	for index, queries := range accepted {
		if err := ValidateVerifySQL(queries); err != nil {
			t.Fatalf("valid statements %d refused: %v", index, err)
		}
	}
	rejected := [][]string{
		{""},
		{"UPDATE fixture.records SET value = 0"},
		{"SELECT 1; DROP TABLE fixture.records"},
		{"SELECT 1\x00"},
		{"  ;"},
		{"selectx 1"},
		{"SELECT " + strings.Repeat("x", MaxVerifySQLBytes)},
		makeStatements(MaxVerifySQLStatements+1, "SELECT 1"),
	}
	for index, queries := range rejected {
		if err := ValidateVerifySQL(queries); err == nil {
			t.Fatalf("case %d accepted an invalid verify statement", index)
		}
	}
}

// TestVerifySQLLimitsAreTheContract pins the limits the API layer and the
// OpenAPI document publish: a change here must be mirrored in app/web, so a
// silent engine-side change fails here first.
func TestVerifySQLLimitsAreTheContract(t *testing.T) {
	if MaxVerifySQLStatements != 16 {
		t.Fatalf("MaxVerifySQLStatements = %d, want 16", MaxVerifySQLStatements)
	}
	if MaxVerifySQLBytes != 4096 {
		t.Fatalf("MaxVerifySQLBytes = %d, want 4096", MaxVerifySQLBytes)
	}
}

func makeStatements(count int, statement string) []string {
	queries := make([]string, count)
	for index := range queries {
		queries[index] = statement
	}
	return queries
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

// TestRehearsalFailureCode pins the classification of a rehearsal that never
// became ready: an early exit is not_ready even when tearing the dead process down
// reports an error; a server that never became ready but was killed by this process
// is not_ready too, because killRehearsal reduces its own SIGKILL exit to a clean
// teardown; only a teardown that really failed is a shutdown failure.
func TestRehearsalFailureCode(t *testing.T) {
	teardown := errors.New("physical: native tool failed to start, finish, or drain its pipes")
	cases := []struct {
		name     string
		exited   bool
		teardown error
		want     string
	}{
		{"early exit, teardown error", true, teardown, "not_ready"},
		{"early exit, clean teardown", true, nil, "not_ready"},
		{"never ready, killed by this process", false, nil, "not_ready"},
		{"never ready, teardown failed", false, teardown, "shutdown_failed"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := rehearsalFailureCode(test.exited, test.teardown); got != test.want {
				t.Fatalf("code = %q, want %q", got, test.want)
			}
		})
	}
}

// signaledExit returns the exit error of a real process this test ended with one
// signal, so the classification sees the same wait status production does.
func signaledExit(t *testing.T, signal syscall.Signal) error {
	t.Helper()
	command := exec.Command("/bin/sh", "-c", "exec sleep 30")
	mustNoError(t, command.Start())
	mustNoError(t, command.Process.Signal(signal))
	err := command.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("wait = %v, want an exit error", err)
	}
	return err
}

// TestOnlyKilledExit requires the teardown classification to accept exactly one
// cause: the SIGKILL this process itself sent. A different signal, an unrelated
// error, or a second error joined next to the exit status all stay failures, so a
// real teardown problem is never hidden behind the expected kill.
func TestOnlyKilledExit(t *testing.T) {
	killed := signaledExit(t, syscall.SIGKILL)
	terminated := signaledExit(t, syscall.SIGTERM)
	if !onlyKilledExit(killed) {
		t.Fatalf("own SIGKILL exit %v was not accepted", killed)
	}
	if !onlyKilledExit(errors.Join(killed, nil)) {
		t.Fatalf("own SIGKILL exit joined with nil was not accepted")
	}
	cases := []struct {
		name string
		err  error
	}{
		{"nil", nil},
		{"other signal", terminated},
		{"plain error", errors.New("physical: native tool failed to start, finish, or drain its pipes")},
		{"kill next to another failure", errors.Join(killed, errors.New("second failure"))},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if onlyKilledExit(test.err) {
				t.Fatalf("%v was accepted as a clean teardown", test.err)
			}
		})
	}
}
