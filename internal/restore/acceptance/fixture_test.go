//go:build integration

// Package acceptance is the native P4 restore worker. One pinned container
// image carries the real mysqld plus xtrabackup/xbstream; the worker produces a
// real chain against a real MySQL source, replicates it to a real Pure-FTPd
// fixture, then restores it through internal/restore and starts the rehearsal
// server on the prepared copy. A missing fixture is a failure, never a skip.
package acceptance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	_ "modernc.org/sqlite"

	"github.com/flyingEirc/Intern/internal/artifact"
	"github.com/flyingEirc/Intern/internal/physical"
	"github.com/flyingEirc/Intern/internal/physical/chain"
	physicalstore "github.com/flyingEirc/Intern/internal/physical/store"
	"github.com/flyingEirc/Intern/internal/replication"
	"github.com/flyingEirc/Intern/internal/restore"
	"github.com/flyingEirc/Intern/internal/storage/ftp"
	"github.com/flyingEirc/Intern/internal/storage/local"
)

const (
	socketPath   = "/var/run/mysqld/mysqld.sock"
	dataDir      = "/var/lib/mysql"
	sourceUser   = "intern_bkp"
	instanceID   = "p4-instance"
	localRepoID  = "local-p4"
	ftpRepoID    = "ftp-p4"
	sourceID     = "p4-source"
	rejectSource = "p4-reject"
)

// producedFile is the owner's independent record of one published and replicated
// backup, including the data snapshot taken immediately before the backup.
type producedFile struct {
	SourceID           string `json:"source_id"`
	BackupID           string `json:"backup_id"`
	ChainID            string `json:"chain_id"`
	Seq                int    `json:"seq"`
	ObjectPath         string `json:"object_path"`
	PayloadSize        int64  `json:"payload_size"`
	PayloadSHA256      string `json:"payload_sha256"`
	MetadataSize       int64  `json:"metadata_size"`
	MetadataSHA256     string `json:"metadata_sha256"`
	EnvelopeSHA256     string `json:"envelope_sha256"`
	MetaEnvelopeSHA256 string `json:"metadata_envelope_sha256"`
	SnapshotSHA256     string `json:"snapshot_sha256"`
	RecordsRows        int64  `json:"records_rows"`
	MaxRecordID        int64  `json:"max_record_id"`
	FromLSN            uint64 `json:"from_lsn"`
	ToLSN              uint64 `json:"to_lsn"`
}

type producedManifest struct {
	InstanceID    string         `json:"instance_id"`
	ServerVersion string         `json:"server_version"`
	ToolVersion   string         `json:"tool_version"`
	Backups       []producedFile `json:"backups"`
}

func (m producedManifest) backup(t *testing.T, index int) producedFile {
	t.Helper()
	if index < 0 || index >= len(m.Backups) {
		t.Fatalf("produced manifest has no backup %d", index)
	}
	return m.Backups[index]
}

type layout struct {
	root        string
	key         string
	wrongKey    string
	work        string
	repo        string
	db          string
	manifest    string
	export      string
	smallWork   string
	smallExport string
}

func newLayout(t *testing.T) layout {
	t.Helper()
	root := os.Getenv("INTERN_P4_ROOT")
	if !filepath.IsAbs(root) {
		t.Fatal("the worker requires an absolute private fixture root")
	}
	return layout{
		root: root, key: filepath.Join(root, "keys", "intern.key"), wrongKey: filepath.Join(root, "keys", "wrong.key"),
		work: filepath.Join(root, "work"), repo: filepath.Join(root, "repo"), db: filepath.Join(root, "intern.db"),
		manifest:    filepath.Join(root, "produced.json"),
		export:      requiredAbsoluteEnv(t, "INTERN_P4_EXPORT"),
		smallWork:   requiredAbsoluteEnv(t, "INTERN_P4_SMALL_WORK"),
		smallExport: requiredAbsoluteEnv(t, "INTERN_P4_SMALL_EXPORT"),
	}
}

