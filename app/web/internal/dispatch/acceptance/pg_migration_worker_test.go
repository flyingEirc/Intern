//go:build integration

package acceptance

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	aesgcm "github.com/tink-crypto/tink-go/v2/proto/aes_gcm_go_proto"
	"google.golang.org/protobuf/proto"

	"github.com/flyingEirc/Intern/app/web/internal/store"
	"github.com/flyingEirc/Intern/internal/pgtest"
	"github.com/flyingEirc/Intern/internal/storage/local"
)

const migrationLegacySource = "06c34299f03becdaf0fdcdffff5fbd4319bedafb"

var migrationTables = []string{
	"instance", "users", "sessions", "session_tombstones", "audit_logs", "repositories", "sources", "policies",
	"runs", "run_logs", "restores", "restore_logs", "chains", "backups", "cleanup_ledger", "replicas", "replica_members", "replica_parts",
}

// pgMigrationScenario runs after the locked W5 cases. No PG row is used to
// construct the source: the pinned, retired production program owns SQLite and
// creates the real full/incremental payloads, replicas, users and restore history.
func pgMigrationScenario(t *testing.T) {
	t.Helper()
	t.Setenv("INTERN_W4_ROOT", filepath.Join(os.Getenv("INTERN_W4_ROOT"), "migration"))
	t.Setenv("INTERN_W5_EXPORT_ROOT", filepath.Join(os.Getenv("INTERN_W5_EXPORT_ROOT"), "migration"))
	f := newFixture(t)
	f.shutdownInProcess(t)
	f.closeAnalysis(t)
	f.dsn = pgtest.New(t)
	w5 = &w5State{}
	source, historical, cleanup, before := migrationLegacyPlane(t, f)
	migrationImportPlane(t, f, source, before)
	migrationConsumerContinuity(t, f, source, historical, cleanup, before)
}

func migrationLegacyPlane(t *testing.T, f *fixture) (string, string, string, string) {
	t.Helper()
	migrationCommand(t, f, "/tmp/legacy-web", "", true, "migrate", "up")
	migrationCommand(t, f, "/tmp/legacy-web", f.secret.admin+"\n", true,
		"user", "create-admin", "--username", "admin", "--password-stdin")
	migrationServe(t, f, "/tmp/legacy-web", false)
	f.admin.login(t, "admin", f.secret.admin)
	f.admin.request(t, http.MethodPost, "/api/v1/users", fmt.Sprintf(
		`{"username":"operator","password":%q,"role":"operator"}`, f.secret.operator), http.StatusCreated)
	f.operator.login(t, "operator", f.secret.operator)
	f.localID = f.createLocal(t)
	f.ftpID = f.createFTP(t)
	f.sourceID = f.createSource(t)
	body := fmt.Sprintf(`{"name":"legacy-policy","source_id":%q,"local_repository_id":%q,"replica_repository_id":%q,"full_cron":%q,"incremental_cron":%q,"timezone":"UTC","enabled":false,"max_chain_length":7,"parallel":1,"throttle":0,"zstd_level":3,"page_tracking":false}`,
		f.sourceID, f.localID, f.ftpID, cronFull, cronInc)
	f.e2ePolicy = f.decodeID(t, f.admin.request(t, http.MethodPost, "/api/v1/policies", body, http.StatusCreated))
	f.enable(t, f.e2ePolicy)
	w5Chain(t, f)
	// An actual legacy restore leaves its immutable history and log in SQLite.
	plan, _ := f.w5Plan(t, *w5.inc1.BackupID, http.StatusOK)
	historical := f.w5RunRestoreNow(t, *w5.inc1.BackupID, plan, "legacy-restore", false, nil)
	f.w5Wait(t, historical, store.RestoreStatusCompleted)
	migrationStop(t, f)
	source := filepath.Join(f.layout.metadata, "intern.db")
	legacy, err := sql.Open("sqlite", source)
	must(t, err)
	defer func() { must(t, legacy.Close()) }()
	for _, schema := range []struct {
		name    string
		version int
	}{{"web", 9}, {"physical", 1}, {"replication", 2}} {
		var version int
		must(t, legacy.QueryRowContext(t.Context(), "SELECT MAX(version_id) FROM "+schema.name+"_goose_db_version WHERE is_applied=1").Scan(&version))
		if version != schema.version {
			t.Fatalf("retired production schema %s=%d", schema.name, version)
		}
	}
	must(t, legacy.QueryRowContext(t.Context(), "SELECT id FROM instance").Scan(&f.instanceID))
	// These explicit fault-history rows model a crash after ownership was
	// persisted. They reference the genuine source chain and are never presented
	// as a legacy tool failure. Recovery must reclaim only their exact targets.
	cleanup := migrationCrashHistory(t, f, legacy)
	must(t, legacy.Close())
	before := migrationFileHash(t, source)
	t.Logf("PG-G3 legacy source=%s schema=web:9,physical:1,replication:2 instance=%s sqlite_sha256=%s origin=retired-production-serve chain=full,inc1,inc2 fault_history=explicit-owned-crash-fixture", migrationLegacySource, f.instanceID, before)
	return source, historical, cleanup, before
}

