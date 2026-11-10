package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/flyingEirc/Intern/internal/pgtest"
	physicalstore "github.com/flyingEirc/Intern/internal/physical/store"
)

func legacyDatabase(t *testing.T) (*sql.DB, string) {
	t.Helper()
	name := filepath.Join(t.TempDir(), "intern.db")
	if err := os.Chmod(filepath.Dir(name), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", name)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		closeErr := db.Close()
		if closeErr != nil {
			t.Errorf("close legacy SQLite fixture: %v", closeErr)
		}
	})
	for _, schema := range []string{"web", "physical", "replication"} {
		provider, providerErr := goose.NewProvider(goose.DialectSQLite3, db, os.DirFS("testdata/sqlite/"+schema),
			goose.WithDisableGlobalRegistry(true), goose.WithTableName(schema+"_goose_db_version"))
		if providerErr != nil {
			t.Fatal(providerErr)
		}
		if _, err = provider.Up(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.Chmod(name, 0o600); err != nil {
		t.Fatal(err)
	}
	return db, name
}

func seedLegacyDatabase(t *testing.T, db *sql.DB) SecretSealer {
	t.Helper()
	sealer := testSealer(t)
	seedLegacyIdentity(t, db)
	seedLegacyResources(t, db, sealer)
	seedLegacyRuns(t, db)
	seedLegacyHistory(t, db)
	return sealer
}

func seedLegacyIdentity(t *testing.T, db *sql.DB) {
	t.Helper()
	seedLegacyStatements(t, db, []string{
		`INSERT INTO instance VALUES (1, '019a7000-0000-7000-8000-000000000001', '2026-11-01T00:00:00Z')`,
		`INSERT INTO users VALUES ('admin', 'legacy-admin', 'legacy-hash', 'admin', 0, 5, 'now', 'now')`,
		`INSERT INTO sessions VALUES ('legacy-token', X'6c65676163792d73657373696f6e', 2000000000)`,
	})
}

func seedLegacyResources(t *testing.T, db *sql.DB, sealer SecretSealer) {
	t.Helper()
	sealed, err := sealer.Seal("sources", "source", "secret_ciphertext", []byte("legacy-source-secret"))
	if err != nil {
		t.Fatal(err)
	}
	seedLegacyStatements(t, db, []string{
		`INSERT INTO repositories VALUES ('local', 'local', 'local', '{"root":"/backups"}', NULL, 'none', 'now', 'now')`,
	})
	if _, err = db.ExecContext(t.Context(), `INSERT INTO sources VALUES ('source', 'source', '/run/mysql.sock', '/var/lib/mysql', 'backup', ?, 'server', '8.4.11', NULL, 'now', 'now')`, sealed); err != nil {
		t.Fatal(err)
	}
	seedLegacyStatements(t, db, []string{
		`INSERT INTO policies (id,name,source_id,local_repository_id,full_cron,incremental_cron,timezone,max_chain_length,parallel,throttle,zstd_level,created_at,updated_at)
VALUES ('policy','policy','source','local','0 2 * * *','0 * * * *','UTC',7,2,100,6,'now','now')`,
	})
}

func seedLegacyRuns(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, status := range []string{"completed", "running", "queued", "failed", "canceled"} {
		if _, err := db.ExecContext(t.Context(), `INSERT INTO runs (id,policy_id,source_id,kind,"trigger",idempotency_key,request_hash,status,scheduled_at,created_at)
VALUES (?, 'policy', 'source', 'full', 'manual', ?, 'hash', ?, 'now', 'now')`, "run-"+status, "key-"+status, status); err != nil {
			t.Fatal(err)
		}
	}
}

func seedLegacyHistory(t *testing.T, db *sql.DB) {
	t.Helper()
	seedLegacyStatements(t, db, []string{
		`INSERT INTO chains VALUES ('chain', 'source', 'server', '8.4.11', '8.4.0-7', '2026-11-01T00:00:00.000000000Z', NULL, 1)`,
		`INSERT INTO backups (id, chain_id, seq, kind, status, instance_id, run_id, from_lsn, to_lsn, last_lsn, checkpoints, object_path, key_id, local_present, created_at, completed_at)
VALUES ('full', 'chain', 1, 'physical-full', 'completed', '019a7000-0000-7000-8000-000000000001', 'run-full', '0', '9223372036854775900', '9223372036854775901', X'636865636b706f696e7473', 'instance/source/chain/full', 'key', 1, '2026-11-01T00:00:00.000000000Z', '2026-11-01T00:00:01.000000000Z')`,
		`INSERT INTO backups (id, chain_id, seq, parent_id, kind, status, instance_id, run_id, from_lsn, to_lsn, last_lsn, object_path, key_id, created_at)
VALUES ('incremental', 'chain', 2, 'full', 'physical-incremental', 'running', '019a7000-0000-7000-8000-000000000001', 'run-inc', '9223372036854775900', NULL, NULL, 'instance/source/chain/inc', 'key', '2026-11-01T00:00:02.000000000Z')`,
		`INSERT INTO cleanup_ledger VALUES (90, 'incremental', '019a7000-0000-7000-8000-000000000001', 'run-inc', 'workdir', '/work/run-inc', 2, 'cleanup_failed', 'now')`,
		`INSERT INTO audit_logs VALUES (120, 'now', 'system', '', '', 'users.create_admin', 'users', 'admin', '', 'success', NULL, '', '{}')`,
		`INSERT INTO session_tombstones VALUES ('revoked-token', 2000000000)`,
		`INSERT INTO run_logs VALUES ('run-completed', 1, 'now', 'info', 'completed', 'legacy completed', '{"bytes":512}')`,
		`INSERT INTO restores (id,backup_id,status,plan_json,plan_digest,requested_by,created_at,side_effects_json) VALUES ('restore', 'full', 'failed', '{}', 'digest', 'admin', 'now', '{"export_published":false}')`,
		`INSERT INTO restore_logs VALUES ('restore', 1, 'now', 'error', 'legacy interrupted', '{"phase":"preparing"}')`,
		`INSERT INTO replicas (backup_id,repository_id,idem_key,status,next_attempt_at,created_at,updated_at,local_repository_id) VALUES ('full', 'remote', 'replicate-full', 'verified', 'now', 'now', 'now', 'local')`,
		`INSERT INTO replica_members VALUES ('full', 'remote', 'payload.bin', 'verified', 'digest', 512, 'now')`,
		`INSERT INTO replica_parts VALUES (180, 'full', 'remote', 'metadata.bin', '.intern-upload-legacy.part', 2, 'remote_transport', 'now')`,
	})
}

func seedLegacyStatements(t *testing.T, db *sql.DB, statements []string) {
	t.Helper()
	for _, statement := range statements {
		if _, err := db.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
}

func TestImportSQLitePreservesIdentitySecretsAndHistory(t *testing.T) {
	legacy, source := legacyDatabase(t)
	sealer := seedLegacyDatabase(t, legacy)
	dsn := pgtest.New(t)
	result, err := ImportSQLite(t.Context(), dsn, source)
	if err != nil {
		t.Fatal(err)
	}
	s := openTestStore(t, dsn)
	requireImportedCounts(t, legacy, s, result)
	requireImportedIdentity(t, s, result)
	requireImportedCredential(t, s, sealer)
	requireImportedRuns(t, s)
	requireImportedAuditSequence(t, s)
	requireImportedSetupCompleted(t, s)
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = ImportSQLite(t.Context(), dsn, source); err == nil || !strings.Contains(err.Error(), "already contains") {
		t.Fatalf("nonempty import = %v", err)
	}
}

func requireImportedCounts(t *testing.T, legacy *sql.DB, s *Store, result ImportResult) {
	t.Helper()
	if result.InstanceID != "019a7000-0000-7000-8000-000000000001" || result.Rows["backups"] != 2 || result.Rows["runs"] != 5 {
		t.Fatalf("import counts/identity = %+v", result)
	}
	for _, table := range legacyTables {
		var sourceCount, destinationCount int64
		if err := legacy.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&sourceCount); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&destinationCount); err != nil {
			t.Fatal(err)
		}
		if sourceCount == 0 || result.Rows[table] != sourceCount || destinationCount != sourceCount {
			t.Fatalf("table %s imported %d, source %d, destination %d", table, result.Rows[table], sourceCount, destinationCount)
		}
	}
}

