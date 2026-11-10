package chain

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/flyingEirc/Intern/internal/pgtest"
	"github.com/flyingEirc/Intern/internal/physical/store"
	"github.com/flyingEirc/Intern/internal/storage/local"
)

func recoveryStore(t *testing.T) (*store.Store, *sql.DB) {
	t.Helper()
	db := pgtest.Open(t)
	check(t, store.Migrate(t.Context(), db))
	return store.New(db), db
}

func seedRecovery(t *testing.T, s *store.Store, work, instance, runID string) string {
	t.Helper()
	request := store.PlanRequest{SourceID: runID, ServerUUID: "server", ServerVersion: "8.4.11", ToolVersion: "8.4.0-7", Kind: "full", MaxChainLength: 8}
	plan, err := s.Plan(t.Context(), request)
	check(t, err)
	object := instance + "/" + runID + "/" + plan.ChainID + "/0001-" + runID
	check(t, s.Begin(t.Context(), store.BeginRequest{Plan: plan, SourceID: runID, ServerUUID: request.ServerUUID, ServerVersion: request.ServerVersion, ToolVersion: request.ToolVersion, InstanceID: instance, RunID: runID, BackupID: runID, ObjectPath: object, StagingToken: runID, WorkDir: filepath.Join(work, "run-"+runID), KeyID: "key", CreatedAt: time.Now()}))
	return object
}

func TestRecoverReclaimsOnlyOwnedInactiveLedger(t *testing.T) {
	s, db := recoveryStore(t)
	work, repository := recoveryWorkRoot(t), filepath.Join(t.TempDir(), "repository")
	repo, err := local.Open(repository)
	check(t, err)
	defer func() { check(t, repo.Close()) }()
	for _, run := range []string{"dead", "active", "foreign"} {
		instance := "instance"
		if run == "foreign" {
			instance = "another"
		}
		object := seedRecovery(t, s, work, instance, run)
		check(t, os.Mkdir(filepath.Join(work, "run-"+run), 0o700))
		check(t, os.WriteFile(filepath.Join(work, "run-"+run, "sentinel"), []byte(run), 0o600))
		_, err = repo.BeginOwned(object, run)
		check(t, err)
	}
	check(t, Recover(t.Context(), s, repo, work, "instance", []string{"active"}))
	check(t, Recover(t.Context(), s, repo, work, "instance", []string{"active"}))
	dead, err := s.Backup(t.Context(), "dead")
	check(t, err)
	if dead.Status != "failed" || dead.FailureCode != "interrupted" {
		t.Fatal("dead run is not interrupted")
	}
	assertRecoveryCount(t, db, "dead", 0)
	for _, target := range []string{filepath.Join(work, "run-dead"), filepath.Join(repository, ".pending-dead")} {
		if _, err = os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("dead run directory survived")
		}
	}
	assertLiveRecoveryTargets(t, db, work, repository)
}

func assertLiveRecoveryTargets(t *testing.T, db *sql.DB, work, repository string) {
	t.Helper()
	for _, run := range []string{"active", "foreign"} {
		assertRecoveryCount(t, db, run, 3)
		data, err := os.ReadFile(filepath.Join(work, "run-"+run, "sentinel"))
		check(t, err)
		if string(data) != run {
			t.Fatal("recovery touched live or foreign bytes")
		}
		if _, err = os.Stat(filepath.Join(repository, ".pending-"+run)); err != nil {
			t.Fatal("recovery touched live or foreign pending")
		}
	}
}

func TestRecoverRefusesSymlinkWorkTargetAndRetainsLedger(t *testing.T) {
	s, db := recoveryStore(t)
	work, repository, outside := recoveryWorkRoot(t), filepath.Join(t.TempDir(), "repository"), t.TempDir()
	repo, err := local.Open(repository)
	check(t, err)
	defer func() { check(t, repo.Close()) }()
	seedRecovery(t, s, work, "instance", "dead")
	check(t, os.WriteFile(filepath.Join(outside, "sentinel"), []byte("untouched"), 0o600))
	check(t, os.Symlink(outside, filepath.Join(work, "run-dead")))
	if err = Recover(t.Context(), s, repo, work, "instance", nil); err == nil {
		t.Fatal("symlink cleanup succeeded")
	}
	assertRecoveryCount(t, db, "dead", 1)
	var attempts int
	var code string
	check(t, db.QueryRowContext(t.Context(), "SELECT attempts,last_error_code FROM cleanup_ledger WHERE backup_id='dead'").Scan(&attempts, &code))
	if attempts != 1 || code != "cleanup_failed" {
		t.Fatal("failed cleanup was not retained")
	}
	data, err := os.ReadFile(filepath.Join(outside, "sentinel"))
	check(t, err)
	if string(data) != "untouched" {
		t.Fatal("cleanup escaped owned work root")
	}
	check(t, os.Remove(filepath.Join(work, "run-dead")))
	check(t, Recover(t.Context(), s, repo, work, "instance", nil))
	assertRecoveryCount(t, db, "dead", 0)
}

func assertRecoveryCount(t *testing.T, db *sql.DB, id string, want int) {
	t.Helper()
	var count int
	check(t, db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM cleanup_ledger WHERE backup_id=$1", id).Scan(&count))
	if count != want {
		t.Fatalf("ledger rows=%d want=%d", count, want)
	}
}

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func recoveryWorkRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "work")
	check(t, os.Mkdir(root, 0o700))
	return root
}
