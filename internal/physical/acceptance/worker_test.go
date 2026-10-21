//go:build integration

package acceptance

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"

	"github.com/flyingEirc/Intern/internal/physical"
)

// This executable is run inside the pinned PXB container by the Testcontainers
// harness. Running it without the real fixture is an error, never a skipped gate.
func TestP2RealWorker(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Fatal("native Linux amd64 worker required")
	}
	password := os.Getenv("INTERN_P2_PASSWORD")
	if password == "" {
		t.Fatal("real physical fixture credentials missing")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	cfg := driver.NewConfig()
	cfg.Net, cfg.Addr, cfg.User, cfg.Passwd = "unix", "/var/run/mysqld/mysqld.sock", "intern_bkp", password
	cfg.Timeout, cfg.ReadTimeout, cfg.WriteTimeout = 5*time.Second, 30*time.Second, 30*time.Second
	connector, err := driver.NewConnector(cfg)
	must(t, err)
	db := sql.OpenDB(connector)
	t.Cleanup(func() { must(t, db.Close()) })
	must(t, db.PingContext(ctx))
	work, backups := t.TempDir(), t.TempDir()
	connection := physical.Connection{Socket: cfg.Addr, DataDir: "/var/lib/mysql", Username: cfg.User, Password: password}
	opts := physical.PreflightOptions{Connection: connection, Binary: "/usr/bin/xtrabackup", WorkRoot: work, BackupRoot: backups, MinWorkBytes: 1, MinBackupBytes: 1}
	result, err := physical.Preflight(ctx, opts)
	must(t, err)
	candidate := linuxAMD64Candidate(t)
	requireLiveSource(t, result, candidate)

	t.Run("preflight-refusals", func(t *testing.T) {
		low := opts
		low.MinWorkBytes = math.MaxUint64
		rejectCheck(t, "space_insufficient", func() error { _, e := physical.Preflight(ctx, low); return e })
		low = opts
		low.MinBackupBytes = math.MaxUint64
		rejectCheck(t, "space_insufficient", func() error { _, e := physical.Preflight(ctx, low); return e })
		denied := os.Getenv("INTERN_P2_DENIED_PASSWORD")
		if denied == "" || denied == password {
			t.Fatal("denied account needs its own credential")
		}
		bad := opts
		bad.Connection.Username = "intern_denied"
		bad.Connection.Password = denied
		rejectCheck(t, "privileges_missing", func() error { _, e := physical.Preflight(ctx, bad); return e })
		bad = opts
		bad.PageTracking = true
		rejectCheck(t, "page_tracking_unavailable", func() error { _, e := physical.Preflight(ctx, bad); return e })
		identity := result.Identity
		identity.Version = "9.7.1"
		rejectCheck(t, "version_unsupported", func() error { return physical.CheckCompatibility(identity, result.Tool, "linux/amd64") })
		rejectCheck(t, "prepare_version_mismatch", func() error {
			return physical.CheckPrepareVersion(candidate.ToolVersion, "8.0.35-36")
		})
		t.Log("P2-A2 refused space, privileges, page tracking, version_unsupported, prepare_version_mismatch")
	})

	assertPoisonedLoginFile(t)
	assertPoisonOverridesDefaults(t, password)
	runBackupChain(t, ctx, db, opts, result.Identity.UUID)
	runRealFailures(t, ctx, opts)
}

func requireLiveSource(t *testing.T, result physical.PreflightResult, candidate physical.CompatibilityEntry) {
	t.Helper()
	if result.Identity.Version != candidate.ServerVersion || result.Tool.Version != candidate.ToolVersion {
		t.Fatalf("candidate version mismatch: mysql=%s pxb=%s matrix=%s/%s", result.Identity.Version, result.Tool.Version, candidate.ServerVersion, candidate.ToolVersion)
	}
	if result.Identity.UUID == "" || result.Identity.PageSize != 16384 {
		t.Fatal("unexpected real source identity")
	}
	if err := os.WriteFile("/var/lib/mysql/intern-must-not-write", []byte("deny"), 0o600); !errors.Is(err, syscall.EROFS) {
		t.Fatalf("datadir write error = %v, want EROFS", err)
	}
	t.Logf("P2-A1 real versions mysql=%s pxb=%s platform=%s/%s uid=%d gid=%d datadir=EROFS socket=shared", result.Identity.Version, result.Tool.Version, runtime.GOOS, runtime.GOARCH, os.Getuid(), os.Getgid())
}

