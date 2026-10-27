package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func openTestStore(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func privateTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestPragmasAndPermissions(t *testing.T) {
	if BusyTimeout != 5*time.Second {
		t.Fatalf("BusyTimeout = %s, want 5s", BusyTimeout)
	}
	dir := filepath.Join(t.TempDir(), "metadata")
	s := openTestStore(t, dir)
	for pragma, want := range map[string]string{
		"journal_mode": "wal", "busy_timeout": strconv.FormatInt(BusyTimeout.Milliseconds(), 10),
		"synchronous": "2", "foreign_keys": "1",
	} {
		var got string
		if err := s.db.QueryRowContext(t.Context(), "PRAGMA "+pragma).Scan(&got); err != nil || got != want {
			t.Fatalf("PRAGMA %s = %q, %v; want %q", pragma, got, err, want)
		}
	}
	if got := s.db.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("max connections = %d", got)
	}
	for path, want := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, "intern.db"): 0o600, filepath.Join(dir, ".intern-web.lock"): 0o600} {
		st, err := os.Lstat(path)
		if err != nil || st.Mode().Perm() != want {
			t.Fatalf("mode of %s: %v, %v; want %o", path, st, err, want)
		}
	}
}

func TestInstanceIdentityPersists(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "metadata")
	s := openTestStore(t, dir)
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
	restarted := openTestStore(t, dir)
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
	dir := privateTempDir(t)
	s := openTestStore(t, dir)
	if _, err := Open(t.Context(), dir); err == nil || !strings.Contains(err.Error(), "another intern process") {
		t.Fatalf("second writer: %v", err)
	}
	status, err := ReadStatus(t.Context(), dir)
	if err != nil || status.InstanceID != s.InstanceID() || status.CurrentVersion != webLatest(t) ||
		status.LatestVersion != webLatest(t) || status.PendingMigrations != 0 {
		t.Fatalf("status under writer lock: %+v, %v", status, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	openTestStore(t, dir)
}

func TestStoreRejectsUnsafePaths(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*testing.T, string) string
	}{
		{"relative", func(_ *testing.T, _ string) string { return "relative" }},
		{"wide directory", func(t *testing.T, dir string) string { os.Chmod(dir, 0o755); return dir }},
		{"symlink directory", func(t *testing.T, dir string) string {
			link := filepath.Join(t.TempDir(), "link")
			if err := os.Symlink(dir, link); err != nil {
				t.Fatal(err)
			}
			return link
		}},
		{"wide database", func(t *testing.T, dir string) string {
			os.WriteFile(filepath.Join(dir, "intern.db"), nil, 0o644)
			return dir
		}},
		{"database directory", func(t *testing.T, dir string) string { os.Mkdir(filepath.Join(dir, "intern.db"), 0o700); return dir }},
		{"symlink database", func(t *testing.T, dir string) string {
			target := filepath.Join(t.TempDir(), "other.db")
			os.WriteFile(target, nil, 0o600)
			os.Symlink(target, filepath.Join(dir, "intern.db"))
			return dir
		}},
		{"symlink lock", func(t *testing.T, dir string) string {
			target := filepath.Join(t.TempDir(), "other.lock")
			os.WriteFile(target, nil, 0o600)
			os.Symlink(target, filepath.Join(dir, ".intern-web.lock"))
			return dir
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := tc.setup(t, privateTempDir(t))
			if s, err := Open(t.Context(), dir); err == nil {
				s.Close()
				t.Fatal("unsafe metadata path accepted")
			}
		})
	}
}