func migrationImportPlane(t *testing.T, f *fixture, source, before string) {
	t.Helper()
	secretFile := filepath.Join(f.layout.root, "database-url")
	must(t, os.WriteFile(secretFile, []byte(f.dsn), 0o600))
	migrationFailureCommands(t, f, source, secretFile)
	output := migrationCommand(t, f, "/tmp/candidate-web", "", true,
		"migrate", "import-sqlite", "--sqlite-file", source, "--database-url-file", secretFile)
	var imported store.ImportResult
	must(t, json.Unmarshal(output, &imported))
	if imported.InstanceID != f.instanceID || len(imported.Rows) != len(migrationTables) {
		t.Fatal("production import changed instance or omitted tables")
	}
	legacy, err := sql.Open("sqlite", source)
	must(t, err)
	pg, err := sql.Open("pgx", f.dsn)
	must(t, err)
	defer func() { must(t, pg.Close()) }()
	for _, table := range migrationTables {
		columns, oldRows, oldDigest := migrationTable(t, legacy, table, nil)
		_, newRows, newDigest := migrationTable(t, pg, table, columns)
		if oldRows != newRows || oldDigest != newDigest || imported.Rows[table] != int64(oldRows) {
			t.Fatalf("import table %s mismatch rows=%d/%d digest=%s/%s", table, oldRows, newRows, oldDigest, newDigest)
		}
		t.Logf("PG-G3 table=%s rows=%d sha256=%s equal=true", table, oldRows, oldDigest)
	}
	must(t, legacy.Close())
	beforeRepeat := migrationDestinationState(t, pg)
	refused := migrationCommand(t, f, "/tmp/candidate-web", "", false,
		"migrate", "import-sqlite", "--sqlite-file", source, "--database-url-file", secretFile)
	if !strings.Contains(string(refused), "already contains business data") {
		t.Fatal("repeated import did not reach occupied-destination refusal")
	}
	migrationAssertDestinationUnchanged(t, pg, beforeRepeat)
	if migrationFileHash(t, source) != before {
		t.Fatal("controlled import changed or deleted source SQLite")
	}
	t.Log("PG-G3 import command=production tables=18 equal=true source_intact=true repeat=refused nonempty=refused rollback=empty")
}