func requiredAbsoluteEnv(t *testing.T, key string) string {
	t.Helper()
	value := os.Getenv(key)
	if !filepath.IsAbs(value) {
		t.Fatalf("%s must be an absolute path", key)
	}
	return value
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(withhold(err.Error()))
	}
}

// withhold removes every fixture credential and key path before a diagnostic is
// printed as evidence.
func withhold(message string) string {
	for _, key := range []string{"INTERN_P4_PASSWORD", "INTERN_P4_FTP_PASSWORD"} {
		if secret := os.Getenv(key); secret != "" {
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
		}
	}
	if root := os.Getenv("INTERN_P4_ROOT"); root != "" {
		message = strings.ReplaceAll(message, filepath.Join(root, "keys"), "[KEK]")
	}
	return message
}

func requireNative(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Fatal("native Linux amd64 worker required")
	}
	if os.Geteuid() == 0 {
		t.Fatal("the restore worker must not run as root")
	}
}

func requireFixture(t *testing.T, paths layout) {
	t.Helper()
	if os.Getenv("INTERN_P4_PASSWORD") == "" {
		t.Fatal("real physical fixture credentials missing")
	}
	if os.Getenv("INTERN_P4_FTP_ADDRESS") == "" || os.Getenv("INTERN_P4_FTP_PASSWORD") == "" {
		t.Fatal("real FTP fixture endpoint and credentials are required")
	}
	for _, path := range []string{"/usr/bin/xtrabackup", "/usr/bin/xbstream"} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("the worker image lacks %s", path)
		}
	}
	for _, path := range []string{paths.export, paths.smallWork, paths.smallExport} {
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("fixture mount %s is not an existing directory", path)
		}
	}
}

func mysqldPath(t *testing.T) string {
	t.Helper()
	for _, path := range []string{"/usr/sbin/mysqld", "/usr/bin/mysqld"} {
		if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() {
			return path
		}
	}
	t.Fatal("the worker image lacks mysqld")
	return ""
}

func openControlDB(t *testing.T, filename string) *sql.DB {
	t.Helper()
	u := url.URL{Scheme: "file", Path: filename}
	q := u.Query()
	for _, pragma := range []string{"journal_mode(WAL)", "synchronous(FULL)", "foreign_keys(1)", "busy_timeout(5000)"} {
		q.Add("_pragma", pragma)
	}
	q.Set("_txlock", "immediate")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	must(t, err)
	db.SetMaxOpenConns(1)
	must(t, physicalstore.Migrate(t.Context(), db))
	must(t, replication.Migrate(t.Context(), db))
	t.Cleanup(func() { must(t, db.Close()) })
	return db
}

func openSource(t *testing.T) *sql.DB {
	t.Helper()
	cfg := driver.NewConfig()
	cfg.Net, cfg.Addr, cfg.User, cfg.Passwd = "unix", socketPath, sourceUser, os.Getenv("INTERN_P4_PASSWORD")
	cfg.Timeout, cfg.ReadTimeout, cfg.WriteTimeout = 5*time.Second, 60*time.Second, 60*time.Second
	connector, err := driver.NewConnector(cfg)
	must(t, err)
	db := sql.OpenDB(connector)
	must(t, db.PingContext(t.Context()))
	t.Cleanup(func() { must(t, db.Close()) })
	return db
}

func (w *worker) openFTP() *ftp.Repository {
	w.t.Helper()
	repository, err := ftp.Open(w.t.Context(), ftp.Config{
		Address: os.Getenv("INTERN_P4_FTP_ADDRESS"), Username: "normal", Password: os.Getenv("INTERN_P4_FTP_PASSWORD"),
		BasePath: "/", InstanceID: instanceID, TLSMode: "plain", DialTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second,
	})
	must(w.t, err)
	w.t.Cleanup(func() { must(w.t, repository.Close()) })
	return repository
}

type worker struct {
	t            *testing.T
	paths        layout
	db           *sql.DB
	store        *physicalstore.Store
	catalog      *replication.Catalog
	repo         *local.Repository
	remote       *ftp.Repository
	source       *sql.DB
	preflight    physical.PreflightResult
	manifest     producedManifest
	now          time.Time
	mysqld       string
	facts        restoreFacts
	rejectBackup producedFile
}