func requireImportedIdentity(t *testing.T, s *Store, result ImportResult) {
	t.Helper()
	if s.InstanceID() != result.InstanceID {
		t.Fatal("imported identity changed")
	}
}

func requireImportedCredential(t *testing.T, s *Store, sealer SecretSealer) {
	t.Helper()
	secret, err := s.SourceSecret(t.Context(), "source", sealer)
	if err != nil || !bytes.Equal(secret, []byte("legacy-source-secret")) {
		t.Fatalf("imported credential AAD = %v", err)
	}
	if _, err = s.SourceSecret(t.Context(), "source", testSealer(t)); err == nil {
		t.Fatal("wrong KEK opened imported secret")
	}
}

func requireImportedRuns(t *testing.T, s *Store) {
	t.Helper()
	data, found, err := s.Sessions().Find("legacy-token")
	if err != nil || !found || string(data) != "legacy-session" {
		t.Fatalf("imported session = %v, %v", found, err)
	}
	backup, err := physicalstore.New(s.db).Backup(t.Context(), "full")
	if err != nil || backup.ToLSN != uint64(9223372036854775900) || backup.Status != "completed" {
		t.Fatalf("imported unsigned LSN = %+v, %v", backup, err)
	}
	for _, status := range []string{"completed", "running", "queued", "failed", "canceled"} {
		run, err := s.Run(t.Context(), "run-"+status)
		if err != nil || run.Status != status {
			t.Fatalf("imported %s run = %+v, %v", status, run, err)
		}
	}
}

