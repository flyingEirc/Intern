//go:build integration

package acceptance

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The reader child is configured through these variables: the file it copies
// and the inherited descriptor it copies into.
const (
	readFileChildPath = "INTERN_W4_READ_FILE"
	readFileChildFD   = "INTERN_W4_READ_FILE_FD"
	// readFileChildTimeout bounds one reader, so a scan of a file another
	// process holds open cannot hang the scenario.
	readFileChildTimeout = 2 * time.Minute
)

// TestW4ReadFileChild is the re-execed helper that reads one file for the
// secret scan. The worker must never open, read or close the control-plane
// database, its WAL or its SHM itself.
//
// Under POSIX rules, closing any descriptor on a file releases every fcntl lock
// the closing process holds on it, and SQLite keeps its shared lock on the -shm
// companion through such a descriptor. A stray open/close in the worker
// therefore detaches every SQLite connection the worker holds: the serving
// child, now unopposed, treats itself as the last connection, checkpoints and
// unlinks the WAL, and the worker's connections keep reading the unlinked
// inode. Every write of every later child becomes invisible to the worker.
//
// Reading the bytes here, in a process that holds no SQLite lock, cannot
// disturb the worker's connections: fcntl locks are not inherited across fork,
// and this process holds none to release.
func TestW4ReadFileChild(t *testing.T) {
	path := os.Getenv(readFileChildPath)
	if path == "" {
		t.Fatalf("%s is required", readFileChildPath)
	}
	raw := os.Getenv(readFileChildFD)
	if raw == "" {
		t.Fatalf("%s is required", readFileChildFD)
	}
	var fd int
	if _, err := fmt.Sscanf(raw, "%d", &fd); err != nil {
		t.Fatalf("%s=%q is not a descriptor: %v", readFileChildFD, raw, err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	out := os.NewFile(uintptr(fd), "secret-scan")
	if out == nil {
		t.Fatal("the inherited scan descriptor is not open")
	}
	defer func() { _ = out.Close() }()
	if _, err = io.Copy(out, file); err != nil {
		t.Fatal(err)
	}
}

// scanFileInChild copies one file through the re-execed reader and reports
// whether any needle appears in it. The parent never opens the file: it reads
// the bytes out of a pipe the child writes, which is what keeps the worker's
// SQLite connections attached (TestW4ReadFileChild).
//
// A reader that cannot be started, fails, or ends without a clean exit fails
// the scenario: an unreadable scan is not a clean one.
func (f *fixture) scanFileInChild(t *testing.T, path string, needles [][]byte) bool {
	t.Helper()
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return false
	}
	executable, err := os.Executable()
	must(t, err)
	read, write, err := os.Pipe()
	must(t, err)
	defer func() { _ = read.Close() }()
	defer func() { _ = write.Close() }()
	var stderr bytes.Buffer
	command := exec.Command(executable, "-test.run=^TestW4ReadFileChild$",
		"-test.timeout="+readFileChildTimeout.String())
	command.Env = append(os.Environ(),
		readFileChildPath+"="+path,
		readFileChildFD+"=3",
	)
	// The reader's own test output must not reach the worker's log: the harness
	// counts the worker's case lines, and a second "--- PASS" from a helper
	// process would corrupt that count. Its stderr is kept only for the failure
	// message, which quotes the reader's own diagnosis.
	command.Stdout = io.Discard
	command.Stderr = &stderr
	command.ExtraFiles = []*os.File{write}
	must(t, command.Start())
	must(t, write.Close())
	found, scanErr := scanStream(read, needles)
	_ = read.Close()
	waitErr := command.Wait()
	must(t, scanErr)
	if waitErr != nil {
		t.Fatalf("the secret-scan reader of %s failed: %v: %s", filepath.Base(path), waitErr, strings.TrimSpace(stderr.String()))
	}
	return found
}

// scanStream reports whether any needle occurs in the bytes read from r. Each
// chunk is scanned with the tail of the previous one in front of it, so a
// needle that straddles a read boundary is still found and the memory held is
// bounded by the longest needle rather than by the file size.
func scanStream(r io.Reader, needles [][]byte) (bool, error) {
	overlap := 0
	for _, needle := range needles {
		overlap = max(overlap, len(needle)-1)
	}
	buffer := make([]byte, 64<<10)
	var carry []byte
	for {
		n, err := r.Read(buffer)
		if n > 0 {
			chunk := make([]byte, 0, len(carry)+n)
			chunk = append(chunk, carry...)
			chunk = append(chunk, buffer[:n]...)
			if containsAny(chunk, needles) {
				return true, nil
			}
			carry = append([]byte(nil), chunk[max(len(chunk)-overlap, 0):]...)
		}
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
}