// restoreFacts records what each real restore produced, for the argv evidence
// and for the harness markers.
type restoreFacts struct {
	prepareArgs map[string][][]string
	sources     map[string]string
}

func newWorker(t *testing.T) *worker {
	t.Helper()
	requireNative(t)
	paths := newLayout(t)
	requireFixture(t, paths)
	for _, dir := range []string{paths.root, filepath.Dir(paths.key), paths.work} {
		must(t, os.MkdirAll(dir, 0o700))
	}
	for _, dir := range []string{paths.root, filepath.Dir(paths.key), paths.work} {
		info, err := os.Lstat(dir)
		must(t, err)
		if !info.IsDir() || info.Mode() != os.ModeDir|0o700 {
			t.Fatalf("fixture directory %s must be a private 0700 directory", dir)
		}
	}
	for _, key := range []string{paths.key, paths.wrongKey} {
		if _, err := os.Lstat(key); errors.Is(err, fs.ErrNotExist) {
			must(t, artifact.GenerateKey(key))
		} else if err != nil {
			must(t, err)
		}
	}
	repo, err := local.Open(paths.repo)
	must(t, err)
	t.Cleanup(func() { must(t, repo.Close()) })
	w := &worker{
		t: t, paths: paths, db: openControlDB(t, paths.db), repo: repo, source: openSource(t),
		now: time.Now().UTC().Add(time.Minute), mysqld: mysqldPath(t),
		facts: restoreFacts{prepareArgs: map[string][][]string{}, sources: map[string]string{}},
	}
	w.store = physicalstore.New(w.db)
	w.catalog = replication.NewCatalog(w.db)
	w.remote = w.openFTP()
	opts := physical.PreflightOptions{
		Connection: physical.Connection{Socket: socketPath, DataDir: dataDir, Username: sourceUser, Password: os.Getenv("INTERN_P4_PASSWORD")},
		Binary:     "/usr/bin/xtrabackup", WorkRoot: paths.work, BackupRoot: paths.repo, MinWorkBytes: 1, MinBackupBytes: 1,
	}
	w.preflight, err = physical.Preflight(t.Context(), opts)
	must(t, err)
	return w
}

func (w *worker) replicator() *replication.Replicator {
	return &replication.Replicator{
		DB: w.db, Store: w.store, Local: w.repo, Remote: replication.NewFTPRemote(w.remote),
		RepositoryID: ftpRepoID, LocalRepositoryID: localRepoID, InstanceID: instanceID,
		KeyFile: w.paths.key, WorkRoot: w.paths.work, MaxExpandedBytes: 4 << 30,
		Now: func() time.Time { return w.now },
	}
}

// workerLocator implements restore.Locator directly from the replica rows of one
// control-plane database, so a cloned database is observed independently.
type workerLocator struct {
	w  *worker
	db *sql.DB
}

func (l workerLocator) Local(context.Context, string) (*local.Repository, error) {
	return l.w.repo, nil
}

