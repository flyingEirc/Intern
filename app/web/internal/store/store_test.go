package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/flyingEirc/Intern/internal/pgtest"
)

func openTestStore(t *testing.T, dsn string) *Store {
	t.Helper()
	s, err := Open(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func openTestStoreDSN(t *testing.T) (*Store, string) {
	t.Helper()
	dsn := pgtest.New(t)
	s, err := Open(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, dsn
}

func TestPostgreSQLSettingsAndSingleConnection(t *testing.T) {
	if StatementTimeout != 5*time.Second {
		t.Fatalf("StatementTimeout = %s, want 5s", StatementTimeout)
	}
	s := openTestStore(t, pgtest.New(t))
	for name, want := range map[string]string{"statement_timeout": "5s", "lock_timeout": "5s"} {
		var got string
		if err := s.db.QueryRowContext(t.Context(), "SELECT current_setting($1)", name).Scan(&got); err != nil || got != want {
			t.Fatalf("%s = %q, %v; want %q", name, got, err, want)
		}
	}
	if got := s.db.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("max connections = %d", got)
	}
}

func TestInstanceIdentityPersists(t *testing.T) {
	s, dsn := openTestStoreDSN(t)
	id, err := uuid.Parse(s.InstanceID())
	if err != nil || id.Version() != 7 {
		t.Fatalf("instance ID = %q, %v", s.InstanceID(), err)
	}
	created := instanceCreatedAt(t, s)
	if _, err = s.db.ExecContext(t.Context(), "INSERT INTO instance VALUES (2, 'another', 'now')"); err == nil {
		t.Fatal("instance table allowed a second row")
	}
	if exists, queryErr := tableExists(t.Context(), s.db, "goose_db_version"); queryErr != nil || exists {
		t.Fatalf("default version table present: %v, %v", exists, queryErr)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := Open(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restarted.Close() })
	if restarted.InstanceID() != id.String() {
		t.Fatalf("instance ID changed: %s -> %s", id, restarted.InstanceID())
	}
	if restartedCreated := instanceCreatedAt(t, restarted); restartedCreated != created {
		t.Fatalf("instance creation time changed: %q -> %q", created, restartedCreated)
	}
}

func instanceCreatedAt(t *testing.T, s *Store) string {
	t.Helper()
	var created string
	if err := s.db.QueryRowContext(t.Context(), "SELECT created_at FROM instance").Scan(&created); err != nil {
		t.Fatal(err)
	}
	if _, err := time.Parse(time.RFC3339Nano, created); err != nil {
		t.Fatal(err)
	}
	return created
}

func TestExclusiveOwnershipAndReadOnlyStatus(t *testing.T) {
	s, dsn := openTestStoreDSN(t)
	if _, err := Open(t.Context(), dsn); !errors.Is(err, ErrOwnerInUse) {
		t.Fatalf("second writer: %v", err)
	}
	status, err := ReadStatus(t.Context(), dsn)
	if err != nil || status.InstanceID != s.InstanceID() || status.CurrentVersion != webLatest(t) ||
		status.LatestVersion != webLatest(t) || status.PendingMigrations != 0 {
		t.Fatalf("status under owner lock: %+v, %v", status, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := Open(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restarted.Close() })
}

func TestMigrationFailureAndFutureVersion(t *testing.T) {
	s, dsn := openTestStoreDSN(t)
	if _, err := s.db.ExecContext(t.Context(), "INSERT INTO web_goose_db_version (version_id, is_applied) VALUES (42, TRUE)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Ready(t.Context()); err == nil {
		t.Fatal("future schema was ready")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(t.Context(), dsn); err == nil || !strings.Contains(err.Error(), "42") {
		t.Fatalf("future version: %v", err)
	}
	status, err := ReadStatus(t.Context(), dsn)
	if err != nil || status.CurrentVersion != 42 {
		t.Fatalf("future status: %+v, %v", status, err)
	}
}

func TestReadyRequiresLiveExactVersion(t *testing.T) {
	s := openTestStore(t, pgtest.New(t))
	if err := s.Ready(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(t.Context(), fmt.Sprintf(
		"DELETE FROM web_goose_db_version WHERE version_id = %d", webLatest(t))); err != nil {
		t.Fatal(err)
	}
	if err := s.Ready(t.Context()); err == nil {
		t.Fatal("older version was ready")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Ready(t.Context()); err == nil {
		t.Fatal("closed database was ready")
	}
}

func TestCanceledCallerNeverPoisonsTheStore(t *testing.T) {
	s, dsn := openTestStoreDSN(t)
	source := createTestSource(t, s, "poison-source", "{{SECRET_poison_source}}")
	failed := PreflightRecord{OK: false, Checks: []string{}, Warnings: []PreflightWarning{}}
	stored := PreflightRecord{OK: true, Checks: []string{"identity"}, Warnings: []PreflightWarning{}}
	release := holdExternalWriteLock(t, dsn)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	result := make(chan error, 1)
	go func() {
		_, err := s.RecordSourcePreflight(ctx, source.ID, failed, SystemAudit("sources.preflight", AuditDetail{}))
		result <- err
	}()
	waitForConnection(t, s)
	cancel()
	time.Sleep(200 * time.Millisecond)
	release()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("write whose caller canceled mid-begin = %v, want the detached transaction to commit", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the canceled write never finished after the foreign lock was released")
	}
	write := func(what string, op func(context.Context) error) {
		t.Helper()
		start := time.Now()
		if err := op(context.Background()); err != nil {
			t.Fatalf("%s after a canceled writer: %v", what, err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("%s took %s; the single connection is poisoned", what, elapsed)
		}
	}
	write("audit", func(ctx context.Context) error { return s.Audit(ctx, SystemAudit("probe.audit", AuditDetail{})) })
	var updated Source
	write("preflight record", func(ctx context.Context) error {
		var err error
		updated, err = s.RecordSourcePreflight(ctx, source.ID, stored, SystemAudit("sources.preflight", AuditDetail{}))
		return err
	})
	if updated.LastPreflight == nil || !updated.LastPreflight.OK {
		t.Fatalf("preflight record = %+v", updated.LastPreflight)
	}
}

func waitForConnection(t *testing.T, s *Store) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for s.ConnectionsInUse() != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("connections in use = %d", s.ConnectionsInUse())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func holdExternalWriteLock(t *testing.T, dsn string) func() {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(t.Context(), "LOCK TABLE sources, users, sessions, session_tombstones, audit_logs IN EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := tx.Rollback(); err != nil {
			t.Errorf("release external PostgreSQL lock: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Errorf("close external PostgreSQL connection: %v", err)
		}
	}
}
