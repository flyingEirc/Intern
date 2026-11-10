//go:build integration

package acceptance

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/flyingEirc/Intern/internal/artifact"
	"github.com/flyingEirc/Intern/internal/pgtest"
	"github.com/flyingEirc/Intern/internal/physical"
	"github.com/flyingEirc/Intern/internal/physical/chain"
	"github.com/flyingEirc/Intern/internal/physical/store"
	"github.com/flyingEirc/Intern/internal/storage/local"
)

func TestP3RecoveryWorker(t *testing.T) {
	pgtest.RequireDSN(t)
	dsn := pgtest.New(t)
	t.Setenv("INTERN_P3_DATABASE_URL", dsn)
	root := t.TempDir()
	for _, dir := range []string{"work", "repo"} {
		must(t, os.Mkdir(filepath.Join(root, dir), 0o700))
	}
	must(t, artifact.GenerateKey(filepath.Join(root, "key.json")))
	db := pgtest.OpenDSN(t, dsn)
	must(t, store.Migrate(t.Context(), db))
	s := store.New(db)
	repo, err := local.Open(filepath.Join(root, "repo"))
	must(t, err)
	t.Cleanup(func() { must(t, repo.Close()) })
	seedP3Owned(t, root, s, repo, "p3-instance", "active")
	seedP3Owned(t, root, s, repo, "foreign-instance", "foreign")
	executable, err := os.Executable()
	must(t, err)
	command := exec.CommandContext(t.Context(), executable, "-test.v", "-test.run=^TestP3KillChildWorker$", "-test.timeout=3m")
	command.Env = append(os.Environ(), "INTERN_P3_RECOVERY_ROOT="+root)
	var diagnostic bytes.Buffer
	command.Stdout, command.Stderr = &diagnostic, &diagnostic
	must(t, command.Start())
	t.Cleanup(func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	waitP3Ready(t, filepath.Join(root, "ready"))
	var id, runID, object string
	must(t, db.QueryRowContext(t.Context(), "SELECT id,run_id,object_path FROM backups WHERE instance_id='p3-instance' AND status='running' AND run_id != 'active'").Scan(&id, &runID, &object))
	assertP3Ledger(t, db, id, 3)
	pending := filepath.Join(root, "repo", ".pending-"+runID)
	work := filepath.Join(root, "work", "run-"+runID)
	info, err := os.Stat(filepath.Join(pending, "payload.bin"))
	must(t, err)
	if info.Size() == 0 {
		t.Fatal("kill boundary preceded real payload I/O")
	}
	if _, err = os.Stat(work); err != nil {
		t.Fatal("kill boundary has no owned work directory")
	}
	killP3Worker(t, command)
	restartP3Worker(t, executable, root)
	backup, err := s.Backup(t.Context(), id)
	must(t, err)
	if backup.Status != "failed" || backup.FailureCode != "interrupted" {
		t.Fatal("killed running row not marked interrupted")
	}
	assertP3Ledger(t, db, id, 0)
	for _, target := range []string{pending, work, filepath.Join(root, "repo", object)} {
		if _, err = os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("recovery left killed run target")
		}
	}
	assertP3OtherOwners(t, root, db, s)
	t.Log("P3-A4 kill=SIGKILL restart=real ledger=empty pending=gone work=gone active=untouched foreign=untouched second_recover=noop")
}

func killP3Worker(t *testing.T, command *exec.Cmd) {
	t.Helper()
	must(t, command.Process.Kill())
	waitErr := command.Wait()
	var exit *exec.ExitError
	if !errors.As(waitErr, &exit) {
		t.Fatal("worker was not killed")
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatal("worker exit was not SIGKILL")
	}
}

func restartP3Worker(t *testing.T, executable, root string) {
	t.Helper()
	// Re-exec, not an in-process call: recovery reopens the same committed WAL
	// database and uses only its persisted ownership ledger.
	restart := exec.CommandContext(t.Context(), executable, "-test.v", "-test.run=^TestP3RestartChildWorker$", "-test.timeout=1m")
	restart.Env = append(os.Environ(), "INTERN_P3_RECOVERY_ROOT="+root)
	output, err := restart.CombinedOutput()
	must(t, err)
	if !bytes.Contains(output, []byte("P3-A4 restart recovery completed")) || bytes.Contains(output, []byte("SKIP")) {
		t.Fatal("restart did not execute real recovery")
	}
}

func assertP3OtherOwners(t *testing.T, root string, db *sql.DB, s *store.Store) {
	t.Helper()
	for _, run := range []string{"active", "foreign"} {
		row, lookupErr := s.Backup(t.Context(), run)
		must(t, lookupErr)
		if row.Status != "running" {
			t.Fatal("recovery changed live or foreign row")
		}
		assertP3Ledger(t, db, run, 3)
		for _, target := range []string{filepath.Join(root, "repo", ".pending-"+run, "sentinel"), filepath.Join(root, "work", "run-"+run, "sentinel")} {
			data, readErr := os.ReadFile(target)
			must(t, readErr)
			if string(data) != run {
				t.Fatal("recovery changed another owner's bytes")
			}
		}
	}
}