func TestP2EncryptedWorker(t *testing.T) {
	password := os.Getenv("INTERN_P2_PASSWORD")
	if password == "" {
		t.Fatal("real physical fixture credentials missing")
	}
	cfg := driver.NewConfig()
	cfg.Net, cfg.Addr, cfg.User, cfg.Passwd = "unix", "/var/run/mysqld/mysqld.sock", "intern_bkp", password
	connector, err := driver.NewConnector(cfg)
	must(t, err)
	db := sql.OpenDB(connector)
	defer func() { must(t, db.Close()) }()
	var encrypted int
	must(t, db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM information_schema.innodb_tablespaces WHERE encryption='Y'").Scan(&encrypted))
	if encrypted == 0 {
		t.Fatal("encrypted rejection fixture was not reached")
	}
	opts := physical.PreflightOptions{
		Connection: physical.Connection{Socket: cfg.Addr, DataDir: "/var/lib/mysql", Username: cfg.User, Password: password},
		Binary:     "/usr/bin/xtrabackup", WorkRoot: t.TempDir(), BackupRoot: t.TempDir(),
		MinWorkBytes: 1, MinBackupBytes: 1,
	}
	rejectCheck(t, "keyring_required", func() error { _, e := physical.Preflight(t.Context(), opts); return e })
	t.Log("P2-A2 encrypted tablespace rejected before backup")
}

func TestP2WrongVolumeWorker(t *testing.T) {
	opts := volumeOptions(t)
	rejectCheck(t, "uuid_mismatch", func() error { _, e := physical.Preflight(t.Context(), opts); return e })
	t.Log("P2-A2 mismatched auto.cnf rejected at same datadir mount")
}

func TestP2UnreadableVolumeWorker(t *testing.T) {
	opts := volumeOptions(t)
	_, err := physical.Preflight(t.Context(), opts)
	var failure *physical.PreflightError
	if !errors.As(err, &failure) || (failure.Code != "datadir_unreadable" && failure.Code != "auto_cnf_unreadable") {
		t.Fatalf("expected datadir readability refusal; observed %v", err)
	}
	t.Log("P2-A2 unreadable datadir rejected as nonroot")
}

func TestP2WritableVolumeWorker(t *testing.T) {
	opts := volumeOptions(t)
	rejectCheck(t, "datadir_mount_writable", func() error {
		_, err := physical.Preflight(t.Context(), opts)
		return err
	})
	if _, err := os.Stat("/var/lib/mysql/intern-must-not-write"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("writable rejection created a datadir file")
	}
	t.Log("P2-A2 writable datadir mount rejected before backup")
}

// TestP2PIDOneWorker is the container entrypoint. The harness does not exec it
// under an init process, so this process is PID 1 for the memfd backup.
func TestP2PIDOneWorker(t *testing.T) {
	if os.Getpid() != 1 {
		t.Fatalf("pid = %d, want container PID 1", os.Getpid())
	}
	assertPoisonedLoginFile(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	opts := volumeOptions(t)
	result, err := physical.Preflight(ctx, opts)
	must(t, err)
	metadata := runDirectBackup(t, ctx, opts)
	assertMemfdCommand(t, metadata)
	assertRealMetadataContract(t, metadata, result.Identity.UUID)
	if metadata.Checkpoints.BackupType != "full-backuped" || metadata.Checkpoints.FromLSN != 0 || metadata.Checkpoints.ToLSN == 0 {
		t.Fatal("PID1 result is not an unprepared full backup")
	}
	assertNoXtrabackupProcess(t)
	t.Log("P2-A1 PID1 memfd backup login_path=ignored")
}

func volumeOptions(t *testing.T) physical.PreflightOptions {
	t.Helper()
	password := os.Getenv("INTERN_P2_PASSWORD")
	if password == "" {
		t.Fatal("real physical fixture credentials missing")
	}
	return physical.PreflightOptions{
		Connection: physical.Connection{Socket: "/var/run/mysqld/mysqld.sock", DataDir: "/var/lib/mysql", Username: "intern_bkp", Password: password},
		Binary:     "/usr/bin/xtrabackup", WorkRoot: t.TempDir(), BackupRoot: t.TempDir(), MinWorkBytes: 1, MinBackupBytes: 1,
	}
}

var errInjectedDownstream = errors.New("injected downstream failure")

type rejectWriter struct{ reached bool }

func (w *rejectWriter) Write([]byte) (int, error) {
	w.reached = true
	return 0, errInjectedDownstream
}

type cancelWriter struct {
	cancel  context.CancelFunc
	reached bool
}

func (w *cancelWriter) Write(p []byte) (int, error) {
	w.reached = true
	w.cancel()
	return len(p), nil
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		message := err.Error()
		for _, key := range []string{"INTERN_P2_PASSWORD", "INTERN_P2_DENIED_PASSWORD"} {
			if secret := os.Getenv(key); secret != "" {
				message = strings.ReplaceAll(message, secret, "[REDACTED]")
			}
		}
		t.Fatal(message)
	}
}

func rejectCheck(t *testing.T, code string, action func() error) {
	t.Helper()
	err := action()
	var failure *physical.PreflightError
	if !errors.As(err, &failure) || failure.Code != code {
		t.Fatalf("expected preflight code %s; observed %v", code, err)
	}
}

func runBackupChain(t *testing.T, ctx context.Context, db *sql.DB, opts physical.PreflightOptions, serverUUID string) {
	t.Helper()
	var parent []byte
	var previous uint64
	for index := range 3 {
		if index > 0 {
			advanceCheckpointPast(t, ctx, db, previous)
		}
		snapshot := t.TempDir()
		source := physical.BackupSource{
			Binary: extraLSNDirWrapper(t, snapshot), WorkRoot: opts.WorkRoot, Connection: opts.Connection,
			ParentCheckpoints: parent, Parallel: 1, Timeout: 90 * time.Second,
		}
		stream, err := os.CreateTemp(opts.BackupRoot, "stream-*")
		must(t, err)
		must(t, source.Run(ctx, stream))
		must(t, stream.Close())
		metadata, err := source.Metadata()
		must(t, err)
		if index == 0 && (metadata.Checkpoints.BackupType != "full-backuped" || metadata.Checkpoints.FromLSN != 0) {
			t.Fatal("invalid full checkpoints")
		}
		if index > 0 && (metadata.Checkpoints.BackupType != "incremental" || metadata.Checkpoints.FromLSN != previous) {
			t.Fatal("incremental does not continue real parent LSN")
		}
		if metadata.Checkpoints.ToLSN <= metadata.Checkpoints.FromLSN {
			t.Fatalf("checkpoint to_lsn %d did not advance past from_lsn %d", metadata.Checkpoints.ToLSN, metadata.Checkpoints.FromLSN)
		}
		streamSize, directorySize := assertAuthoritativeMetadata(t, ctx, stream.Name(), snapshot, metadata)
		assertMemfdCommand(t, metadata)
		assertRealMetadataContract(t, metadata, serverUUID)
		t.Logf("P2-A4 backup=%d from_lsn=%d to_lsn=%d last_lsn=%d extra_lsndir_bytes=exact stream_checkpoints=exact stream_info_backup_size=%d extra_lsndir_backup_size=%d login_path=ignored", index, metadata.Checkpoints.FromLSN, metadata.Checkpoints.ToLSN, metadata.Checkpoints.LastLSN, streamSize, directorySize)
		parent = metadata.CheckpointsRaw
		previous = metadata.Checkpoints.ToLSN
	}
}

// extraLSNDirWrapper runs the official xtrabackup and copies the --extra-lsndir
// files the tool wrote. The copy finishes before the wrapper exits, so it is
// independent of ReadMetadata and survives work-directory removal. The wrapper
// does not interpret xbstream.
func extraLSNDirWrapper(t *testing.T, snapshotDir string) string {
	t.Helper()
	if strings.ContainsAny(snapshotDir, "'\n") {
		t.Fatal("snapshot directory cannot be embedded in the tool wrapper")
	}
	path := filepath.Join(t.TempDir(), "xtrabackup")
	body := `#!/bin/sh
extra=
previous=
for arg in "$@"; do
  if [ "$previous" = "--extra-lsndir" ]; then
    extra=$arg
  fi
  case "$arg" in
    --extra-lsndir=*) extra=${arg#--extra-lsndir=} ;;
  esac
  previous=$arg
done
/usr/bin/xtrabackup "$@"
status=$?
if [ "$status" -eq 0 ] && [ -n "$extra" ]; then
  cp -- "$extra/xtrabackup_checkpoints" '` + snapshotDir + `/xtrabackup_checkpoints' &&
    cp -- "$extra/xtrabackup_info" '` + snapshotDir + `/xtrabackup_info' ||
    echo "extra-lsndir snapshot failed" > '` + snapshotDir + `/snapshot-error'
fi
exit "$status"
`
	must(t, os.WriteFile(path, []byte(body), 0o700))
	return path
}

// assertAuthoritativeMetadata proves Metadata raw bytes are the tool's
// --extra-lsndir files. Official xbstream extraction must match those
// checkpoint bytes. The streamed info file is the earlier rendering: every
// field matches except backup_size, which is sampled again after that chunk
// is counted.
func assertAuthoritativeMetadata(t *testing.T, ctx context.Context, streamPath, snapshotDir string, metadata physical.Metadata) (streamSize, directorySize uint64) {
	t.Helper()
	checkpoints, info := readExtraSnapshot(t, snapshotDir)
	if !bytes.Equal(checkpoints, metadata.CheckpointsRaw) {
		t.Fatalf("Metadata.CheckpointsRaw is not the extra-lsndir file (%d vs %d bytes)", len(metadata.CheckpointsRaw), len(checkpoints))
	}
	if !bytes.Equal(info, metadata.InfoRaw) {
		t.Fatalf("Metadata.InfoRaw is not the extra-lsndir file (%d vs %d bytes)", len(metadata.InfoRaw), len(info))
	}
	extracted := extractXBStream(t, ctx, streamPath)
	streamedCheckpoints, err := os.ReadFile(filepath.Join(extracted, "xtrabackup_checkpoints"))
	must(t, err)
	if !bytes.Equal(streamedCheckpoints, metadata.CheckpointsRaw) {
		t.Fatal("streamed xtrabackup_checkpoints is not the extra-lsndir bytes")
	}
	streamedInfo, err := os.ReadFile(filepath.Join(extracted, "xtrabackup_info"))
	must(t, err)
	return compareStreamInfo(t, streamedInfo, info)
}

func readExtraSnapshot(t *testing.T, snapshotDir string) (checkpoints, info []byte) {
	t.Helper()
	var err error
	checkpoints, err = os.ReadFile(filepath.Join(snapshotDir, "xtrabackup_checkpoints"))
	if err != nil {
		note, _ := os.ReadFile(filepath.Join(snapshotDir, "snapshot-error"))
		t.Fatalf("official extra-lsndir checkpoint snapshot missing: %v %s", err, note)
	}
	info, err = os.ReadFile(filepath.Join(snapshotDir, "xtrabackup_info"))
	if err != nil {
		note, _ := os.ReadFile(filepath.Join(snapshotDir, "snapshot-error"))
		t.Fatalf("official extra-lsndir info snapshot missing: %v %s", err, note)
	}
	return checkpoints, info
}

func extractXBStream(t *testing.T, ctx context.Context, streamPath string) string {
	t.Helper()
	extracted := t.TempDir()
	input, err := os.Open(streamPath)
	must(t, err)
	command := exec.CommandContext(ctx, "/usr/bin/xbstream", "-x", "-C", extracted)
	command.Stdin = input
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	must(t, command.Run())
	must(t, input.Close())
	return extracted
}

func compareStreamInfo(t *testing.T, streamedInfo, directoryInfo []byte) (streamSize, directorySize uint64) {
	t.Helper()
	streamFields := infoFields(t, streamedInfo)
	directoryFields := infoFields(t, directoryInfo)
	if len(streamFields) != len(directoryFields) {
		t.Fatalf("stream info has %d fields, extra-lsndir has %d", len(streamFields), len(directoryFields))
	}
	for key, value := range directoryFields {
		streamValue, ok := streamFields[key]
		if !ok {
			t.Fatalf("stream info is missing %s", key)
		}
		if key == "backup_size" {
			streamSize = parseDecimal(t, streamValue)
			directorySize = parseDecimal(t, value)
			continue
		}
		if streamValue != value {
			t.Fatalf("stream info %s differs from the extra-lsndir rendering", key)
		}
	}
	if _, ok := streamFields["backup_size"]; !ok {
		t.Fatal("backup_size missing from streamed info")
	}
	if directorySize <= streamSize {
		t.Fatalf("extra-lsndir backup_size %d is not the later sample of streamed backup_size %d", directorySize, streamSize)
	}
	return streamSize, directorySize
}

func infoFields(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	if len(raw) == 0 || bytes.Contains(raw, []byte{0}) {
		t.Fatal("info file is empty or contains NUL")
	}
	fields := make(map[string]string)
	for line := range strings.SplitSeq(strings.TrimSuffix(string(raw), "\n"), "\n") {
		key, value, ok := strings.Cut(line, " = ")
		if !ok || key == "" || strings.Contains(key, " ") {
			t.Fatal("info line is not key = value")
		}
		if _, exists := fields[key]; exists {
			t.Fatalf("duplicate info field %s", key)
		}
		fields[key] = value
	}
	return fields
}

func parseDecimal(t *testing.T, value string) uint64 {
	t.Helper()
	number, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		t.Fatalf("backup_size %q is not an unsigned integer", value)
	}
	return number
}