func TestMigrationFailureAndFutureVersion(t *testing.T) {
	t.Run("corrupt", func(t *testing.T) {
		dir := privateTempDir(t)
		os.WriteFile(filepath.Join(dir, "intern.db"), []byte("not a database"), 0o600)
		if _, err := Open(t.Context(), dir); err == nil {
			t.Fatal("corrupt database accepted")
		}
		os.Remove(filepath.Join(dir, "intern.db"))
		openTestStore(t, dir)
	})
	t.Run("future", func(t *testing.T) {
		dir := privateTempDir(t)
		s := openTestStore(t, dir)
		if _, err := s.db.ExecContext(t.Context(), "INSERT INTO web_goose_db_version (version_id, is_applied) VALUES (42, 1)"); err != nil {
			t.Fatal(err)
		}
		if err := s.Ready(t.Context()); err == nil {
			t.Fatal("future schema was ready")
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(t.Context(), dir); err == nil || !strings.Contains(err.Error(), "42") || !strings.Contains(err.Error(), "version 4") {
			t.Fatalf("future version: %v", err)
		}
		status, err := ReadStatus(t.Context(), dir)
		if err != nil || status.CurrentVersion != 42 {
			t.Fatalf("future status: %+v, %v", status, err)
		}
	})
}

func TestReadyRequiresLiveExactVersion(t *testing.T) {
	s := openTestStore(t, privateTempDir(t))
	if err := s.Ready(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Removing the newest applied row leaves the embedded version ahead of the
	// stored one, which is exactly the "not fully migrated" state Ready refuses.
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

// TestCanceledCallerNeverPoisonsTheStore is the advisor's Y4 probe, pinned with
// a seam instead of a delay: a foreign connection holds the SQLite write lock,
// so the store's BEGIN IMMEDIATE waits inside the driver with the single pooled
// connection checked out. The test cancels the caller exactly there - observed
// through ConnectionsInUse, not slept into - and then releases the lock. The
// caller's context is only consulted before the transaction begins, so the
// begin, every statement and the commit run under context.WithoutCancel: the
// canceled write must still be stored, and no connection may be left half-open.
// With the pre-fix cancellable BeginTx the transaction opens under an already
// canceled context, the commit (or the rollback that replaces it) fails, and
// every later write then waits out busy_timeout - which is exactly what the
// follow-up writes below refuse to tolerate.
func TestCanceledCallerNeverPoisonsTheStore(t *testing.T) {
	s, path := openLockedStore(t)
	source := createTestSource(t, s, "poison-source", "{{SECRET_poison_source}}")
	failed := PreflightRecord{OK: false, Checks: []string{}, Warnings: []PreflightWarning{}}
	stored := PreflightRecord{OK: true, Checks: []string{"identity"}, Warnings: []PreflightWarning{}}

	release := holdExternalWriteLock(t, path)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	result := make(chan error, 1)
	go func() {
		_, err := s.RecordSourcePreflight(ctx, source.ID, failed, SystemAudit("sources.preflight", AuditDetail{}))
		result <- err
	}()

	// The connection is checked out and the transaction's BEGIN IMMEDIATE is in
	// flight: the pool reports it in use, and the write cannot have completed.
	waitForConnection(t, s)
	cancel()
	// Let the cancellation be delivered while the begin is still waiting.
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
	write("audit", func(ctx context.Context) error {
		return s.Audit(ctx, SystemAudit("probe.audit", AuditDetail{}))
	})
	var updated Source
	write("preflight record", func(ctx context.Context) error {
		var err error
		updated, err = s.RecordSourcePreflight(ctx, source.ID, stored, SystemAudit("sources.preflight", AuditDetail{}))
		return err
	})
	if updated.LastPreflight == nil || !updated.LastPreflight.OK {
		t.Fatalf("preflight record = %+v, want the later observation to have replaced the canceled one",
			updated.LastPreflight)
	}
}

// waitForConnection requires the store's single pooled connection to be checked
// out inside a driver call, which is the deterministic signal that a write holds
// an open transaction.
func waitForConnection(t *testing.T, s *Store) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for s.ConnectionsInUse() != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("connections in use = %d, want the write to hold the single connection", s.ConnectionsInUse())
		}
		time.Sleep(5 * time.Millisecond)
	}
}