func migrationConsumerContinuity(t *testing.T, f *fixture, source, historical, cleanup, before string) {
	t.Helper()
	pg, err := sql.Open("pgx", f.dsn)
	must(t, err)
	defer func() { must(t, pg.Close()) }()
	migrationServe(t, f, "/tmp/candidate-web", true)
	defer migrationStop(t, f)
	f.login(t)
	migrationSetupClosed(t, f)
	if f.w5Get(t, historical).Status != store.RestoreStatusCompleted || len(f.w5Logs(t, historical)) == 0 {
		t.Fatal("legacy restore history disappeared from API")
	}
	if w5PathExists(cleanup) {
		t.Fatal("imported exact cleanup responsibility was not recovered")
	}
	var remaining int
	must(t, pg.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM cleanup_ledger").Scan(&remaining))
	if remaining != 0 {
		t.Fatalf("imported cleanup rows remain: %d", remaining)
	}
	f.w5DropLocalPayloads(t)
	for _, point := range []struct {
		name     string
		run      store.Run
		snapshot w5Snapshot
	}{
		{"full", w5.full, w5.snapFull}, {"inc1", w5.inc1, w5.snap1}, {"inc2", w5.inc2, w5.snap2},
	} {
		migrationRestore(t, f, point.name, *point.run.BackupID, point.snapshot)
	}
	migrationContinuedBackup(t, f)
	migrationStop(t, f)
	migrationServe(t, f, "/tmp/candidate-web", true)
	f.login(t)
	migrationSetupClosed(t, f)
	migrationStop(t, f)
	f.assertNoSecret(t, nil)
	if migrationFileHash(t, source) != before {
		t.Fatal("serve changed source SQLite")
	}
	t.Log("PG-G3 continuity login=original setup=closed history=visible chain=1,2,3,4 lsn=continuous replicas=4 cleanup=recovered")
}

func migrationContinuedBackup(t *testing.T, f *fixture) {
	t.Helper()
	f.dml(t, 99)
	continued := f.waitRun(t, f.triggerRun(t, f.e2ePolicy, store.RunKindIncremental, "migration-continued"), store.RunStatusCompleted)
	if continued.BackupID == nil {
		t.Fatal("continued incremental published no backup")
	}
	f.waitReplica(t, *continued.BackupID, "verified")
	members := longestChain(t, f.listChains(t))
	if len(members) != 4 || !lsnContinuous(members) || members[3].ID != *continued.BackupID {
		t.Fatal("imported chain did not continue with the fourth member")
	}
	verified := 0
	for _, replica := range f.listReplicas(t, "source_id="+f.sourceID) {
		if replica.Status == "verified" {
			verified++
		}
	}
	if verified != 4 {
		t.Fatalf("verified historical and new replicas=%d want=4", verified)
	}
	// Reclaim only this fixture's newly published member after FTP verification.
	repo, err := local.Open(f.layout.local)
	must(t, err)
	must(t, repo.ReclaimPublished(members[3].ObjectPath))
	must(t, repo.Close())
	migrationRestore(t, f, "continued", *continued.BackupID, f.w5Snapshot(t))
}

func migrationCommand(t *testing.T, f *fixture, binary, input string, success bool, args ...string) []byte {
	t.Helper()
	args = append(args, "--metadata-dir", f.layout.metadata)
	command := exec.CommandContext(t.Context(), binary, args...)
	command.Stdin = strings.NewReader(input)
	command.Env = migrationEnv()
	output, err := command.CombinedOutput()
	migrationAssertNoSecret(t, f, output)
	if (err == nil) != success {
		t.Fatalf("migration command success=%t want=%t", err == nil, success)
	}
	return output
}

// migrationAssertNoSecret supplements the fixture's business credentials with
// this scenario's actual database credentials and cleartext KEK material.
func migrationAssertNoSecret(t *testing.T, f *fixture, output []byte) {
	t.Helper()
	if containsAny(output, migrationSecretNeedles(t, f)) {
		t.Fatal("migration output exposed a secret")
	}
}

func migrationSecretNeedles(t *testing.T, f *fixture) [][]byte {
	t.Helper()
	needles := append(f.secretNeedles(), []byte(f.dsn))
	databaseURL, err := url.Parse(f.dsn)
	if err != nil {
		t.Fatal("cannot parse migration database URL for secret scan")
	}
	if databaseURL.User != nil {
		password, _ := databaseURL.User.Password()
		needles = append(needles, []byte(password), []byte(url.QueryEscape(password)), []byte(url.PathEscape(password)))
	}
	raw, err := os.ReadFile(f.layout.kek)
	must(t, err)
	needles = append(needles, bytes.TrimSpace(raw))
	var keyset struct {
		Key []struct {
			KeyData struct {
				Value []byte `json:"value"`
			} `json:"keyData"`
		} `json:"key"`
	}
	must(t, json.Unmarshal(raw, &keyset))
	if len(keyset.Key) == 0 {
		t.Fatal("migration secret scan found no KEK material")
	}
	for _, key := range keyset.Key {
		var material aesgcm.AesGcmKey
		must(t, proto.Unmarshal(key.KeyData.Value, &material))
		if len(material.KeyValue) == 0 {
			t.Fatal("migration secret scan found empty KEK material")
		}
		for _, secret := range [][]byte{key.KeyData.Value, material.KeyValue} {
			needles = append(needles, secret, []byte(base64.StdEncoding.EncodeToString(secret)), []byte(hex.EncodeToString(secret)))
		}
	}
	return needles
}

func migrationAssertServeLogs(t *testing.T, f *fixture) {
	t.Helper()
	for _, path := range f.childLogs {
		output, err := os.ReadFile(path)
		must(t, err)
		migrationAssertNoSecret(t, f, output)
	}
}

func migrationEnv() []string {
	env := make([]string, 0, len(os.Environ()))
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "INTERN_DATABASE_URL=") && !strings.HasPrefix(value, "INTERN_DATABASE_URL_FILE=") {
			env = append(env, value)
		}
	}
	return env
}

