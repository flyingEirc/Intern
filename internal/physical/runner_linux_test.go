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
	path := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func backupFixtureBody() string {
	return `credential=${1#--defaults-file=}
[ "$credential" != "$1" ]
[ "$(stat -c %a "$credential")" = 600 ]
[ "$(stat -c %a "$(dirname "$credential")")" = 700 ]
lsn=
for arg in "$@"; do
 case "$arg" in
 --extra-lsndir=*) lsn=${arg#--extra-lsndir=} ;;
 --password=*|--no-server-version-check) exit 70 ;;
 esac
done
cat > "$lsn/xtrabackup_checkpoints" <<'CHECKPOINTS'
` + testCheckpoints + `CHECKPOINTS
cat > "$lsn/xtrabackup_info" <<'INFO'
` + testInfo + `INFO
printf 'native-stream'
printf 'completed OK!\n' >&2
`
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

func TestBackupSourceSuccessAndMetadataOwnership(t *testing.T) {
	source := testSource(t, fakeTool(t, backupFixtureBody()))
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
		"stderr-flood":     "i=0; while [ $i -lt 5000 ]; do printf 'private-test-value password=hidden token=hidden\\n' >&2; i=$((i+1)); done; exit 1\n",
	} {
		t.Run(name, func(t *testing.T) {
			source := testSource(t, fakeTool(t, body))
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

type failingWriter struct {
	err error
}

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestBackupDownstreamErrors(t *testing.T) {
	failure := errors.New("downstream sentinel")
	for name, want := range map[string]error{"short-write": io.ErrShortWrite, "write-error": failure} {
		t.Run(name, func(t *testing.T) {
			err := want
			if errors.Is(want, io.ErrShortWrite) {
				err = nil
			}
			source := testSource(t, fakeTool(t, "while :; do printf 'stream-chunk'; done\n"))
			if got := source.Run(context.Background(), failingWriter{err: err}); !errors.Is(got, want) {
				t.Fatalf("got %v, want %v", got, want)
			}
			assertWorkEmpty(t, source.WorkRoot)
		})
	}
}

func TestBackupCancellationAndWatchdog(t *testing.T) {
	for _, watchdog := range []bool{false, true} {
		t.Run(map[bool]string{false: "caller", true: "watchdog"}[watchdog], func(t *testing.T) {
			source := testSource(t, fakeTool(t, "printf 'ready'; sleep 30\n"))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := context.Canceled
			writer := io.Writer(cancelOnWrite{cancel: cancel})
			if watchdog {
				source.Timeout = 100 * time.Millisecond
				want = context.DeadlineExceeded
				writer = io.Discard
			}
			if err := source.Run(ctx, writer); !errors.Is(err, want) {
				t.Fatalf("got %v, want %v", err, want)
			}
			assertWorkEmpty(t, source.WorkRoot)
		})
	}
}

type cancelOnWrite struct{ cancel context.CancelFunc }

func (w cancelOnWrite) Write(p []byte) (int, error) {
	w.cancel()
	return len(p), nil
}

func TestProbeAndExtractEntrypoints(t *testing.T) {
	binary := fakeTool(t, "[ \"$1\" = --version ]; printf 'xtrabackup version 8.4.0-7 based on MySQL server 8.4.0\\n' >&2\n")
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

func TestIncrementalUsesAndValidatesParentLSN(t *testing.T) {
	body := strings.NewReplacer(
		"full-backuped", "incremental",
		"from_lsn = 0", "from_lsn = 100",
		"to_lsn = 100", "to_lsn = 200",
		"last_lsn = 110", "last_lsn = 210",
		"incremental = N", "incremental = Y",
	).Replace(backupFixtureBody())
	body = "found=0; for arg in \"$@\"; do [ \"$arg\" != --incremental-lsn=100 ] || found=1; done; [ \"$found\" = 1 ]\n" + body
	source := testSource(t, fakeTool(t, body))
	source.ParentCheckpoints = []byte(testCheckpoints)
	if err := source.Run(context.Background(), io.Discard); err != nil {
		t.Fatal(err)
	}
	metadata, err := source.Metadata()
	if err != nil || metadata.Checkpoints.FromLSN != 100 || metadata.Checkpoints.ToLSN != 200 || !metadata.Info.Incremental {
		t.Fatalf("incremental metadata mismatch: %v", err)
	}
	assertWorkEmpty(t, source.WorkRoot)
	source = testSource(t, fakeTool(t, backupFixtureBody()))
	source.ParentCheckpoints = []byte(testCheckpoints)
	if err := source.Run(context.Background(), io.Discard); err == nil {
		t.Fatal("full metadata accepted for incremental request")
	}
	assertWorkEmpty(t, source.WorkRoot)
}

func TestDownstreamSecretErrorIsOpaqueButPreserved(t *testing.T) {
	sentinel := errors.New("password=private-test-value")
	source := testSource(t, fakeTool(t, "while :; do printf chunk; done\n"))
	err := source.Run(context.Background(), failingWriter{err: sentinel})
	if !errors.Is(err, sentinel) || strings.Contains(err.Error(), "private-test-value") {
		t.Fatalf("downstream cause not safely preserved: %v", err)
	}
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