func requireImportedSetupCompleted(t *testing.T, s *Store) {
	t.Helper()
	var completed bool
	err := s.db.QueryRowContext(t.Context(), "SELECT completed FROM setup_state WHERE singleton = 1").Scan(&completed)
	if err != nil || !completed {
		t.Fatalf("setup_state.completed = %t, %v", completed, err)
	}
}

func requireImportedAuditSequence(t *testing.T, s *Store) {
	t.Helper()
	if err := s.Audit(t.Context(), SystemAudit("after.import", AuditDetail{})); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := s.db.QueryRowContext(t.Context(), "SELECT id FROM audit_logs WHERE action='after.import'").Scan(&id); err != nil || id <= 120 {
		t.Fatalf("audit sequence after import = %d, %v", id, err)
	}
}

func TestImportSQLiteFailureRollsBackEveryRow(t *testing.T) {
	legacy, source := legacyDatabase(t)
	seedLegacyDatabase(t, legacy)
	// SQLite permits embedded NUL text; PostgreSQL refuses it after earlier
	// tables have already been copied, exercising the real transaction rollback.
	if _, err := legacy.ExecContext(t.Context(), "UPDATE runs SET request_hash = ? WHERE id='run-running'", "invalid\x00text"); err != nil {
		t.Fatal(err)
	}
	dsn := pgtest.New(t)
	initial := openTestStore(t, dsn)
	identity := initial.InstanceID()
	if err := initial.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportSQLite(t.Context(), dsn, source); err == nil {
		t.Fatal("invalid legacy row imported")
	}
	s := openTestStore(t, dsn)
	if s.InstanceID() != identity {
		t.Fatal("failed import changed destination identity")
	}
	for _, table := range legacyTables[1:] {
		var count int
		if err := s.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("rollback table %s = %d, %v", table, count, err)
		}
	}
}

