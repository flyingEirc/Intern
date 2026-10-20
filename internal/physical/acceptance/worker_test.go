//go:build integration

package acceptance

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	cfg := driver.NewConfig()
	cfg.Net, cfg.Addr, cfg.User, cfg.Passwd = "unix", "/var/run/mysqld/mysqld.sock", "intern_bkp", password
	cfg.Timeout, cfg.ReadTimeout, cfg.WriteTimeout = 5*time.Second, 10*time.Second, 10*time.Second
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
	if result.Identity.Version != "8.4.11" || result.Tool.Version != "8.4.0-7" {
		t.Fatalf("candidate version mismatch: mysql=%s pxb=%s", result.Identity.Version, result.Tool.Version)
	}
	if result.Identity.UUID == "" || result.Identity.PageSize != 16384 {
		t.Fatal("unexpected real source identity")
	}
	if err = os.WriteFile("/var/lib/mysql/intern-must-not-write", []byte("deny"), 0o600); err == nil {
		t.Fatal("datadir is not mounted read-only")
	}
	t.Logf("P2-A1 real versions mysql=%s pxb=%s platform=%s/%s uid=%d gid=%d datadir=readonly socket=shared", result.Identity.Version, result.Tool.Version, runtime.GOOS, runtime.GOARCH, os.Getuid(), os.Getgid())

	t.Run("preflight-refusals", func(t *testing.T) {
		low := opts
		low.MinWorkBytes = math.MaxUint64
		rejectCheck(t, "space_insufficient", func() error { _, e := physical.Preflight(ctx, low); return e })
		low = opts
		low.MinBackupBytes = math.MaxUint64
		rejectCheck(t, "space_insufficient", func() error { _, e := physical.Preflight(ctx, low); return e })
		bad := opts
		bad.Connection.Username = "intern_denied"
		rejectCheck(t, "privileges_missing", func() error { _, e := physical.Preflight(ctx, bad); return e })
		bad = opts
		bad.PageTracking = true
		rejectCheck(t, "page_tracking_unavailable", func() error { _, e := physical.Preflight(ctx, bad); return e })
		identity := result.Identity
		identity.Version = "9.7.1"
		reject(t, func() error { return physical.CheckCompatibility(identity, result.Tool, "linux/amd64") })
		reject(t, func() error { return physical.CheckPrepareVersion("8.4.0-7", "8.0.35-36") })
	})

	runBackupChain(t, ctx, db, opts)
	runRealFailures(t, ctx, opts)
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

type rejectWriter struct{ reached bool }

func (w *rejectWriter) Write(p []byte) (int, error) {
	w.reached = true
	return 0, errors.New("injected downstream failure")
}

type cancelWriter struct {
	cancel  context.CancelFunc
	reached bool
}

func (w *cancelWriter) Write(p []byte) (int, error) {
	w.reached = true
	w.cancel()
	return 0, context.Canceled
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		password := os.Getenv("INTERN_P2_PASSWORD")
		message := err.Error()
		if password != "" {
			message = strings.ReplaceAll(message, password, "[REDACTED]")
		}
		t.Fatal(message)
	}
}

func reject(t *testing.T, action func() error) {
	t.Helper()
	if action() == nil {
		t.Fatal("unsafe preflight accepted")
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

func runBackupChain(t *testing.T, ctx context.Context, db *sql.DB, opts physical.PreflightOptions) {
	t.Helper()
	var parent []byte
	var previous uint64
	for index := range 3 {
		source := physical.BackupSource{Binary: opts.Binary, WorkRoot: opts.WorkRoot, Connection: opts.Connection, ParentCheckpoints: parent, Parallel: 1, Timeout: 90 * time.Second}
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
			t.Fatal("real DML did not advance backup LSN")
		}
		assertRawMetadata(t, ctx, stream.Name(), metadata)
		t.Logf("P2-A4 backup=%d from_lsn=%d to_lsn=%d last_lsn=%d raw_metadata=byte_equal", index, metadata.Checkpoints.FromLSN, metadata.Checkpoints.ToLSN, metadata.Checkpoints.LastLSN)
		parent = metadata.CheckpointsRaw
		previous = metadata.Checkpoints.ToLSN
		_, err = db.ExecContext(ctx, "INSERT INTO fixture.records(value) VALUES (?)", index+1)
		must(t, err)
	}
}

func assertRawMetadata(t *testing.T, ctx context.Context, path string, metadata physical.Metadata) {
	t.Helper()
	extracted := t.TempDir()
	input, err := os.Open(path)
	must(t, err)
	command := exec.CommandContext(ctx, "/usr/bin/xbstream", "-x", "-C", extracted)
	command.Stdin = input
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	must(t, command.Run())
	must(t, input.Close())
	raw, err := os.ReadFile(filepath.Join(extracted, "xtrabackup_checkpoints"))
	must(t, err)
	if !bytes.Equal(raw, metadata.CheckpointsRaw) {
		t.Fatal("extra-lsndir checkpoints differ from actual stream")
	}
	raw, err = os.ReadFile(filepath.Join(extracted, "xtrabackup_info"))
	must(t, err)
	if !bytes.Equal(raw, metadata.InfoRaw) {
		t.Fatal("extra-lsndir info differs from actual stream")
	}
}

func runRealFailures(t *testing.T, ctx context.Context, opts physical.PreflightOptions) {
	t.Helper()
	work, connection := opts.WorkRoot, opts.Connection
	t.Run("real-downstream-failure", func(t *testing.T) {
		out := &rejectWriter{}
		source := physical.BackupSource{Binary: opts.Binary, WorkRoot: work, Connection: connection, Parallel: 1, Timeout: time.Minute}
		if source.Run(ctx, out) == nil || !out.reached {
			t.Fatal("real output failure not reached or accepted")
		}
		if _, metadataErr := source.Metadata(); metadataErr == nil {
			t.Fatal("failed stream exposed successful metadata")
		}
	})
	t.Run("real-cancellation", func(t *testing.T) {
		runCtx, stop := context.WithCancel(ctx)
		defer stop()
		out := &cancelWriter{cancel: stop}
		source := physical.BackupSource{Binary: opts.Binary, WorkRoot: work, Connection: connection, Parallel: 1, Timeout: time.Minute}
		if source.Run(runCtx, out) == nil || !out.reached {
			t.Fatal("real cancellation not reached or accepted")
		}
	})
	entries, err := os.ReadDir(work)
	must(t, err)
	if len(entries) != 0 {
		t.Fatal("runner leaked private work or credential files")
	}
	t.Log("P2-A3 real downstream failure and cancellation reached; work and credentials removed")
}