func runRealFailures(t *testing.T, ctx context.Context, opts physical.PreflightOptions) {
	t.Helper()
	assertNoXtrabackupProcess(t)
	t.Run("real-downstream-failure", func(t *testing.T) {
		out := &rejectWriter{}
		source := physical.BackupSource{Binary: opts.Binary, WorkRoot: opts.WorkRoot, Connection: opts.Connection, Parallel: 1, Timeout: time.Minute}
		err := source.Run(ctx, out)
		if !out.reached || !errors.Is(err, errInjectedDownstream) || errors.Is(err, context.Canceled) {
			t.Fatalf("real output failure = %v, reached = %v", err, out.reached)
		}
		if _, metadataErr := source.Metadata(); metadataErr == nil {
			t.Fatal("failed stream exposed successful metadata")
		}
		assertNoXtrabackupProcess(t)
		assertWorkGone(t, opts.WorkRoot)
		t.Log("P2-A3 real downstream errors.Is injected; xtrabackup processes gone")
	})
	t.Run("real-cancellation", func(t *testing.T) {
		runCtx, stop := context.WithCancel(ctx)
		defer stop()
		out := &cancelWriter{cancel: stop}
		source := physical.BackupSource{Binary: opts.Binary, WorkRoot: opts.WorkRoot, Connection: opts.Connection, Parallel: 1, Timeout: time.Minute}
		err := source.Run(runCtx, out)
		if !out.reached || !errors.Is(err, context.Canceled) || errors.Is(err, errInjectedDownstream) {
			t.Fatalf("real cancellation = %v, reached = %v", err, out.reached)
		}
		assertNoXtrabackupProcess(t)
		assertWorkGone(t, opts.WorkRoot)
		t.Log("P2-A3 real cancellation errors.Is context.Canceled; xtrabackup processes gone")
	})
	runRealWatchdog(t, ctx, opts)
	assertWorkGone(t, opts.WorkRoot)
}

