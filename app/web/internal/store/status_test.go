package store

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestStatusDoesNotCreateMetadata(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "metadata")
	status, err := ReadStatus(t.Context(), dir)
	assertStatus(t, status, err, unmigratedStatus(t))
	if _, err = os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("status created directory: %v", err)
	}
	if err = os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	status, err = ReadStatus(t.Context(), dir)
	assertStatus(t, status, err, unmigratedStatus(t))
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("status created files: %v, %v", entries, err)
	}
}

func TestStatusDoesNotMigrate(t *testing.T) {
	dir := privateTempDir(t)
	path := filepath.Join(dir, "intern.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	status, err := ReadStatus(t.Context(), dir)
	assertStatus(t, status, err, unmigratedStatus(t))
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("status changed database: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "intern.db" {
		t.Fatalf("status created lock or side files: %v, %v", entries, err)
	}
}

func TestStatusDatabaseIsReadOnly(t *testing.T) {
	dir := privateTempDir(t)
	s := openTestStore(t, dir)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := openDatabase(t.Context(), filepath.Join(dir, "intern.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.ExecContext(t.Context(), "DELETE FROM instance"); err == nil {
		t.Fatal("read-only database accepted a write")
	}
}

// unmigratedStatus is the status of a metadata directory with no applied
// migration set: every embedded latest, nothing applied. Both latests come from
// their embedded sets, so a new web or replication migration does not leave this
// expectation stale.
func unmigratedStatus(t *testing.T) Status {
	t.Helper()
	latest := replicationLatest(t)
	web := webLatest(t)
	return Status{
		LatestVersion: web, PendingMigrations: int(web),
		PhysicalLatestVersion: 1, PhysicalPendingMigrations: 1,
		ReplicationLatestVersion: latest, ReplicationPending: int(latest),
	}
}

// webLatest is the newest embedded web migration version.
func webLatest(t *testing.T) int64 {
	t.Helper()
	versions, err := embeddedVersions()
	if err != nil {
		t.Fatal(err)
	}
	return latestVersion(versions)
}

func assertStatus(t *testing.T, got Status, err error, want Status) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("status = %+v; want %+v", got, want)
	}
}
