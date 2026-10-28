package store

import (
	"database/sql"
	"errors"
	"testing"
)

func TestCleanupLedgerIsolationAndRetries(t *testing.T) {
	s := testStore(t)
	mustBegin(t, s, "full", "orphan")
	mustBegin(t, s, "full", "active")
	foreignPlan, err := s.Plan(t.Context(), planRequest("full"))
	if err != nil {
		t.Fatal(err)
	}
	foreign := beginRequest(foreignPlan, "foreign")
	foreign.InstanceID = "other-instance"
	if err = s.Begin(t.Context(), foreign); err != nil {
		t.Fatal(err)
	}
	if err = s.Fail(t.Context(), "orphan", "interrupted"); err != nil {
		t.Fatal(err)
	}
	pending := assertCleanupCount(t, s, "instance", []string{"active"}, 3)
	for _, cleanup := range pending {
		checkCleanupRetry(t, s, cleanup)
	}
	assertCleanupCount(t, s, "instance", []string{"active"}, 0)
	assertCleanupRun(t, s, "instance", "active")
	assertCleanupRun(t, s, "other-instance", "foreign")
}

func TestCompletionTransactionRetainsLedgerOnFailure(t *testing.T) {
	s := testStore(t)
	mustBegin(t, s, "full", "full")
	execSQL(t, s, `CREATE TRIGGER reject_cleanup BEFORE DELETE ON cleanup_ledger BEGIN SELECT RAISE(ABORT, 'injected metadata failure'); END`)
	if err := s.Complete(t.Context(), completion("full", "full-backuped", 0, 100)); err == nil {
		t.Fatal("metadata transaction failure ignored")
	}
	backup, err := s.Backup(t.Context(), "full")
	if err != nil || backup.Status != "running" || backup.LocalPresent || len(backup.CheckpointsRaw) != 0 {
		t.Fatalf("partial completion: %+v %v", backup, err)
	}
	pending, err := s.PendingCleanup(t.Context(), "instance", nil)
	if err != nil || len(pending) != 3 {
		t.Fatalf("lost cleanup after rollback: %+v %v", pending, err)
	}
	execSQL(t, s, "DROP TRIGGER reject_cleanup")
	mustComplete(t, s, completion("full", "full-backuped", 0, 100))
	// Even an externally restored stale ledger must never expose completed objects.
	execSQL(t, s, "INSERT INTO cleanup_ledger (backup_id, instance_id, run_id, kind, target, created_at) VALUES ('full', 'instance', 'full', 'object', 'stale', 'now')")
	if pending, err = s.PendingCleanup(t.Context(), "instance", nil); err != nil || len(pending) != 0 {
		t.Fatalf("completed object exposed for cleanup: %+v %v", pending, err)
	}
}

func TestPhysicalMigrateIdempotenceAndDowngrade(t *testing.T) {
	s := testStore(t)
	mustBegin(t, s, "full", "retained")
	for range 2 {
		if err := Migrate(t.Context(), s.db); err != nil {
			t.Fatal(err)
		}
	}
	current, latest, err := Version(t.Context(), s.db)
	if err != nil || current != 1 || latest != 1 {
		t.Fatalf("versions = %d %d %v", current, latest, err)
	}
	if _, err = s.Backup(t.Context(), "retained"); err != nil {
		t.Fatal(err)
	}
}

func TestPhysicalMigrateRejectsDowngrade(t *testing.T) {
	s := testStore(t)
	execSQL(t, s, "INSERT INTO physical_goose_db_version (version_id, is_applied) VALUES (99, 1)")
	if err := Migrate(t.Context(), s.db); err == nil {
		t.Fatal("downgrade accepted")
	}
	current, latest, err := Version(t.Context(), s.db)
	if err != nil || current != 99 || latest != 1 {
		t.Fatalf("future versions = %d %d %v", current, latest, err)
	}
	if _, err = s.Backup(t.Context(), "missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing backup: %v", err)
	}
}

func assertCleanupCount(t *testing.T, s *Store, instance string, active []string, count int) []Cleanup {
	t.Helper()
	pending, err := s.PendingCleanup(t.Context(), instance, active)
	if err != nil || len(pending) != count {
		t.Fatalf("cleanup for %s: %+v %v; want %d", instance, pending, err, count)
	}
	return pending
}

func assertCleanupRun(t *testing.T, s *Store, instance, run string) {
	t.Helper()
	pending := assertCleanupCount(t, s, instance, nil, 3)
	if pending[0].RunID != run {
		t.Fatalf("ownership lost: %+v; want %s", pending, run)
	}
}

func checkCleanupRetry(t *testing.T, s *Store, cleanup Cleanup) {
	t.Helper()
	if cleanup.BackupID != "orphan" || cleanup.RunID != "orphan" {
		t.Fatalf("wrong owner: %+v", cleanup)
	}
	if err := s.CleanupFailed(t.Context(), cleanup.ID, "permission_denied"); err != nil {
		t.Fatal(err)
	}
	var attempts int
	var code string
	if err := s.db.QueryRowContext(t.Context(), "SELECT attempts, last_error_code FROM cleanup_ledger WHERE id = ?", cleanup.ID).Scan(&attempts, &code); err != nil || attempts != 1 || code != "permission_denied" {
		t.Fatalf("retry record: %d %q %v", attempts, code, err)
	}
	for range 2 {
		if err := s.CleanupDone(t.Context(), cleanup.ID); err != nil {
			t.Fatal(err)
		}
	}
}