func runRealWatchdog(t *testing.T, ctx context.Context, opts physical.PreflightOptions) {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "xtrabackup-stopped")
	source := physical.BackupSource{
		Binary: watchdogWrapper(t, marker), WorkRoot: opts.WorkRoot, Connection: opts.Connection,
		Parallel: 1, Timeout: 45 * time.Second,
	}
	err := source.Run(ctx, io.Discard)
	pid, stopped := readStoppedMarker(marker)
	if !stopped || !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errInjectedDownstream) {
		note, _ := os.ReadFile(marker + ".note")
		t.Fatalf("real watchdog = %v, stopped = %t, note = %q", err, stopped, strings.TrimSpace(string(note)))
	}
	if !processReaped(pid) {
		t.Fatalf("watchdog left stopped xtrabackup %d alive", pid)
	}
	assertNoXtrabackupProcess(t)
	assertWorkGone(t, opts.WorkRoot)
	t.Logf("P2-A3 real watchdog errors.Is DeadlineExceeded; stopped pid %d reaped; xtrabackup processes gone", pid)
}

// watchdogWrapper starts the official xtrabackup and SIGSTOPs that child once
// its executable name is xtrabackup. The marker is written only after /proc
// state is T for that same process, still in this wrapper's process group.
// Seeing the process earlier is not enough. The wrapper keeps checking that
// the stopped tool is live until BackupSource's deadline SIGKILLs the group.
// BackupSource accepts only a basename of xtrabackup, so the wrapper uses that
// name and still executes /usr/bin/xtrabackup.
func watchdogWrapper(t *testing.T, marker string) string {
	t.Helper()
	if strings.ContainsAny(marker, "'\n\\") {
		t.Fatal("watchdog marker path cannot be embedded")
	}
	path := filepath.Join(t.TempDir(), "xtrabackup")
	body := `#!/bin/sh
set -eu
marker='` + marker + `'
proc_comm() {
  cat "/proc/$1/comm" 2>/dev/null || true
}
proc_stat_field() {
  raw=$(cat "/proc/$1/stat" 2>/dev/null || true)
  tail=${raw##*)}
  tail=${tail# }
  n=$2
  while [ "$n" -gt 1 ]; do
    tail=${tail#* }
    n=$((n - 1))
  done
  printf '%s' "${tail%% *}"
}
pause() {
  sleep 0.1 2>/dev/null || sleep 1
}
fail() {
  printf '%s\n' "$1" > "$marker.note"
  exit 1
}
/usr/bin/xtrabackup "$@" >/dev/null 2>&1 &
child=$!
if [ -z "$child" ] || [ "$child" -eq "$$" ]; then
  fail "refusing to stop the wrapper itself"
fi
deadline=$(($(date +%s) + 15))
while [ "$(date +%s)" -lt "$deadline" ]; do
  if [ "$(proc_comm "$child")" = "xtrabackup" ]; then
    break
  fi
  pause
done
if [ "$(proc_comm "$child")" != "xtrabackup" ]; then
  fail "real xtrabackup was not observed before it could be stopped"
fi
if ! kill -s STOP "$child" 2>/dev/null; then
  fail "real xtrabackup exited before it could be stopped"
fi
deadline=$(($(date +%s) + 5))
state=
while [ "$(date +%s)" -lt "$deadline" ]; do
  if [ "$(proc_comm "$child")" != "xtrabackup" ]; then
    fail "real xtrabackup exited while being stopped"
  fi
  state=$(proc_stat_field "$child" 1)
  if [ "$state" = "T" ]; then
    break
  fi
  pause
done
state=$(proc_stat_field "$child" 1)
comm=$(proc_comm "$child")
pgrp=$(proc_stat_field "$child" 3)
if [ "$state" != "T" ] || [ "$comm" != "xtrabackup" ] || [ "$pgrp" != "$$" ]; then
  fail "real xtrabackup was not a stopped group member (comm=$comm state=$state pgrp=$pgrp)"
fi
printf '%s T\n' "$child" > "$marker"
while :; do
  comm=$(proc_comm "$child")
  state=$(proc_stat_field "$child" 1)
  if [ "$comm" != "xtrabackup" ] || [ "$state" != "T" ]; then
    fail "stopped xtrabackup did not stay live until the watchdog"
  fi
  pause
done
`
	must(t, os.WriteFile(path, []byte(body), 0o700))
	return path
}