func migrationServe(t *testing.T, f *fixture, binary string, postgres bool) {
	t.Helper()
	f.listen = freeListen(t)
	args := []string{
		"serve", "--metadata-dir", f.layout.metadata, "--listen", f.listen,
		"--tls-cert-file", f.layout.cert, "--tls-key-file", f.layout.key,
		"--kek-file", f.layout.kek, "--work-root", f.layout.work, "--xtrabackup-path", xtrabackupBin,
		"--min-work-bytes", "1", "--min-backup-bytes", "1", "--export-root", f.layout.export,
		"--xbstream-path", f.settings.XbstreamPath, "--mysqld-path", f.settings.MysqldPath,
	}
	if postgres {
		args = append(args, "--database-url-file", filepath.Join(f.layout.root, "database-url"))
	}
	logFile, err := os.CreateTemp(f.layout.root, "migration-serve-*.log")
	must(t, err)
	command := exec.Command(binary, args...)
	command.Env = migrationEnv()
	command.Stdout, command.Stderr = logFile, logFile
	must(t, command.Start())
	f.child = command
	f.childLogs = append(f.childLogs, logFile.Name())
	t.Cleanup(func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
		_ = logFile.Close()
		output, readErr := os.ReadFile(logFile.Name())
		must(t, readErr)
		migrationAssertNoSecret(t, f, output)
	})
	f.bindChildClients(t)
	f.waitHealth(t)
}

func migrationStop(t *testing.T, f *fixture) {
	t.Helper()
	defer migrationAssertServeLogs(t, f)
	if f.child == nil || f.child.ProcessState != nil {
		return
	}
	must(t, f.child.Process.Signal(syscall.SIGTERM))
	done := make(chan error, 1)
	go func() { done <- f.child.Wait() }()
	select {
	case err := <-done:
		must(t, err)
	case <-time.After(30 * time.Second):
		_ = f.child.Process.Kill()
		<-done
		t.Fatal("migration serve failed to drain")
	}
}