func (l workerLocator) Replicas(ctx context.Context, backupID string) ([]restore.ReplicaRef, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT repository_id, status FROM replicas WHERE backup_id = ? ORDER BY repository_id`, backupID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var refs []restore.ReplicaRef
	for rows.Next() {
		var repositoryID, status string
		if err = rows.Scan(&repositoryID, &status); err != nil {
			return nil, err
		}
		refs = append(refs, restore.ReplicaRef{RepositoryID: repositoryID, Open: func(context.Context) (*ftp.Repository, error) {
			return l.w.openFTP(), nil
		}})
	}
	return refs, rows.Err()
}

// options builds the production restore configuration. RestrictedRoots names the
// metadata-like fixture root so an export inside it would be refused.
func (w *worker) options() restore.Options {
	return restore.Options{
		Store: w.store, Replication: w.catalog, Locator: workerLocator{w: w, db: w.db},
		KeyFile: w.paths.key, WorkRoot: w.paths.work, ExportRoot: w.paths.export,
		XtrabackupPath: "/usr/bin/xtrabackup", XbstreamPath: "/usr/bin/xbstream", MysqldPath: w.mysqld,
		RestrictedRoots: []string{w.paths.root}, ExpansionFactor: 8, Parallel: 2,
	}
}

func (w *worker) rejectOptions() restore.Options {
	opts := w.options()
	opts.XtrabackupPath = w.toolWrapper()
	return opts
}

// toolWrapper is the real pinned xtrabackup plus an argv log. The version banner
// is re-emitted under the wrapper path so the engine's probe accepts it, and
// every prepare would be recorded; the rejection suite requires no prepare line.
func (w *worker) toolWrapper() string {
	w.t.Helper()
	path := filepath.Join(w.paths.root, "bin", "xtrabackup")
	if _, err := os.Lstat(path); err == nil {
		return path
	}
	must(w.t, os.MkdirAll(filepath.Dir(path), 0o700))
	body := `#!/bin/sh
log="${INTERN_P4_TOOL_LOG:?}"
printf '%s\n' "$*" >> "$log"
for arg in "$@"; do
  if [ "$arg" = "--version" ]; then
    /usr/bin/xtrabackup --version | sed -e "s#^/usr/bin/xtrabackup#$0#"
    exit 0
  fi
done
exec /usr/bin/xtrabackup "$@"
`
	must(w.t, os.WriteFile(path, []byte(body), 0o700))
	return path
}

func (w *worker) toolLog() string { return filepath.Join(w.paths.root, "tool-argv.log") }

// mutate applies substantive DML and, for an incremental, moves the InnoDB
// checkpoint strictly past the parent's to_lsn so the parent is a real recovery
// point instead of a duplicate LSN.
func (w *worker) mutate(ctx context.Context, index int, after *uint64) {
	w.t.Helper()
	for row := range 64 {
		_, err := w.source.ExecContext(ctx, "INSERT INTO fixture.records(value) VALUES (?)", int64(index*1000+row))
		must(w.t, err)
	}
	payload := bytes.Repeat([]byte{'a'}, 256*1024)
	if after == nil {
		for range 8 {
			_, err := w.source.ExecContext(ctx, "INSERT INTO fixture.lsn_pressure(payload) VALUES (?)", payload)
			must(w.t, err)
		}
		return
	}
	limit := redoCapacity(w.t, ctx, w.source) * 2
	if limit > 256<<20 {
		w.t.Fatalf("redo capacity %d is too large for the fixture", limit/2)
	}
	deadline := time.Now().Add(90 * time.Second)
	var written uint64
	for {
		if written < limit {
			_, err := w.source.ExecContext(ctx, "INSERT INTO fixture.lsn_pressure(payload) VALUES (?)", payload)
			must(w.t, err)
			written += uint64(len(payload))
		}
		if serverCheckpoint(w.t, ctx, w.source) > *after {
			return
		}
		if time.Now().After(deadline) {
			w.t.Fatalf("checkpoint did not move past %d after %d bytes", *after, written)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func redoCapacity(t *testing.T, ctx context.Context, db *sql.DB) uint64 {
	t.Helper()
	var value string
	must(t, db.QueryRowContext(ctx, "SELECT VARIABLE_VALUE FROM performance_schema.global_variables WHERE VARIABLE_NAME = 'innodb_redo_log_capacity'").Scan(&value))
	capacity, err := strconv.ParseUint(value, 10, 64)
	must(t, err)
	if capacity == 0 {
		t.Fatal("innodb_redo_log_capacity is zero")
	}
	return capacity
}

func serverCheckpoint(t *testing.T, ctx context.Context, db *sql.DB) uint64 {
	t.Helper()
	var raw string
	must(t, db.QueryRowContext(ctx, "SELECT storage_engines FROM performance_schema.log_status").Scan(&raw))
	var body struct {
		InnoDB struct {
			Checkpoint uint64 `json:"LSN_checkpoint"`
		} `json:"InnoDB"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil || body.InnoDB.Checkpoint == 0 {
		t.Fatalf("cannot read the InnoDB checkpoint: %v", err)
	}
	return body.InnoDB.Checkpoint
}