func seedP3Owned(t *testing.T, root string, s *store.Store, repo *local.Repository, instance, runID string) {
	t.Helper()
	request := store.PlanRequest{SourceID: runID, ServerUUID: "00000000-0000-0000-0000-000000000001", ServerVersion: "8.4.11", ToolVersion: "8.4.0-7", Kind: "full", MaxChainLength: 8}
	plan, err := s.Plan(t.Context(), request)
	must(t, err)
	object := instance + "/" + runID + "/" + plan.ChainID + "/0001-" + runID
	work := filepath.Join(root, "work", "run-"+runID)
	must(t, s.Begin(t.Context(), store.BeginRequest{Plan: plan, SourceID: runID, ServerUUID: request.ServerUUID, ServerVersion: request.ServerVersion, ToolVersion: request.ToolVersion, InstanceID: instance, RunID: runID, BackupID: runID, ObjectPath: object, StagingToken: runID, WorkDir: work, KeyID: "fixture", CreatedAt: time.Now()}))
	must(t, os.Mkdir(work, 0o700))
	must(t, os.WriteFile(filepath.Join(work, "sentinel"), []byte(runID), 0o600))
	staging, err := repo.BeginOwned(object, runID)
	must(t, err)
	sink, err := staging.Create("sentinel")
	must(t, err)
	_, err = sink.Write([]byte(runID))
	must(t, err)
	_, err = sink.Commit(t.Context())
	must(t, err)
}

func waitP3Ready(t *testing.T, marker string) {
	t.Helper()
	timer := time.NewTimer(90 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if raw, err := os.ReadFile(marker); err == nil && string(raw) == "written" {
			return
		}
		select {
		case <-timer.C:
			t.Fatal("kill worker never reached real stream write")
		case <-t.Context().Done():
			t.Fatal("kill fixture canceled before injection")
		case <-ticker.C:
		}
	}
}

func TestP3KillChildWorker(t *testing.T) {
	pgtest.RequireDSN(t)
	root := os.Getenv("INTERN_P3_RECOVERY_ROOT")
	if !filepath.IsAbs(root) {
		t.Fatal("kill child requires owned fixture root")
	}
	db := pgtest.OpenDSN(t, os.Getenv("INTERN_P3_DATABASE_URL"))
	must(t, store.Migrate(t.Context(), db))
	repo, err := local.Open(filepath.Join(root, "repo"))
	must(t, err)
	defer func() { must(t, repo.Close()) }()
	opts := volumeOptions(t)
	opts.WorkRoot, opts.BackupRoot = filepath.Join(root, "work"), filepath.Join(root, "repo")
	preflight, err := physical.Preflight(t.Context(), opts)
	must(t, err)
	job := chain.Job{Store: store.New(db), Repo: repo, Binary: opts.Binary, WorkRoot: opts.WorkRoot, Connection: opts.Connection, Identity: preflight.Identity, ToolVersion: preflight.Tool.Version, InstanceID: "p3-instance", SourceID: "killed", Kind: "full", MaxChainLength: 8, KeyFile: filepath.Join(root, "key.json")}
	_, err = chain.RunWithFault(t.Context(), job, func(point string) error {
		if point == "write" {
			must(t, os.WriteFile(filepath.Join(root, "ready"), []byte("written"), 0o600))
			select {} // The parent must SIGKILL this separate worker at actual I/O.
		}
		return nil
	})
	must(t, err)
	t.Fatal("kill child unexpectedly completed")
}

func TestP3RestartChildWorker(t *testing.T) {
	pgtest.RequireDSN(t)
	root := os.Getenv("INTERN_P3_RECOVERY_ROOT")
	if !filepath.IsAbs(root) {
		t.Fatal("restart child requires owned fixture root")
	}
	db := pgtest.OpenDSN(t, os.Getenv("INTERN_P3_DATABASE_URL"))
	must(t, store.Migrate(t.Context(), db))
	repo, err := local.Open(filepath.Join(root, "repo"))
	must(t, err)
	defer func() { must(t, repo.Close()) }()
	s := store.New(db)
	must(t, chain.Recover(t.Context(), s, repo, filepath.Join(root, "work"), "p3-instance", []string{"active"}))
	before := p3RecoveryState(t, s)
	must(t, chain.Recover(t.Context(), s, repo, filepath.Join(root, "work"), "p3-instance", []string{"active"}))
	if after := p3RecoveryState(t, s); before != after {
		t.Fatal("second recovery changed ledger")
	}
	t.Log("P3-A4 restart recovery completed")
}

func p3RecoveryState(t *testing.T, s *store.Store) string {
	t.Helper()
	var state strings.Builder
	for _, instance := range []string{"p3-instance", "foreign-instance"} {
		rows, err := s.PendingCleanup(context.Background(), instance, nil)
		must(t, err)
		for _, row := range rows {
			state.WriteString(row.BackupID + ":" + row.Kind + ":" + row.Target + "\n")
		}
	}
	return state.String()
}