func migrationFileHash(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	must(t, err)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func migrationTable(t *testing.T, db *sql.DB, table string, columns []string) ([]string, int, string) {
	t.Helper()
	selection := "*"
	if columns != nil {
		quoted := make([]string, len(columns))
		for i, column := range columns {
			quoted[i] = `"` + column + `"`
		}
		selection = strings.Join(quoted, ",")
	}
	rows, err := db.QueryContext(t.Context(), "SELECT "+selection+" FROM "+table)
	must(t, err)
	defer func() { must(t, rows.Close()) }()
	columns, err = rows.Columns()
	must(t, err)
	var encoded []string
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		must(t, rows.Scan(pointers...))
		data, marshalErr := json.Marshal(values)
		must(t, marshalErr)
		encoded = append(encoded, string(data))
	}
	must(t, rows.Err())
	sort.Strings(encoded)
	return columns, len(encoded), fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(encoded, "\n"))))
}

func migrationRestore(t *testing.T, f *fixture, name, backup string, point w5Snapshot) {
	t.Helper()
	plan, _ := f.w5Plan(t, backup, http.StatusOK)
	if len(plan.Links) == 0 {
		t.Fatal("migration restore selected no links")
	}
	for _, link := range plan.Links {
		if link.Source != "ftp" {
			t.Fatal("migration restore did not use its verified FTP replica")
		}
	}
	id := f.w5RunRestore(t, backup, plan, "migration-"+name, false, nil)
	restored := f.w5Wait(t, id, store.RestoreStatusCompleted)
	if restored.ExportPath == nil || *restored.ExportPath != filepath.Join(f.layout.export, id) {
		t.Fatal("migration restore did not publish its exact export")
	}
	t.Logf("PG-G3 export point=%s id=%s snapshot=%s source=ftp", name, id, point.digest)
}

