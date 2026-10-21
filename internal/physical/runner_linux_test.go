package physical

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeTool(t *testing.T, body string) string {
	t.Helper()
	return fakeNamedTool(t, "tool", body)
}

func fakeNamedTool(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func shellQuote(path string) string {
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}

func backupFixtureBody() string {
	return `credential=${1#--defaults-file=}
[ "$credential" != "$1" ]
[ "$2" = "--no-login-paths" ]
case "$credential" in
/proc/self/fd/*) ;;
*) exit 72 ;;
esac
[ -z "${MYSQL_PWD+x}" ]
[ -z "${MYSQL_TEST_LOGIN_FILE+x}" ]
[ -z "${MYSQL_HOME+x}" ]
[ -f "$credential" ]
[ "$(stat -L -c %a "$credential")" = 600 ]
grep -q '^user="backup"$' "$credential"
grep -q '^password="private-test-value"$' "$credential"
lsn=
for arg in "$@"; do
 case "$arg" in
 --extra-lsndir=*) lsn=${arg#--extra-lsndir=} ;;
 --password=*|--no-server-version-check) exit 70 ;;
 esac
done
[ -n "$lsn" ]
[ "$(stat -c %a "$(dirname "$lsn")")" = 700 ]
[ ! -e "$(dirname "$lsn")/client.cnf" ]
cat > "$lsn/xtrabackup_checkpoints" <<'CHECKPOINTS'
` + testCheckpoints + `CHECKPOINTS
cat > "$lsn/xtrabackup_info" <<'INFO'
` + testInfo + `INFO
printf 'native-stream'
printf 'completed OK!\n' >&2
`
}

func grandchildScript(pidFile, follow string) string {
	return "sleep 30 &\nprintf '%s %s\\n' \"$$\" \"$!\" > " + shellQuote(pidFile) + "\n" + follow
}

func testSource(t *testing.T, binary string) *BackupSource {
	t.Helper()
	return &BackupSource{Binary: binary, WorkRoot: t.TempDir(), Connection: Connection{Socket: "/mysql/mysql.sock", DataDir: "/mysql/data", Username: "backup", Password: "private-test-value"}, Timeout: 5 * time.Second}
}

func assertWorkEmpty(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("work root not cleaned: count=%d error=%v", len(entries), err)
	}
}

func assertToolReaped(t *testing.T, source *BackupSource, ctx context.Context, writer io.Writer, want error, pidFile string, limit time.Duration) {
	t.Helper()
	started := time.Now()
	err := source.Run(ctx, writer)
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
	if time.Since(started) > limit {
		t.Fatalf("run took %s, limit %s", time.Since(started), limit)
	}
	assertPIDsGone(t, pidFile)
	assertWorkEmpty(t, source.WorkRoot)
}