// snapshotStats is the same independent data snapshot the P3 harness uses, plus
// the exact row count and maximum id used to check the rehearsal queries.
func (w *worker) snapshotStats(ctx context.Context) (string, int64, int64) {
	w.t.Helper()
	hash := sha256.New()
	for _, query := range []string{
		"SELECT id, CAST(value AS CHAR) FROM fixture.records ORDER BY id",
		"SELECT id, SHA2(payload,256) FROM fixture.lsn_pressure ORDER BY id",
	} {
		rows, err := w.source.QueryContext(ctx, query)
		must(w.t, err)
		for rows.Next() {
			var id int64
			var value string
			must(w.t, rows.Scan(&id, &value))
			_, err = fmt.Fprintf(hash, "%d:%s\n", id, value)
			must(w.t, err)
		}
		must(w.t, rows.Err())
		must(w.t, rows.Close())
		_, err = hash.Write([]byte{0})
		must(w.t, err)
	}
	var rowsCount, maxID int64
	must(w.t, w.source.QueryRowContext(ctx, "SELECT COUNT(*), COALESCE(MAX(id), 0) FROM fixture.records").Scan(&rowsCount, &maxID))
	return hex.EncodeToString(hash.Sum(nil)), rowsCount, maxID
}

// produce publishes the real chain used by every restore scenario plus one
// unreplicated full backup for the truncated-local rejection.
func (w *worker) produce() {
	w.t.Helper()
	ctx, cancel := context.WithTimeout(w.t.Context(), 12*time.Minute)
	defer cancel()
	w.manifest = producedManifest{InstanceID: instanceID, ServerVersion: w.preflight.Identity.Version, ToolVersion: w.preflight.Tool.Version}
	var parent chain.Result
	for index := range 3 {
		var after *uint64
		if index > 0 {
			after = &parent.Checkpoints.ToLSN
		}
		w.mutate(ctx, index, after)
		snapshot, rowsCount, maxID := w.snapshotStats(ctx)
		kind := "incremental"
		if index == 0 {
			kind = "full"
		}
		result := w.runBackup(ctx, sourceID, kind)
		if after != nil && (result.Checkpoints.FromLSN != *after || result.ChainID != parent.ChainID) {
			w.t.Fatal("incremental did not continue the produced chain")
		}
		if afterSnapshot, _, _ := w.snapshotStats(ctx); afterSnapshot != snapshot {
			w.t.Fatal("fixture data changed during the physical snapshot")
		}
		w.manifest.Backups = append(w.manifest.Backups, w.record(ctx, result, sourceID, snapshot, rowsCount, maxID))
		w.t.Logf("P4-A0 produced seq=%d backup=%s from_lsn=%d to_lsn=%d snapshot=%s rows=%d", result.Seq, result.BackupID, result.Checkpoints.FromLSN, result.Checkpoints.ToLSN, snapshot, rowsCount)
		parent = result
	}
	w.mutate(ctx, 3, &parent.Checkpoints.ToLSN)
	snapshot, rowsCount, maxID := w.snapshotStats(ctx)
	reject := w.runBackup(ctx, rejectSource, "full")
	w.rejectBackup = w.record(ctx, reject, rejectSource, snapshot, rowsCount, maxID)
	w.t.Logf("P4-A0 reject_source backup=%s chain=%s", reject.BackupID, reject.ChainID)
	w.replicate(ctx)
	must(w.t, writeManifest(w.paths.manifest, w.manifest))
}

func (w *worker) runBackup(ctx context.Context, sourceIDValue, kind string) chain.Result {
	w.t.Helper()
	job := chain.Job{
		Store: w.store, Repo: w.repo, Binary: "/usr/bin/xtrabackup", WorkRoot: w.paths.work,
		Connection: physical.Connection{Socket: socketPath, DataDir: dataDir, Username: sourceUser, Password: os.Getenv("INTERN_P4_PASSWORD")},
		Identity:   w.preflight.Identity, ToolVersion: w.preflight.Tool.Version,
		InstanceID: instanceID, SourceID: sourceIDValue, Kind: kind, MaxChainLength: 8, KeyFile: w.paths.key, Parallel: 2,
	}
	result, err := chain.Run(ctx, job)
	must(w.t, err)
	return result
}