// readStoppedMarker accepts only a marker written after /proc state T.
// A file that merely records that a process existed is not acceptance.
func readStoppedMarker(path string) (int, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	line, rest, found := strings.Cut(string(raw), "\n")
	if found && rest != "" {
		return 0, false
	}
	pidText, state, ok := strings.Cut(line, " ")
	if !ok || state != "T" {
		return 0, false
	}
	pid, err := strconv.Atoi(pidText)
	if err != nil || pid <= 1 || pid == os.Getpid() {
		return 0, false
	}
	return pid, true
}

func processReaped(pid int) bool {
	return pid > 1 && errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

func runDirectBackup(t *testing.T, ctx context.Context, opts physical.PreflightOptions) physical.Metadata {
	t.Helper()
	stream, err := os.CreateTemp(opts.BackupRoot, "pid1-stream-*")
	must(t, err)
	source := physical.BackupSource{
		Binary: opts.Binary, WorkRoot: opts.WorkRoot, Connection: opts.Connection,
		Parallel: 1, Timeout: 3 * time.Minute,
	}
	must(t, source.Run(ctx, stream))
	must(t, stream.Close())
	metadata, err := source.Metadata()
	must(t, err)
	return metadata
}

func assertMemfdCommand(t *testing.T, metadata physical.Metadata) {
	t.Helper()
	command := metadata.Info.Fields["tool_command"]
	if password := os.Getenv("INTERN_P2_PASSWORD"); password != "" {
		command = strings.ReplaceAll(command, password, "[REDACTED]")
	}
	defaults := strings.Index(command, "--defaults-file=/proc/self/fd/4")
	login := strings.Index(command, "--no-login-paths")
	if defaults < 0 || login < defaults || strings.Contains(command, "password=") {
		t.Fatalf("tool_command missing memfd defaults before --no-login-paths: %s", command)
	}
}

// assertRealMetadataContract checks a real xtrabackup_info file. Its uuid is
// the tool's SELECT UUID() value, not the server UUID, and lock_time is only
// a signed duration beside that backup's LSN interval.
func assertRealMetadataContract(t *testing.T, metadata physical.Metadata, serverUUID string) {
	t.Helper()
	backupUUID := metadata.Info.Fields["uuid"]
	if serverUUID == "" || backupUUID == "" || backupUUID == serverUUID || metadata.Info.UUID != backupUUID {
		t.Fatalf("backup UUID %q is not the xtrabackup_info identity distinct from server %q", metadata.Info.UUID, serverUUID)
	}
	lock, ok := metadata.Info.Fields["lock_time"]
	if !ok {
		t.Fatal("real xtrabackup_info omitted informational lock_time")
	}
	if _, err := strconv.ParseInt(lock, 10, 64); err != nil {
		t.Fatalf("lock_time %q is not a signed informational duration", lock)
	}
	if metadata.Info.FromLSN != metadata.Checkpoints.FromLSN || metadata.Info.ToLSN != metadata.Checkpoints.ToLSN {
		t.Fatal("informational lock_time was stored beside a mismatched LSN interval")
	}
	t.Logf("backup_uuid_provenance=xtrabackup_info backup_uuid=%s server_uuid=%s lock_time=%s lock_time_informational", backupUUID, serverUUID, lock)
}

func assertPoisonedLoginFile(t *testing.T) {
	t.Helper()
	if os.Getenv("MYSQL_TEST_LOGIN_FILE") != "" {
		t.Fatal("MYSQL_TEST_LOGIN_FILE would be stripped before --no-login-paths; place the fixture at HOME/.mylogin.cnf")
	}
	home := os.Getenv("HOME")
	if home == "" {
		t.Fatal("worker HOME is unset, so .mylogin.cnf would not be found")
	}
	path := filepath.Join(home, ".mylogin.cnf")
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("poisoned login file is missing at HOME/.mylogin.cnf: %v", err)
	}
	if !workerOwnedLogin(info) {
		t.Fatal("poisoned login file is not a worker-owned 0600 regular file")
	}
	t.Log("P2-A3 poisoned login file is worker-owned HOME/.mylogin.cnf")
}