func TestBackupSourceSuccessAndMetadataOwnership(t *testing.T) {
	t.Setenv("MYSQL_PWD", "private-test-value")
	t.Setenv("MYSQL_TEST_LOGIN_FILE", filepath.Join(t.TempDir(), "mylogin.cnf"))
	t.Setenv("MYSQL_HOME", t.TempDir())
	source := testSource(t, fakeNamedTool(t, "xtrabackup", backupFixtureBody()))
	var output bytes.Buffer
	if err := source.Run(context.Background(), &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "native-stream" {
		t.Fatal("stream changed")
	}
	metadata, err := source.Metadata()
	if err != nil || string(metadata.CheckpointsRaw) != testCheckpoints || string(metadata.InfoRaw) != testInfo {
		t.Fatalf("metadata mismatch: %v", err)
	}
	metadata.CheckpointsRaw[0] = 'X'
	metadata.Info.Fields["uuid"] = "changed"
	again, err := source.Metadata()
	if err != nil || string(again.CheckpointsRaw) != testCheckpoints || again.Info.Fields["uuid"] == "changed" {
		t.Fatal("metadata ownership escaped")
	}
	if err := source.Run(context.Background(), io.Discard); err == nil {
		t.Fatal("source reused")
	}
	assertWorkEmpty(t, source.WorkRoot)
}

func TestBackupRejectsExitMarkerAndMetadataFailures(t *testing.T) {
	for name, body := range map[string]string{
		"nonzero":          backupFixtureBody() + "exit 9\n",
		"half-stream":      "printf partial; exit 7\n",
		"missing-marker":   strings.Replace(backupFixtureBody(), "printf 'completed OK!\\n' >&2", "true", 1),
		"missing-metadata": "printf partial; printf 'completed OK!\\n' >&2\n",
		"bad-metadata":     strings.Replace(backupFixtureBody(), "to_lsn = 100", "to_lsn = -1", 1),
	} {
		t.Run(name, func(t *testing.T) {
			source := testSource(t, fakeNamedTool(t, "xtrabackup", body))
			err := source.Run(context.Background(), io.Discard)
			if err == nil || strings.Contains(err.Error(), "private-test-value") || strings.Contains(err.Error(), "hidden") {
				t.Fatalf("failure or diagnostic safety violated: %v", err)
			}
			if _, err := source.Metadata(); err == nil {
				t.Fatal("failed backup published metadata")
			}
			assertWorkEmpty(t, source.WorkRoot)
		})
	}
}

func TestStderrFloodCompletes(t *testing.T) {
	t.Setenv("MYSQL_PWD", "private-test-value")
	t.Setenv("MYSQL_TEST_LOGIN_FILE", filepath.Join(t.TempDir(), "mylogin.cnf"))
	t.Setenv("MYSQL_HOME", t.TempDir())
	body := `i=0
while [ "$i" -lt 4000 ]; do
 printf 'private-test-value password=hidden token=hidden\n' >&2
 i=$((i+1))
done
` + backupFixtureBody()
	source := testSource(t, fakeNamedTool(t, "xtrabackup", body))
	if err := source.Run(context.Background(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Metadata(); err != nil {
		t.Fatal(err)
	}
	assertWorkEmpty(t, source.WorkRoot)
}

type failingWriter struct {
	err error
}

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestBackupDownstreamErrors(t *testing.T) {
	failure := errors.New("downstream sentinel")
	limit := supervisorWaitDelay + 3*time.Second
	for name, want := range map[string]error{"short-write": io.ErrShortWrite, "write-error": failure} {
		t.Run(name, func(t *testing.T) {
			err := want
			if errors.Is(want, io.ErrShortWrite) {
				err = nil
			}
			pidFile := filepath.Join(t.TempDir(), "pids")
			source := testSource(t, fakeNamedTool(t, "xtrabackup", grandchildScript(pidFile, "while :; do printf stream-chunk; done\n")))
			assertToolReaped(t, source, context.Background(), failingWriter{err: err}, want, pidFile, limit)
		})
	}
}

func TestBackupCancellationAndWatchdog(t *testing.T) {
	for _, watchdog := range []bool{false, true} {
		name := "caller"
		if watchdog {
			name = "watchdog"
		}
		t.Run(name, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "pids")
			source := testSource(t, fakeNamedTool(t, "xtrabackup", grandchildScript(pidFile, "printf ready\nwait\n")))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := context.Canceled
			writer := io.Writer(cancelOnWrite{cancel: cancel})
			limit := supervisorWaitDelay + 3*time.Second
			if watchdog {
				source.Timeout = 4 * time.Second
				want = context.DeadlineExceeded
				writer = io.Discard
				limit = source.Timeout + supervisorWaitDelay + 3*time.Second
			}
			assertToolReaped(t, source, ctx, writer, want, pidFile, limit)
		})
	}
}

type cancelOnWrite struct{ cancel context.CancelFunc }

func (w cancelOnWrite) Write(p []byte) (int, error) {
	w.cancel()
	return len(p), nil
}

func TestProbeAndExtractEntrypoints(t *testing.T) {
	t.Setenv("MYSQL_PWD", "private-test-value")
	t.Setenv("MYSQL_TEST_LOGIN_FILE", filepath.Join(t.TempDir(), "mylogin.cnf"))
	t.Setenv("MYSQL_HOME", t.TempDir())
	binary := fakeNamedTool(t, "xtrabackup", `[ "$1" = --no-defaults ]
[ "$2" = --no-login-paths ]
[ "$3" = --version ]
[ -z "${MYSQL_PWD+x}" ]
[ -z "${MYSQL_TEST_LOGIN_FILE+x}" ]
[ -z "${MYSQL_HOME+x}" ]
printf '%s version 8.4.0-7 based on MySQL server 8.4.0\n' "$0" >&2
`)
	version, err := ProbeVersion(context.Background(), binary)
	if err != nil || version.Version != "8.4.0-7" {
		t.Fatalf("probe: %#v, %v", version, err)
	}
	root := t.TempDir()
	binary = fakeTool(t, "[ \"$1\" = --extract ]; cat > extracted\n")
	dir, err := Extract(context.Background(), ExtractOptions{Binary: binary, WorkRoot: root}, strings.NewReader("archive"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "extracted"))
	if err != nil || string(raw) != "archive" {
		t.Fatal("extract input was not forwarded")
	}
	os.RemoveAll(dir)
	binary = fakeTool(t, "cat > partial; exit 1\n")
	if _, err := Extract(context.Background(), ExtractOptions{Binary: binary, WorkRoot: root}, strings.NewReader("archive")); err == nil {
		t.Fatal("failed extraction accepted")
	}
	assertWorkEmpty(t, root)
}

func TestProbeRejectsUnanchoredBannerAndBasename(t *testing.T) {
	wrongName := fakeNamedTool(t, "xtrabackup-8.4", "printf streamed\n")
	var output bytes.Buffer
	source := testSource(t, wrongName)
	if err := source.Run(context.Background(), &output); err == nil || output.Len() != 0 {
		t.Fatal("non-xtrabackup binary streamed a backup")
	}
	if _, err := ProbeVersion(context.Background(), wrongName); err == nil {
		t.Fatal("versioned basename was probed")
	}
	binary := fakeNamedTool(t, "xtrabackup", "printf 'xtrabackup version 8.4.0-7\\n' >&2\n")
	if _, err := ProbeVersion(context.Background(), binary); err == nil {
		t.Fatal("banner was not anchored to the configured binary")
	}
}

func TestNonUTF8ConfigurationDoesNotStart(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	source := testSource(t, fakeNamedTool(t, "xtrabackup", "printf x > "+shellQuote(marker)+"\n"))
	source.Connection.DataDir = "/mysql/\xff"
	var output bytes.Buffer
	err := source.Run(context.Background(), &output)
	if !errors.Is(err, errSupervisorEncoding) || output.Len() != 0 {
		t.Fatalf("non-UTF-8 configuration: %v bytes=%d", err, output.Len())
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("tool started")
	}
	assertWorkEmpty(t, source.WorkRoot)
}

func TestIncrementalUsesAndValidatesParentLSN(t *testing.T) {
	body := strings.NewReplacer(
		"full-backuped", "incremental",
		"from_lsn = 0", "from_lsn = 100",
		"to_lsn = 100", "to_lsn = 200",
		"last_lsn = 110", "last_lsn = 210",
		"incremental = N", "incremental = Y",
	).Replace(backupFixtureBody())
	body = "found=0; for arg in \"$@\"; do [ \"$arg\" != --incremental-lsn=100 ] || found=1; done; [ \"$found\" = 1 ]\n" + body
	source := testSource(t, fakeNamedTool(t, "xtrabackup", body))
	source.ParentCheckpoints = []byte(testCheckpoints)
	if err := source.Run(context.Background(), io.Discard); err != nil {
		t.Fatal(err)
	}
	metadata, err := source.Metadata()
	if err != nil || metadata.Checkpoints.FromLSN != 100 || metadata.Checkpoints.ToLSN != 200 || !metadata.Info.Incremental {
		t.Fatalf("incremental metadata mismatch: %v", err)
	}
	assertWorkEmpty(t, source.WorkRoot)
	source = testSource(t, fakeNamedTool(t, "xtrabackup", backupFixtureBody()))
	source.ParentCheckpoints = []byte(testCheckpoints)
	if err := source.Run(context.Background(), io.Discard); err == nil {
		t.Fatal("full metadata accepted for incremental request")
	}
	assertWorkEmpty(t, source.WorkRoot)
}

func TestDownstreamSecretErrorIsOpaqueButPreserved(t *testing.T) {
	sentinel := errors.New("password=private-test-value")
	pidFile := filepath.Join(t.TempDir(), "pids")
	source := testSource(t, fakeNamedTool(t, "xtrabackup", grandchildScript(pidFile, "while :; do printf chunk; done\n")))
	err := source.Run(context.Background(), failingWriter{err: sentinel})
	if !errors.Is(err, sentinel) || strings.Contains(err.Error(), "private-test-value") {
		t.Fatalf("downstream cause not safely preserved: %v", err)
	}
	assertPIDsGone(t, pidFile)
	assertWorkEmpty(t, source.WorkRoot)
}

func TestExtractCancellationClosesBlockedInput(t *testing.T) {
	input, output := io.Pipe()
	defer output.Close()
	root := t.TempDir()
	_, err := Extract(context.Background(), ExtractOptions{
		Binary: fakeTool(t, "cat > partial\n"), WorkRoot: root, Timeout: 100 * time.Millisecond,
	}, input)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked input cancellation = %v", err)
	}
	if _, err := output.Write([]byte("closed")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("input pipe was not closed: %v", err)
	}
	assertWorkEmpty(t, root)
}

func TestControlPathsNeverStartTool(t *testing.T) {
	for _, test := range []struct {
		name string
		set  func(*BackupSource, string)
	}{
		{"socket", func(s *BackupSource, path string) { s.Connection.Socket = path }},
		{"datadir", func(s *BackupSource, path string) { s.Connection.DataDir = path }},
		{"work", func(s *BackupSource, path string) { s.WorkRoot = path }},
	} {
		t.Run(test.name, func(t *testing.T) {
			badPath := filepath.Join(t.TempDir(), "unsupported\npath")
			if err := os.Mkdir(badPath, 0o700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(t.TempDir(), "started")
			source := testSource(t, fakeNamedTool(t, "xtrabackup", "printf started > "+shellQuote(marker)+"\n"))
			test.set(source, badPath)
			if err := source.Run(t.Context(), io.Discard); err == nil {
				t.Fatal("control separator path admitted")
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("tool started before rejecting unsupported path")
			}
		})
	}
}