func (w *worker) record(ctx context.Context, result chain.Result, source string, snapshot string, rowsCount, maxID int64) producedFile {
	w.t.Helper()
	backup, err := w.store.Backup(ctx, result.BackupID)
	must(w.t, err)
	if backup.Status != "completed" || !backup.LocalPresent || backup.ObjectPath != result.ObjectPath {
		w.t.Fatal("the produced backup is not completed and locally present")
	}
	return producedFile{
		SourceID: source, BackupID: backup.ID, ChainID: backup.ChainID, Seq: backup.Seq, ObjectPath: backup.ObjectPath,
		PayloadSize: backup.PayloadSize, PayloadSHA256: backup.PayloadSHA256,
		MetadataSize: backup.MetadataSize, MetadataSHA256: backup.MetadataSHA256,
		EnvelopeSHA256:     w.localDigest(backup.ObjectPath, "envelope.json"),
		MetaEnvelopeSHA256: w.localDigest(backup.ObjectPath, "metadata.envelope.json"),
		SnapshotSHA256:     snapshot, RecordsRows: rowsCount, MaxRecordID: maxID,
		FromLSN: backup.FromLSN, ToLSN: backup.ToLSN,
	}
}

func (w *worker) localDigest(objectPath, member string) string {
	w.t.Helper()
	file, err := w.repo.Open(objectPath + "/" + member)
	must(w.t, err)
	defer func() { must(w.t, file.Close()) }()
	hash := sha256.New()
	_, err = io.Copy(hash, file)
	must(w.t, err)
	return hex.EncodeToString(hash.Sum(nil))
}

// replicate copies every produced main-chain backup to the real FTP fixture and
// requires the verification readback to record verified.
func (w *worker) replicate(ctx context.Context) {
	w.t.Helper()
	r := w.replicator()
	for _, file := range w.manifest.Backups {
		must(w.t, r.Enqueue(ctx, file.BackupID))
	}
	for attempts := 0; ; attempts++ {
		processed, err := r.Step(ctx)
		if !processed {
			break
		}
		if err != nil {
			w.now = w.now.Add(time.Minute)
			if attempts > 12 {
				w.t.Fatalf("replication did not complete: %v", err)
			}
			continue
		}
		if attempts > 12 {
			w.t.Fatal("replication queue did not drain")
		}
	}
	for _, file := range w.manifest.Backups {
		record, err := w.catalog.Replica(ctx, file.BackupID, ftpRepoID)
		must(w.t, err)
		if record.Status != replication.StatusVerified {
			w.t.Fatalf("replica status = %q, want verified", record.Status)
		}
	}
}

// remoteDigest is one observable state of the whole replica namespace: every
// key with its size and content digest, plus the instance marker.
func (w *worker) remoteDigest(t *testing.T) string {
	t.Helper()
	keys, err := w.remote.List(t.Context(), instanceID)
	must(t, err)
	hash := sha256.New()
	for _, key := range keys {
		size, err := w.remote.Size(t.Context(), key)
		must(t, err)
		reader, err := w.remote.Open(t.Context(), key, 0)
		must(t, err)
		digest := sha256.New()
		written, copyErr := io.Copy(digest, reader)
		closeErr := reader.Close()
		if copyErr != nil || closeErr != nil || written != size {
			t.Fatalf("remote member %s could not be read back", key)
		}
		fmt.Fprintf(hash, "%s|%d|%s\n", key, size, hex.EncodeToString(digest.Sum(nil)))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func writeManifest(path string, manifest producedManifest) error {
	raw, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

func loadManifest(t *testing.T, path string) producedManifest {
	t.Helper()
	raw, err := os.ReadFile(path)
	must(t, err)
	var manifest producedManifest
	if err = json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.InstanceID != instanceID || len(manifest.Backups) != 3 {
		t.Fatal("the produced manifest does not describe this instance and chain")
	}
	return manifest
}