func workerOwnedLogin(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && info.Mode().IsRegular() && info.Mode().Perm() == 0o600 && stat.Uid == uint32(os.Geteuid())
}

func poisonedLoginRejected(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(text, "intern_denied") && (strings.Contains(lower, "access denied") || strings.Contains(text, "1045"))
}

// assertPoisonOverridesDefaults runs the official xtrabackup with the real
// credentials in a defaults file and without --no-login-paths. HOME's native
// login file must replace the username with the denied account and be rejected.
func assertPoisonOverridesDefaults(t *testing.T, password string) {
	t.Helper()
	assertPoisonedLoginFile(t)
	if os.Getenv("HOME") == "" || os.Getenv("MYSQL_TEST_LOGIN_FILE") != "" {
		t.Fatal("poison probe must use HOME/.mylogin.cnf after MYSQL_TEST_LOGIN_FILE is removed")
	}
	if strings.ContainsAny(password, "\r\n\"\\") {
		t.Fatal("fixture password cannot be embedded")
	}
	dir := t.TempDir()
	defaults := filepath.Join(dir, "client.cnf")
	body := "[client]\nuser=\"intern_bkp\"\npassword=\"" + password + "\"\n"
	must(t, os.WriteFile(defaults, []byte(body), 0o600))
	defer func() { _ = os.Remove(defaults) }()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/usr/bin/xtrabackup",
		"--defaults-file="+defaults, "--backup",
		"--socket=/var/run/mysqld/mysqld.sock", "--datadir=/var/lib/mysql",
		"--target-dir="+filepath.Join(dir, "rejected"))
	command.Env = withoutMySQLOverrides(os.Environ())
	output, err := command.CombinedOutput()
	if strings.Contains(string(output), password) {
		t.Fatal("poison probe leaked the backup credential")
	}
	if err == nil || ctx.Err() != nil || !poisonedLoginRejected(string(output)) {
		t.Fatal("xtrabackup without --no-login-paths did not use the denied login-path account")
	}
	t.Log("P2-A3 poisoned login path rejected without --no-login-paths")
}

