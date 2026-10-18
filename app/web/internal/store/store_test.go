package store

import (
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
	if err := os.Chmod(dir, 0700); err != nil {
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
	for path, want := range map[string]os.FileMode{dir: 0700, filepath.Join(dir, "intern.db"): 0600, filepath.Join(dir, ".intern-web.lock"): 0600} {
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
	if err != nil || status.InstanceID != s.InstanceID() || status.CurrentVersion != 3 || status.LatestVersion != 3 || status.PendingMigrations != 0 {
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
		{"wide directory", func(t *testing.T, dir string) string { os.Chmod(dir, 0755); return dir }},
		{"symlink directory", func(t *testing.T, dir string) string {
			link := filepath.Join(t.TempDir(), "link")
			if err := os.Symlink(dir, link); err != nil {
				t.Fatal(err)
			}
			return link
		}},
		{"wide database", func(t *testing.T, dir string) string {
			os.WriteFile(filepath.Join(dir, "intern.db"), nil, 0644)
			return dir
		}},
		{"database directory", func(t *testing.T, dir string) string { os.Mkdir(filepath.Join(dir, "intern.db"), 0700); return dir }},
		{"symlink database", func(t *testing.T, dir string) string {
			target := filepath.Join(t.TempDir(), "other.db")
			os.WriteFile(target, nil, 0600)
			os.Symlink(target, filepath.Join(dir, "intern.db"))
			return dir
		}},
		{"symlink lock", func(t *testing.T, dir string) string {
			target := filepath.Join(t.TempDir(), "other.lock")
			os.WriteFile(target, nil, 0600)
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
		os.WriteFile(filepath.Join(dir, "intern.db"), []byte("not a database"), 0600)
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
		if _, err := Open(t.Context(), dir); err == nil || !strings.Contains(err.Error(), "42") || !strings.Contains(err.Error(), "version 3") {
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
	if _, err := s.db.ExecContext(t.Context(), "DELETE FROM web_goose_db_version WHERE version_id = 3"); err != nil {
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
