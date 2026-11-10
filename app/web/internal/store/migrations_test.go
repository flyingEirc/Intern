package store

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/pressly/goose/v3"

	"github.com/flyingEirc/Intern/internal/pgtest"
)

// migrationSet returns the embedded migrations whose version is at most through,
// which is how a test starts a database at an older schema than the binary
// carries — the exact state a real upgrade begins from.
func migrationSet(t *testing.T, through int64) fs.FS {
	t.Helper()
	entries, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	set := fstest.MapFS{}
	for _, entry := range entries {
		name := filepath.Base(entry)
		prefix, _, _ := strings.Cut(name, "_")
		version, parseErr := strconv.ParseInt(prefix, 10, 64)
		if parseErr != nil {
			t.Fatalf("migration %q has no numeric prefix", name)
		}
		if version > through {
			continue
		}
		data, readErr := migrations.ReadFile(entry)
		if readErr != nil {
			t.Fatal(readErr)
		}
		set[name] = &fstest.MapFile{Data: data}
	}
	if len(set) == 0 {
		t.Fatalf("no migrations at or below version %d", through)
	}
	return set
}

func applyMigrations(t *testing.T, db *sql.DB, fsys fs.FS) int64 {
	t.Helper()
	provider, err := goose.NewProvider(goose.DialectPostgres, db, fsys, providerOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	applied, err := provider.Up(context.Background())
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var current int64
	if err = db.QueryRow(`SELECT MAX(version_id) FROM web_goose_db_version WHERE is_applied`).Scan(&current); err != nil {
		t.Fatal(err)
	}
	t.Logf("applied %d migrations, now at version %d", len(applied), current)
	return current
}

// TestFTPSMigrationUpgradesExistingRows is the S2 schema gate: a database that
// already holds plain repositories and policies keeps every row and every
// constraint through the rebuild, and only then accepts the FTPS modes.
func TestFTPSMigrationUpgradesExistingRows(t *testing.T) {
	db := seedPreFTPSDatabase(t)
	if got := applyMigrations(t, db, migrationSet(t, latestEmbedded(t))); got != latestEmbedded(t) {
		t.Fatalf("upgraded version = %d", got)
	}
	requirePlainRowPreserved(t, db)
	requireForeignKeysIntact(t, db)
	requireFTPSConstraints(t, db)
}

// TestFTPSMigrationDownIsAtomic pins both downgrade outcomes. With an FTPS row
// present the plain-only shape cannot hold it, so the downgrade must fail and
// leave the database exactly as it was — the original table with every row, no
// half-built copy, still at the FTPS version — so a retry starts from the same
// state. Without one it must restore the plain-only shape and keep the rows.
func TestFTPSMigrationDownIsAtomic(t *testing.T) {
	dsn := pgtest.New(t)
	db := openMigrationDB(t, dsn)
	const latest int64 = 9
	applyMigrations(t, db, migrationSet(t, latest))
	for _, statement := range []string{
		ftpRepositoryInsert("r-plain", "plain-one", "plain"),
		ftpRepositoryInsert("r-ftps", "ftps-one", "explicit"),
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrationSet(t, latest), providerOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = provider.Down(context.Background()); err == nil {
		t.Fatal("the downgrade dropped the FTPS repository instead of refusing")
	}
	// A new connection sees only committed state: whatever the failed statement
	// left open on the old one is gone with it.
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openMigrationDB(t, dsn)
	requireRepositoryState(t, db, latest, 2)

	if _, err = db.Exec(`DELETE FROM repositories WHERE id='r-ftps'`); err != nil {
		t.Fatal(err)
	}
	provider, err = goose.NewProvider(goose.DialectPostgres, db, migrationSet(t, latest), providerOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = provider.Down(context.Background()); err != nil {
		t.Fatalf("the downgrade of a plain-only database failed: %v", err)
	}
	requireRepositoryState(t, db, latest-1, 1)
	if _, err = db.Exec(ftpRepositoryInsert("r-again", "again", "explicit")); !sqliteRejects(err) {
		t.Fatalf("the downgraded schema accepted FTPS: %v", err)
	}
}

func openMigrationDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	return db
}

// requireRepositoryState checks the schema version, that no rebuild table was
// left behind, and how many repository rows survived.
func requireRepositoryState(t *testing.T, db *sql.DB, version int64, rows int) {
	t.Helper()
	var current int64
	if err := db.QueryRow(`SELECT MAX(version_id) FROM web_goose_db_version WHERE is_applied AND id IN (SELECT MAX(id) FROM web_goose_db_version GROUP BY version_id)`).Scan(&current); err != nil {
		t.Fatal(err)
	}
	if current != version {
		t.Fatalf("schema version = %d, want %d", current, version)
	}
	var leftovers int
	if err := db.QueryRow(`SELECT count(*) FROM information_schema.tables WHERE table_schema = current_schema() AND table_name IN ('repositories_ftps','repositories_plain')`).
		Scan(&leftovers); err != nil {
		t.Fatal(err)
	}
	if leftovers != 0 {
		t.Fatalf("%d half-built rebuild tables were left behind", leftovers)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM repositories`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != rows {
		t.Fatalf("repositories rows = %d, want %d", count, rows)
	}
}

// seedPreFTPSDatabase opens a database at schema 8 holding one plain
// repository, a source and a policy that references the repository.
func seedPreFTPSDatabase(t *testing.T) *sql.DB {
	t.Helper()
	db := openMigrationDB(t, pgtest.New(t))
	if got := applyMigrations(t, db, migrationSet(t, 8)); got != 8 {
		t.Fatalf("baseline version = %d, want 8", got)
	}
	// The plain-only shape must refuse FTPS before the upgrade, which is what
	// makes this database a real pre-S2 state instead of an empty file.
	if _, err := db.Exec(ftpRepositoryInsert("r-early", "early", "explicit")); !sqliteRejects(err) {
		t.Fatalf("the old schema accepted FTPS: %v", err)
	}
	for _, statement := range []string{
		ftpRepositoryInsert("r-plain", "plain-one", "plain"),
		`INSERT INTO sources (id,name,socket,datadir,username,secret_ciphertext,created_at,updated_at)
		 VALUES ('s-1','source','/run/mysqld/mysqld.sock','/var/lib/mysql','backup',decode('736563726574','hex'),'now','then')`,
		`INSERT INTO policies (id,name,source_id,local_repository_id,replica_repository_id,full_cron,incremental_cron,
			timezone,max_chain_length,parallel,throttle,zstd_level,page_tracking,enabled,created_at,updated_at)
		 VALUES ('p-1','policy','s-1','r-plain','r-plain','0 2 * * *','0 * * * *','UTC',3,1,0,3,0,0,'now','then')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	return db
}

// requirePlainRowPreserved checks the seeded plain repository survived the
// table rebuild unchanged, including its sealed secret.
func requirePlainRowPreserved(t *testing.T, db *sql.DB) {
	t.Helper()
	var name, mode, config, createdAt string
	if err := db.QueryRow(`SELECT name, tls_mode, config_json, created_at FROM repositories WHERE id='r-plain'`).
		Scan(&name, &mode, &config, &createdAt); err != nil {
		t.Fatal(err)
	}
	if name != "plain-one" || mode != "plain" || createdAt != "now" ||
		config != `{"host":"ftp.example","port":21,"base_path":"/backups","username":"u"}` {
		t.Fatalf("row changed across the rebuild: %q %q %q %q", name, mode, config, createdAt)
	}
	// The sealed secret is copied byte for byte: a rotated ciphertext would make
	// every stored credential unreadable.
	var secret []byte
	if err := db.QueryRow(`SELECT secret_ciphertext FROM repositories WHERE id='r-plain'`).Scan(&secret); err != nil {
		t.Fatal(err)
	}
	if string(secret) != "secret" {
		t.Fatalf("sealed secret = %q", secret)
	}
}

// requireForeignKeysIntact checks the rebuild left foreign keys enforced,
// violation-free and switched on.
func requireForeignKeysIntact(t *testing.T, db *sql.DB) {
	t.Helper()
	// Foreign keys are still enforced, which only holds if the rename left the
	// policies REFERENCES clause pointing at this table.
	if _, err := db.Exec(`INSERT INTO policies (id,name,source_id,local_repository_id,full_cron,incremental_cron,
		timezone,max_chain_length,parallel,throttle,zstd_level,page_tracking,enabled,created_at,updated_at)
		VALUES ('p-bad','bad','s-1','r-missing','0 2 * * *','0 * * * *','UTC',3,1,0,3,0,0,'now','then')`); !sqliteRejects(err) {
		t.Fatalf("a dangling repository reference was accepted: %v", err)
	}
	var policies int
	if err := db.QueryRow(`SELECT COUNT(*) FROM policies p JOIN repositories r ON r.id = p.local_repository_id`).Scan(&policies); err != nil || policies != 1 {
		t.Fatalf("preserved policy references = %d, %v", policies, err)
	}
}

// requireFTPSConstraints checks the relaxed schema now accepts both FTPS modes
// and still refuses the combinations that would weaken a repository.
func requireFTPSConstraints(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, mode := range []string{"explicit", "implicit"} {
		if _, err := db.Exec(ftpRepositoryInsert("r-"+mode, mode, mode)); err != nil {
			t.Fatalf("FTPS mode %s refused: %v", mode, err)
		}
	}
	if _, err := db.Exec(noSecretFTPRepositoryInsert("r-ftps-bare", "bare", "explicit")); !sqliteRejects(err) {
		t.Fatalf("a secret-less FTPS repository was accepted: %v", err)
	}
	if _, err := db.Exec(localRepositoryInsert("r-local-explicit", "local", "explicit")); !sqliteRejects(err) {
		t.Fatalf("a local repository with an FTPS mode was accepted: %v", err)
	}
	if _, err := db.Exec(`UPDATE repositories SET tls_mode='starttls' WHERE id='r-plain'`); !sqliteRejects(err) {
		t.Fatalf("an unknown tls_mode was accepted after the upgrade: %v", err)
	}
}

func latestEmbedded(t *testing.T) int64 {
	t.Helper()
	version, err := LatestVersion()
	if err != nil {
		t.Fatal(err)
	}
	return version
}

func ftpRepositoryInsert(id, name, mode string) string {
	return `INSERT INTO repositories (id,name,kind,config_json,secret_ciphertext,tls_mode,created_at,updated_at) VALUES (
		'` + id + `','` + name + `','ftp',
		'{"host":"ftp.example","port":21,"base_path":"/backups","username":"u"}',
		decode('736563726574','hex'),'` + mode + `','now','then')`
}

func noSecretFTPRepositoryInsert(id, name, mode string) string {
	return `INSERT INTO repositories (id,name,kind,config_json,secret_ciphertext,tls_mode,created_at,updated_at) VALUES (
		'` + id + `','` + name + `','ftp',
		'{"host":"ftp.example","port":990,"base_path":"/backups","username":"u"}',
		NULL,'` + mode + `','now','then')`
}

func localRepositoryInsert(id, name, mode string) string {
	return `INSERT INTO repositories (id,name,kind,config_json,secret_ciphertext,tls_mode,created_at,updated_at) VALUES (
		'` + id + `','` + name + `','local','{"root":"/repository"}',NULL,'` + mode + `','now','then')`
}

func sqliteRejects(err error) bool {
	return err != nil && !errors.Is(err, ErrConflict)
}