func withoutMySQLOverrides(base []string) []string {
	kept := make([]string, 0, len(base))
	for _, value := range base {
		if strings.HasPrefix(value, "MYSQL_PWD=") || strings.HasPrefix(value, "MYSQL_TEST_LOGIN_FILE=") || strings.HasPrefix(value, "MYSQL_HOME=") {
			continue
		}
		kept = append(kept, value)
	}
	return kept
}

func assertWorkGone(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	must(t, err)
	if len(entries) != 0 {
		t.Fatalf("runner leaked private work or credential files: %d", len(entries))
	}
}

func linuxAMD64Candidate(t *testing.T) physical.CompatibilityEntry {
	t.Helper()
	entries, err := physical.CompatibilityMatrix()
	must(t, err)
	var found physical.CompatibilityEntry
	count := 0
	for _, entry := range entries {
		if entry.Distribution != "mysql-community" || entry.Platform != "linux/amd64" {
			continue
		}
		found = entry
		count++
	}
	if count != 1 || found.ServerVersion == "" || found.ToolVersion == "" {
		t.Fatal("expected one linux/amd64 community candidate")
	}
	return found
}

// advanceCheckpointPast moves the InnoDB checkpoint LSN, which XtraBackup stores
// as to_lsn, strictly past the previous backup. The native harness keeps the
// redo log at 32MiB, so writing twice that capacity cannot leave the checkpoint
// behind the parent. A quiet server would otherwise repeat to_lsn.
func advanceCheckpointPast(t *testing.T, ctx context.Context, db *sql.DB, after uint64) {
	t.Helper()
	limit := redoCapacity(t, ctx, db) * 2
	if limit > 256<<20 {
		t.Fatalf("redo capacity %d is too large to force a checkpoint within the scenario", limit/2)
	}
	payload := bytes.Repeat([]byte{'a'}, 256*1024)
	deadline := time.Now().Add(60 * time.Second)
	var written uint64
	var checkpoint uint64
	for {
		if written < limit {
			_, err := db.ExecContext(ctx, "INSERT INTO fixture.lsn_pressure(payload) VALUES (?)", payload)
			must(t, err)
			written += uint64(len(payload))
		}
		checkpoint = serverCheckpoint(t, ctx, db)
		if checkpoint > after {
			t.Logf("P2-A4 server checkpoint %d moved past parent to_lsn %d after %d bytes", checkpoint, after, written)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("server checkpoint %d did not pass parent to_lsn %d after %d bytes", checkpoint, after, written)
		}
		if written >= limit {
			time.Sleep(100 * time.Millisecond)
		}
	}
}