func migrationCrashHistory(t *testing.T, f *fixture, db *sql.DB) string {
	t.Helper()
	runID := "019a7000-0000-7000-8000-000000000091"
	backupID := "019a7000000070008000000000000091"
	target := filepath.Join(f.layout.work, "run-"+runID)
	must(t, os.Mkdir(target, 0o700))
	chainID := "019a7000000070008000000000000092"
	_, err := db.ExecContext(t.Context(), `INSERT INTO chains (id,source_id,server_uuid,server_version,tool_version,started_at,sealed_at) SELECT ?,source_id,server_uuid,server_version,tool_version,'2000-01-01T00:00:00.000000000Z','2000-01-01T00:00:01.000000000Z' FROM chains WHERE id=?`, chainID, w5.members[0].ChainID)
	must(t, err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO backups (id,chain_id,seq,kind,status,instance_id,run_id,object_path,key_id,created_at,failure_code) SELECT ?,?,1,'physical-full','failed',instance_id,?,object_path || '-interrupted',key_id,created_at,'interrupted' FROM backups WHERE id=?`, backupID, chainID, runID, *w5.inc2.BackupID)
	must(t, err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO cleanup_ledger (backup_id,instance_id,run_id,kind,target,attempts,last_error_code,created_at) VALUES (?,?,?,'workdir',?,2,'cleanup_failed',?)`, backupID, f.instanceID, runID, target, time.Now().UTC().Format(time.RFC3339Nano))
	must(t, err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO replica_parts (backup_id,repository_id,member,part,attempts,last_error_code,created_at) VALUES (?,?,'metadata.bin','.intern-upload-019a7000000070008000000000000091.part',2,'remote_transport',?)`, *w5.full.BackupID, f.ftpID, time.Now().UTC().Format(time.RFC3339Nano))
	must(t, err)
	return target
}

func migrationFailureCommands(t *testing.T, f *fixture, source, secretFile string) {
	t.Helper()
	// An incompatible row fails after users/resources have been copied, proving
	// the production command rolls its whole transaction back.
	invalid := filepath.Join(f.layout.root, "invalid.db")
	data, err := os.ReadFile(source)
	must(t, err)
	must(t, os.WriteFile(invalid, data, 0o600))
	db, err := sql.Open("sqlite", invalid)
	must(t, err)
	_, err = db.ExecContext(t.Context(), "UPDATE runs SET request_hash=?", "invalid\x00text")
	must(t, err)
	must(t, db.Close())
	// Materialize the empty schema and identity before the failing command so
	// rollback must preserve an observed target, including its setup flag.
	target, err := store.Open(t.Context(), f.dsn)
	must(t, err)
	if target.InstanceID() == f.instanceID {
		t.Fatal("rollback target unexpectedly shares the legacy instance identity")
	}
	must(t, target.Close())
	pg, err := sql.Open("pgx", f.dsn)
	must(t, err)
	defer func() { must(t, pg.Close()) }()
	var completed bool
	must(t, pg.QueryRowContext(t.Context(), "SELECT completed FROM setup_state WHERE singleton=1").Scan(&completed))
	if completed {
		t.Fatal("empty rollback target is already initialized")
	}
	empty := migrationDestinationState(t, pg)
	invalidBefore := migrationFileHash(t, invalid)
	sourceBefore := migrationFileHash(t, source)
	failed := migrationCommand(t, f, "/tmp/candidate-web", "", false, "migrate", "import-sqlite", "--sqlite-file", invalid, "--database-url-file", secretFile)
	if !strings.Contains(string(failed), "import SQLite table runs") {
		t.Fatal("rollback injection failed before copying the runs table")
	}
	migrationAssertDestinationUnchanged(t, pg, empty)
	if migrationFileHash(t, invalid) != invalidBefore {
		t.Fatal("failed import changed or deleted its source SQLite")
	}
	_, err = pg.ExecContext(t.Context(), `INSERT INTO users(id,username,password_hash,role,created_at,updated_at) VALUES ('occupied','occupied','hash','viewer','now','now')`)
	must(t, err)
	occupied := migrationDestinationState(t, pg)
	refused := migrationCommand(t, f, "/tmp/candidate-web", "", false, "migrate", "import-sqlite", "--sqlite-file", source, "--database-url-file", secretFile)
	if !strings.Contains(string(refused), "already contains business data") {
		t.Fatal("nonempty import failed without reaching destination protection")
	}
	migrationAssertDestinationUnchanged(t, pg, occupied)
	if migrationFileHash(t, source) != sourceBefore {
		t.Fatal("refused import changed or deleted its source SQLite")
	}
	_, err = pg.ExecContext(t.Context(), "DELETE FROM users")
	must(t, err)
}

// migrationDestinationState includes full rows for identity, setup completion,
// all business tables and each independently owned migration history.
func migrationDestinationState(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	state := make(map[string]string, len(migrationTables)+4)
	tables := append([]string(nil), migrationTables...)
	tables = append(tables, "setup_state", "web_goose_db_version", "physical_goose_db_version", "replication_goose_db_version")
	for _, table := range tables {
		_, count, digest := migrationTable(t, db, table, nil)
		state[table] = fmt.Sprintf("rows=%d sha256=%s", count, digest)
	}
	return state
}

func migrationAssertDestinationUnchanged(t *testing.T, db *sql.DB, before map[string]string) {
	t.Helper()
	after := migrationDestinationState(t, db)
	for table, state := range before {
		if after[table] != state {
			t.Fatalf("rejected import changed target %s: before=%s after=%s", table, state, after[table])
		}
	}
}

func migrationSetupClosed(t *testing.T, f *fixture) {
	t.Helper()
	anonymous := newClient(t, f.admin.http, f.admin.base, "anonymous")
	raw := anonymous.request(t, http.MethodGet, "/api/v1/auth/setup", "", http.StatusOK)
	var status map[string]any
	must(t, json.Unmarshal(raw, &status))
	if len(status) != 1 || status["required"] != false {
		t.Fatal("imported setup status reopened initialization or leaked identity")
	}
	refused := anonymous.request(t, http.MethodPost, "/api/v1/auth/setup", fmt.Sprintf(`{"username":"another-admin","password":%q}`, f.secret.admin), http.StatusConflict)
	if !bytesContainCode(refused, "setup_completed") {
		t.Fatal("anonymous imported setup did not return setup_completed")
	}
}