func TestImportSQLiteRejectsWrongVersionAndCorruption(t *testing.T) {
	legacy, source := legacyDatabase(t)
	if _, err := legacy.ExecContext(t.Context(), "UPDATE web_goose_db_version SET version_id=42 WHERE version_id=9"); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportSQLite(t.Context(), pgtest.New(t), source); err == nil || !strings.Contains(err.Error(), "version 42") {
		t.Fatalf("wrong version import = %v", err)
	}
	corrupt := filepath.Join(t.TempDir(), "intern.db")
	if err := os.WriteFile(corrupt, []byte("corrupt SQLite file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportSQLite(t.Context(), pgtest.New(t), corrupt); err == nil {
		t.Fatal("corrupt database imported")
	}
}

func setImportAfterRow(t *testing.T, hook func(context.Context, *sql.Tx, string, int64) error) {
	t.Helper()
	importHookMu.Lock()
	importAfterRow = hook
	importHookMu.Unlock()
	t.Cleanup(func() {
		importHookMu.Lock()
		importAfterRow = nil
		importHookMu.Unlock()
	})
}

func TestImportSurvivesStatementAndTransactionWindows(t *testing.T) {
	legacy, source := legacyDatabase(t)
	seedLegacyDatabase(t, legacy)
	sourceUsers := legacyUserCount(t, legacy)
	holdImportRow(t, "instance", 1, sleepPastInteractiveLimits)
	dsn := pgtest.New(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	start := time.Now()
	result, err := ImportSQLite(ctx, dsn, source)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 30*time.Second {
		t.Fatalf("import finished in %s", time.Since(start))
	}
	s := openTestStore(t, dsn)
	requireImportedCounts(t, legacy, s, result)
	requireImportedIdentity(t, s, result)
	requireStatementTimeout(t, s, "5s")
	requireLegacyUserCount(t, legacy, sourceUsers)
}

func TestImportCancelRollsBackWithoutReleasingOwner(t *testing.T) {
	legacy, source := legacyDatabase(t)
	seedLegacyDatabase(t, legacy)
	sourceUsers := legacyUserCount(t, legacy)
	dsn := pgtest.New(t)
	initial := openTestStore(t, dsn)
	identity := initial.InstanceID()
	if err := initial.Close(); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	holdImportRow(t, "users", 1, func(ctx context.Context, tx *sql.Tx) error {
		close(started)
		_, err := tx.ExecContext(ctx, "SELECT pg_sleep(31)")
		return err
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := ImportSQLite(ctx, dsn, source)
		done <- err
	}()
	waitForHeldImport(t, started, done)
	requireOwnerStillHeld(t, dsn)
	cancel()
	requireImportCanceled(t, done)
	requireImportRolledBack(t, dsn, identity)
	requireLegacyUserCount(t, legacy, sourceUsers)
}

func legacyUserCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var count int
	if err := db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM users").Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func requireLegacyUserCount(t *testing.T, db *sql.DB, want int) {
	t.Helper()
	if got := legacyUserCount(t, db); got != want {
		t.Fatalf("source users = %d, was %d", got, want)
	}
}

func holdImportRow(t *testing.T, table string, row int64, fn func(context.Context, *sql.Tx) error) {
	t.Helper()
	setImportAfterRow(t, func(ctx context.Context, tx *sql.Tx, got string, n int64) error {
		if got != table || n != row {
			return nil
		}
		return fn(ctx, tx)
	})
}

func sleepPastInteractiveLimits(ctx context.Context, tx *sql.Tx) error {
	var timeout string
	if err := tx.QueryRowContext(ctx, "SELECT current_setting('statement_timeout')").Scan(&timeout); err != nil {
		return err
	}
	if timeout != "0" && timeout != "0ms" {
		return fmt.Errorf("import statement_timeout = %s", timeout)
	}
	_, err := tx.ExecContext(ctx, "SELECT pg_sleep(31)")
	return err
}

func requireStatementTimeout(t *testing.T, s *Store, want string) {
	t.Helper()
	var got string
	if err := s.db.QueryRowContext(t.Context(), "SELECT current_setting('statement_timeout')").Scan(&got); err != nil || got != want {
		t.Fatalf("statement_timeout = %q, %v; want %q", got, err, want)
	}
}

func waitForHeldImport(t *testing.T, started <-chan struct{}, done <-chan error) {
	t.Helper()
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("import ended before the held statement: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("import did not reach the held statement")
	}
}

func requireOwnerStillHeld(t *testing.T, dsn string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := Open(ctx, dsn); !errors.Is(err, ErrOwnerInUse) {
		t.Fatalf("import released ownership: %v", err)
	}
}

func requireImportCanceled(t *testing.T, done <-chan error) {
	t.Helper()
	canceledAt := time.Now()
	select {
	case err := <-done:
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled import = %v", err)
		}
		if elapsed := time.Since(canceledAt); elapsed > 5*time.Second {
			t.Fatalf("cancel took %s", elapsed)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("canceled import did not return")
	}
}

func TestImportSQLiteRejectsOccupiedTargetWithoutWriting(t *testing.T) {
	_, source := legacyDatabase(t)
	dsn := pgtest.New(t)
	occupyDestinationWithoutInstance(t, dsn)
	before := snapshotControlPlane(t, dsn)
	requireEmptyInstanceAndGoose(t, before)
	_, err := ImportSQLite(t.Context(), dsn, source)
	t.Run("refuses occupied destination", func(t *testing.T) {
		requireOccupiedRefusal(t, err, source)
	})
	after := snapshotControlPlane(t, dsn)
	t.Run("goose version tables unchanged", func(t *testing.T) {
		requireGooseUnchanged(t, before, after)
	})
	t.Run("instance rows unchanged", func(t *testing.T) {
		requireInstanceUnchanged(t, before, after)
	})
}

func requireEmptyInstanceAndGoose(t *testing.T, state controlPlaneState) {
	t.Helper()
	if state.instance != "empty" || state.web == "absent" || state.web == "empty" {
		t.Fatalf("precondition goose=%q instance=%q", state.web, state.instance)
	}
}

func requireOccupiedRefusal(t *testing.T, err error, source string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "already contains") {
		t.Fatalf("occupied import = %v", err)
	}
	if _, statErr := os.Stat(source); statErr != nil {
		t.Fatalf("legacy SQLite removed: %v", statErr)
	}
}

func requireGooseUnchanged(t *testing.T, before, after controlPlaneState) {
	t.Helper()
	if before.web != after.web || before.physical != after.physical || before.replication != after.replication {
		t.Fatalf("goose version tables changed\nbefore web=%q physical=%q replication=%q\nafter web=%q physical=%q replication=%q",
			before.web, before.physical, before.replication, after.web, after.physical, after.replication)
	}
}

func requireInstanceUnchanged(t *testing.T, before, after controlPlaneState) {
	t.Helper()
	if before.instance != after.instance {
		t.Fatalf("instance rows changed: %q -> %q", before.instance, after.instance)
	}
}

func occupyDestinationWithoutInstance(t *testing.T, dsn string) {
	t.Helper()
	s, err := Open(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = s.db.ExecContext(t.Context(), `INSERT INTO users (id, username, password_hash, role, disabled, session_epoch, created_at, updated_at)
VALUES ('occupied-user', 'occupied', 'hash', 'viewer', 0, 0, 'now', 'now')`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(t.Context(), "DELETE FROM instance"); err != nil {
		t.Fatal(err)
	}
}

type controlPlaneState struct {
	web, physical, replication, instance string
}

func snapshotControlPlane(t *testing.T, dsn string) controlPlaneState {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	return controlPlaneState{
		web:         relationDump(t, db, "web_goose_db_version", "SELECT version_id::text || ' ' || is_applied::text FROM web_goose_db_version ORDER BY version_id, id"),
		physical:    relationDump(t, db, "physical_goose_db_version", "SELECT version_id::text || ' ' || is_applied::text FROM physical_goose_db_version ORDER BY version_id, id"),
		replication: relationDump(t, db, "replication_goose_db_version", "SELECT version_id::text || ' ' || is_applied::text FROM replication_goose_db_version ORDER BY version_id, id"),
		instance:    relationDump(t, db, "instance", "SELECT id || ' ' || created_at FROM instance ORDER BY singleton"),
	}
}

func relationDump(t *testing.T, db *sql.DB, table, query string) string {
	t.Helper()
	var exists bool
	if err := db.QueryRowContext(t.Context(), "SELECT to_regclass($1) IS NOT NULL", table).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		return "absent"
	}
	rows, err := db.QueryContext(t.Context(), query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	lines := make([]string, 0)
	for rows.Next() {
		var line string
		if err = rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, line)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(lines) == 0 {
		return "empty"
	}
	return strings.Join(lines, "\n")
}

func requireImportRolledBack(t *testing.T, dsn, identity string) {
	t.Helper()
	s := openTestStore(t, dsn)
	if s.InstanceID() != identity {
		t.Fatalf("canceled import changed identity %s -> %s", identity, s.InstanceID())
	}
	for _, table := range legacyTables[1:] {
		var count int
		if err := s.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("rollback table %s = %d, %v", table, count, err)
		}
	}
}