func redoCapacity(t *testing.T, ctx context.Context, db *sql.DB) uint64 {
	t.Helper()
	var value string
	must(t, db.QueryRowContext(ctx, "SELECT VARIABLE_VALUE FROM performance_schema.global_variables WHERE VARIABLE_NAME = 'innodb_redo_log_capacity'").Scan(&value))
	capacity, err := strconv.ParseUint(value, 10, 64)
	must(t, err)
	if capacity == 0 {
		t.Fatal("innodb_redo_log_capacity is zero")
	}
	return capacity
}

func serverCheckpoint(t *testing.T, ctx context.Context, db *sql.DB) uint64 {
	t.Helper()
	var raw string
	must(t, db.QueryRowContext(ctx, "SELECT storage_engines FROM performance_schema.log_status").Scan(&raw))
	var body struct {
		InnoDB struct {
			Checkpoint uint64 `json:"LSN_checkpoint"`
		} `json:"InnoDB"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil || body.InnoDB.Checkpoint == 0 {
		t.Fatalf("cannot read InnoDB checkpoint from log_status: %v", err)
	}
	return body.InnoDB.Checkpoint
}

func assertNoXtrabackupProcess(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	must(t, err)
	self := strconv.Itoa(os.Getpid())
	var left []string
	for _, entry := range entries {
		if entry.Name() == self || !digitsOnly(entry.Name()) {
			continue
		}
		dir := filepath.Join("/proc", entry.Name())
		comm, commErr := os.ReadFile(filepath.Join(dir, "comm"))
		raw, cmdErr := os.ReadFile(filepath.Join(dir, "cmdline"))
		if commErr != nil && cmdErr != nil {
			continue
		}
		if xtrabackupProcess(string(comm), raw) {
			left = append(left, entry.Name()+" "+strings.TrimSpace(string(comm)))
		}
	}
	if len(left) != 0 {
		t.Fatalf("xtrabackup processes still exist: %s", strings.Join(left, ", "))
	}
}

func xtrabackupProcess(comm string, cmdline []byte) bool {
	if strings.TrimSpace(comm) == "xtrabackup" {
		return true
	}
	for _, part := range bytes.Split(bytes.TrimRight(cmdline, "\x00"), []byte{0}) {
		if len(part) > 0 && filepath.Base(string(part)) == "xtrabackup" {
			return true
		}
	}
	return false
}

func digitsOnly(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
