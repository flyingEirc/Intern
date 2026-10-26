package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	physicalstore "github.com/flyingEirc/Intern/internal/physical/store"
)

func TestPhysicalAndWebShareDatabase(t *testing.T) {
	dir := privateTempDir(t)
	s := openTestStore(t, dir)
	assertSharedTables(t, s)
	beginPhysicalBackup(t, s)
	if err := physicalstore.Migrate(t.Context(), s.db); err != nil {
		t.Fatal(err)
	}
	// A separate read-only connection to intern.db sees both schemas and the
	// physical transaction; no second catalog file participates.
	db, err := openDatabase(t.Context(), filepath.Join(dir, "intern.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var instance, owner string
	if err = db.QueryRowContext(t.Context(), "SELECT i.id, b.instance_id FROM instance i CROSS JOIN backups b WHERE b.id = 'backup'").Scan(&instance, &owner); err != nil || instance != s.InstanceID() || owner != instance {
		t.Fatalf("same-file ownership: %q %q %v", instance, owner, err)
	}
	if _, err = os.Stat(filepath.Join(dir, "catalog.sqlite")); !os.IsNotExist(err) {
		t.Fatalf("unexpected CLI catalog: %v", err)
	}
	assertPhysicalStatus(t, dir, 1, 0)
	if err = s.Ready(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestPhysicalPendingMigrationAndFutureReadiness(t *testing.T) {
	for _, tc := range []struct {
		name    string
		query   string
		current int64
		pending int
	}{
		{"pending", "DELETE FROM physical_goose_db_version WHERE version_id = 1", 0, 1},
		{"missing table", "DROP TABLE physical_goose_db_version", 0, 1},
		{"future", "INSERT INTO physical_goose_db_version (version_id, is_applied) VALUES (42, 1)", 42, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := privateTempDir(t)
			s := openTestStore(t, dir)
			if _, err := s.db.ExecContext(t.Context(), tc.query); err != nil {
				t.Fatal(err)
			}
			if err := s.Ready(t.Context()); err == nil || !strings.Contains(err.Error(), "physical database version") {
				t.Fatalf("unready physical schema: %v", err)
			}
			assertPhysicalStatus(t, dir, tc.current, tc.pending)
			if tc.name == "future" {
				assertPhysicalDowngradeRejected(t, s, dir)
			}
		})
	}
}

func TestPhysicalStatusBeforeDatabaseCreation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not-created")
	status, err := ReadStatus(t.Context(), dir)
	if err != nil || status.PhysicalCurrentVersion != 0 || status.PhysicalLatestVersion != 1 || status.PhysicalPendingMigrations != 1 ||
		status.ReplicationCurrentVersion != 0 || status.ReplicationLatestVersion != 1 || status.ReplicationPending != 1 {
		t.Fatalf("uninitialized status: %+v %v", status, err)
	}
	if _, err = os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("status created metadata directory: %v", err)
	}
}

func assertSharedTables(t *testing.T, s *Store) {
	t.Helper()
	for _, table := range []string{"instance", "users", "sessions", "web_goose_db_version", "chains", "backups", "cleanup_ledger", "physical_goose_db_version", "replicas", "replica_members", "replica_parts", "replication_goose_db_version"} {
		present, err := tableExists(t.Context(), s.db, table)
		if err != nil || !present {
			t.Fatalf("same intern.db table %s: %v %v", table, present, err)
		}
	}
}

func beginPhysicalBackup(t *testing.T, s *Store) {
	t.Helper()
	physical := physicalstore.New(s.db)
	plan, err := physical.Plan(t.Context(), physicalstore.PlanRequest{
		SourceID: "source", ServerUUID: "server", ServerVersion: "8.4.11", ToolVersion: "8.4.0-7", Kind: "full", MaxChainLength: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = physical.Begin(t.Context(), physicalstore.BeginRequest{
		Plan: plan, SourceID: "source", ServerUUID: "server", ServerVersion: "8.4.11", ToolVersion: "8.4.0-7",
		InstanceID: s.InstanceID(), RunID: "run", BackupID: "backup", ObjectPath: "instance/source/chain/backup",
		StagingToken: "run", WorkDir: "/work/run-run", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
}

func assertPhysicalStatus(t *testing.T, dir string, current int64, pending int) {
	t.Helper()
	status, err := ReadStatus(t.Context(), dir)
	if err != nil || status.PhysicalCurrentVersion != current || status.PhysicalLatestVersion != 1 ||
		status.PhysicalPendingMigrations != pending || status.CurrentVersion != status.LatestVersion {
		t.Fatalf("physical status: %+v %v", status, err)
	}
}

// TestReplicationPendingMigrationAndFutureReadiness mirrors the physical cases
// for the replication migration set, so Ready and ReadStatus agree about it.
func TestReplicationPendingMigrationAndFutureReadiness(t *testing.T) {
	for _, tc := range []struct {
		name    string
		query   string
		current int64
		pending int
	}{
		{"pending", "DELETE FROM replication_goose_db_version WHERE version_id = 1", 0, 1},
		{"missing table", "DROP TABLE replication_goose_db_version", 0, 1},
		{"future", "INSERT INTO replication_goose_db_version (version_id, is_applied) VALUES (42, 1)", 42, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertReplicationSchemaState(t, tc.name, tc.query, tc.current, tc.pending)
		})
	}
}

func assertReplicationSchemaState(t *testing.T, name, query string, current int64, pending int) {
	t.Helper()
	dir := privateTempDir(t)
	s := openTestStore(t, dir)
	if _, err := s.db.ExecContext(t.Context(), query); err != nil {
		t.Fatal(err)
	}
	if err := s.Ready(t.Context()); err == nil || !strings.Contains(err.Error(), "replication database version") {
		t.Fatalf("unready replication schema: %v", err)
	}
	assertReplicationStatus(t, dir, current, pending)
	if name == "future" {
		assertReplicationDowngradeRejected(t, s, dir)
	}
}

func assertReplicationStatus(t *testing.T, dir string, current int64, pending int) {
	t.Helper()
	status, err := ReadStatus(t.Context(), dir)
	if err != nil || status.ReplicationCurrentVersion != current ||
		status.ReplicationLatestVersion != 1 || status.ReplicationPending != pending {
		t.Fatalf("replication status: %+v %v", status, err)
	}
}

func assertReplicationDowngradeRejected(t *testing.T, s *Store, dir string) {
	t.Helper()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(t.Context(), dir); err == nil || !strings.Contains(err.Error(), "replication database version 42") {
		t.Fatalf("replication downgrade accepted: %v", err)
	}
}

func assertPhysicalDowngradeRejected(t *testing.T, s *Store, dir string) {
	t.Helper()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(t.Context(), dir); err == nil || !strings.Contains(err.Error(), "physical database version 42") {
		t.Fatalf("physical downgrade accepted: %v", err)
	}
}
