//go:build integration

// Package acceptance is the combined native R1 harness worker. One pinned PXB
// container produces real XtraBackup chains against a real MySQL into a 0700
// repository plus a fully migrated control-plane database, then the same worker
// runs the Replicator against the real Pure-FTPd fixture. The worker runs as the
// mysql uid, never root, and needs both fixtures: running it without them is an
// error, never a skip.
package acceptance

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	client "github.com/jlaffaye/ftp"
	_ "modernc.org/sqlite"

	"github.com/flyingEirc/Intern/internal/artifact"
	physicalstore "github.com/flyingEirc/Intern/internal/physical/store"
	"github.com/flyingEirc/Intern/internal/replication"
	"github.com/flyingEirc/Intern/internal/storage/ftp"
	"github.com/flyingEirc/Intern/internal/storage/local"
)

// The FTP instance must equal the chain instance, so every scenario shares one
// remote namespace and differs only by repository identity and object path.
const (
	instanceID  = "r1-instance"
	primaryRepo = "ftp-primary"
	quotaRepo   = "ftp-quota"
	socketPath  = "/var/run/mysqld/mysqld.sock"
	dataDir     = "/var/lib/mysql"
)

// members is the fixed upload order of one replica.
var members = []string{"payload.bin", "envelope.json", "metadata.bin", "metadata.envelope.json"}

// layout is the private fixture tree below the root the harness created for this
// uid. The root is 0700 and owned here, and the KEK lives outside the work root.
type layout struct {
	root, key, wrongKey, work, repo, db, manifest string
}

func newLayout(t *testing.T) layout {
	t.Helper()
	root := os.Getenv("INTERN_R1_ROOT")
	if !filepath.IsAbs(root) {
		t.Fatal("the worker requires an absolute private fixture root")
	}
	return layout{
		root: root, key: filepath.Join(root, "keys", "intern.key"),
		wrongKey: filepath.Join(root, "keys", "wrong.key"), work: filepath.Join(root, "work"),
		repo: filepath.Join(root, "repo"), db: filepath.Join(root, "intern.db"),
		manifest: filepath.Join(root, "produced.json"),
	}
}

// producedFile is one real published backup, recorded by the producer so the
// scenarios have an authority independent of the engine under test.
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
	FromLSN            uint64 `json:"from_lsn"`
	ToLSN              uint64 `json:"to_lsn"`
}

// producedManifest binds the produced artifacts to the real versions that wrote
// them and to this instance.
type producedManifest struct {
	InstanceID    string         `json:"instance_id"`
	ServerVersion string         `json:"server_version"`
	ToolVersion   string         `json:"tool_version"`
	Backups       []producedFile `json:"backups"`
}

func (m producedManifest) bySource(sourceID string) []producedFile {
	var files []producedFile
	for _, file := range m.Backups {
		if file.SourceID == sourceID {
			files = append(files, file)
		}
	}
	return files
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(withhold(err.Error()))
	}
}

// withhold removes every fixture credential before a diagnostic can be printed
// or saved as evidence.
func withhold(message string) string {
	for _, key := range []string{"INTERN_R1_PASSWORD", "INTERN_R1_FTP_PASSWORD", "INTERN_R1_FTP_QUOTA_PASSWORD"} {
		if secret := os.Getenv(key); secret != "" {
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
		}
	}
	return message
}

func requireNative(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Fatal("native Linux amd64 worker required")
	}
	if os.Geteuid() == 0 {
		t.Fatal("the replication worker must not run as root")
	}
}

// openDB opens the shared control-plane database with the web store pragmas.
func openDB(t *testing.T, name string, migrate bool) *sql.DB {
	t.Helper()
	u := url.URL{Scheme: "file", Path: name}
	q := u.Query()
	for _, pragma := range []string{"journal_mode(WAL)", "synchronous(FULL)", "foreign_keys(1)", "busy_timeout(5000)"} {
		q.Add("_pragma", pragma)
	}
	q.Set("_txlock", "immediate")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	must(t, err)
	db.SetMaxOpenConns(1)
	if migrate {
		must(t, physicalstore.Migrate(t.Context(), db))
		must(t, replication.Migrate(t.Context(), db))
	}
	t.Cleanup(func() { must(t, db.Close()) })
	return db
}

// worker owns the fixture tree, the control-plane database, the local repository
// and both real FTP accounts. It is created once per process and used by the
// producer phase and then by the replication scenarios.
type worker struct {
	t        *testing.T
	paths    layout
	db       *sql.DB
	store    *physicalstore.Store
	repo     *local.Repository
	manifest producedManifest
	now      time.Time
	ftp      *ftp.Repository
	quota    *ftp.Repository
}

func newWorker(t *testing.T) *worker {
	t.Helper()
	requireNative(t)
	if os.Getenv("INTERN_R1_FTP_ADDRESS") == "" || os.Getenv("INTERN_R1_FTP_PASSWORD") == "" || os.Getenv("INTERN_R1_FTP_QUOTA_PASSWORD") == "" {
		t.Fatal("real FTP fixture endpoint and per-account credentials are required")
	}
	paths := prepareRoot(t)
	repo, err := local.Open(paths.repo)
	must(t, err)
	t.Cleanup(func() { must(t, repo.Close()) })
	db := openDB(t, paths.db, true)
	w := &worker{t: t, paths: paths, db: db, store: physicalstore.New(db), repo: repo, now: time.Now().UTC()}
	w.ftp = w.openRemote("normal", os.Getenv("INTERN_R1_FTP_PASSWORD"))
	w.quota = w.openRemote("quota", os.Getenv("INTERN_R1_FTP_QUOTA_PASSWORD"))
	return w
}

// prepareRoot creates the private fixture tree and both KEKs. The chain KEK and
// the unrelated KEK live outside the work root, as the artifact package requires.
// It is idempotent: a separate worker process reuses the keys its owner made, so a
// restarted process reads the same chains instead of resealing with a new key.
func prepareRoot(t *testing.T) layout {
	t.Helper()
	paths := newLayout(t)
	for _, dir := range []string{paths.root, filepath.Dir(paths.key), paths.work} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			must(t, err)
		}
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
			continue
		} else if err != nil {
			must(t, err)
		}
	}
	return paths
}

// openRemote opens one real FTP account inside the single instance namespace.
func (w *worker) openRemote(user, password string) *ftp.Repository {
	w.t.Helper()
	repository, err := ftp.Open(w.t.Context(), ftp.Config{
		Address: os.Getenv("INTERN_R1_FTP_ADDRESS"), Username: user, Password: password,
		BasePath: "/", InstanceID: instanceID, TLSMode: "plain",
		DialTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second,
	})
	must(w.t, err)
	w.t.Cleanup(func() { must(w.t, repository.Close()) })
	return repository
}

// loadManifest reads the produced authority the producer phase wrote.
func (w *worker) loadManifest() {
	w.t.Helper()
	raw, err := os.ReadFile(w.paths.manifest)
	must(w.t, err)
	if err = json.Unmarshal(raw, &w.manifest); err != nil {
		w.t.Fatal(err)
	}
	if w.manifest.InstanceID != instanceID || len(w.manifest.Backups) == 0 {
		w.t.Fatal("the produced manifest does not describe this instance")
	}
}

// replicator binds one repository identity, its real remote, the injected clock
// and one policy. Only the repository identity separates the scenarios.
func (w *worker) replicator(repositoryID string, remote replication.Remote, keyPath string, policy replication.Policy) *replication.Replicator {
	return &replication.Replicator{
		DB: w.db, Store: w.store, Local: w.repo, Remote: remote,
		RepositoryID: repositoryID, InstanceID: instanceID, KeyFile: keyPath, WorkRoot: w.paths.work,
		MaxExpandedBytes: 4 << 30, Policy: policy, Now: func() time.Time { return w.now },
	}
}

// primary is the replicator of the FTP account every scenario uses by default.
func (w *worker) primary(policy replication.Policy) *replication.Replicator {
	return w.replicator(primaryRepo, replication.NewFTPRemote(w.ftp), w.paths.key, policy)
}

// produced returns the producer's own record of the backups of one source.
func (w *worker) produced(t *testing.T, sourceID string) []producedFile {
	t.Helper()
	files := w.manifest.bySource(sourceID)
	if len(files) == 0 {
		t.Fatalf("producer recorded no backup for source %s", sourceID)
	}
	return files
}

// producedOfChain narrows one source's records to a single chain.
func (w *worker) producedOfChain(t *testing.T, sourceID, chainID string) []producedFile {
	t.Helper()
	var selected []producedFile
	for _, file := range w.produced(t, sourceID) {
		if file.ChainID == chainID {
			selected = append(selected, file)
		}
	}
	if len(selected) == 0 {
		t.Fatalf("no produced backup of source %s belongs to chain %s", sourceID, chainID)
	}
	return selected
}

func (w *worker) backup(t *testing.T, id string) physicalstore.Backup {
	t.Helper()
	record, err := w.store.Backup(t.Context(), id)
	must(t, err)
	return record
}

func (w *worker) replica(t *testing.T, replicator *replication.Replicator, backupID string) replication.Replica {
	t.Helper()
	record, err := replicator.Replica(t.Context(), backupID)
	must(t, err)
	return record
}

// replicaFor reads one replica of any repository. Only the identity matters for
// the accessor, so no remote is attached.
func (w *worker) replicaFor(t *testing.T, repositoryID, backupID string) replication.Replica {
	t.Helper()
	record, err := (&replication.Replicator{DB: w.db, RepositoryID: repositoryID}).Replica(t.Context(), backupID)
	must(t, err)
	return record
}

func (w *worker) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var count int
	must(t, w.db.QueryRowContext(t.Context(), query, args...).Scan(&count))
	return count
}

func (w *worker) partRows(t *testing.T, repositoryID string) int {
	return w.count(t, "SELECT COUNT(*) FROM replica_parts WHERE repository_id = ?", repositoryID)
}

// memberStatuses returns the per-member state of one replica.
func (w *worker) memberStatuses(t *testing.T, backupID, repositoryID string) map[string]string {
	t.Helper()
	rows, err := w.db.QueryContext(t.Context(),
		"SELECT member, status FROM replica_members WHERE backup_id = ? AND repository_id = ?", backupID, repositoryID)
	must(t, err)
	defer func() { must(t, rows.Close()) }()
	result := make(map[string]string, len(members))
	for rows.Next() {
		var member, status string
		must(t, rows.Scan(&member, &status))
		result[member] = status
	}
	must(t, rows.Err())
	return result
}

// alertSince reads the durable alert time of one replica, nil when cleared.
func (w *worker) alertSince(t *testing.T, repositoryID, backupID string) *time.Time {
	t.Helper()
	var value sql.NullString
	err := w.db.QueryRowContext(t.Context(),
		"SELECT alert_since FROM replicas WHERE backup_id = ? AND repository_id = ?", backupID, repositoryID).Scan(&value)
	must(t, err)
	if !value.Valid {
		return nil
	}
	parsed, parseErr := time.Parse(time.RFC3339Nano, value.String)
	must(t, parseErr)
	return &parsed
}

// nextAttempt reads the durable schedule of one replica.
func (w *worker) nextAttempt(t *testing.T, repositoryID, backupID string) time.Time {
	t.Helper()
	record := w.replicaFor(t, repositoryID, backupID)
	if record.NextAttemptAt == nil {
		t.Fatal("replica has no schedule")
	}
	return *record.NextAttemptAt
}

// remoteKeys returns the four remote keys of one backup in the fixed member
// order. The remote key is the local object path plus the member.
func remoteKeys(file producedFile) []string {
	keys := make([]string, 0, len(members))
	for _, member := range members {
		keys = append(keys, file.ObjectPath+"/"+member)
	}
	return keys
}

// remoteDigests returns the member digests the producer recorded.
func remoteDigests(file producedFile) map[string]string {
	return map[string]string{
		"payload.bin":            file.PayloadSHA256,
		"envelope.json":          file.EnvelopeSHA256,
		"metadata.bin":           file.MetadataSHA256,
		"metadata.envelope.json": file.MetaEnvelopeSHA256,
	}
}

// localDigest hashes one published local member through the real repository and
// returns its digest and byte count.
func (w *worker) localDigest(t *testing.T, file producedFile, member string) (string, int64) {
	t.Helper()
	opened, err := w.repo.Open(file.ObjectPath + "/" + member)
	must(t, err)
	defer func() { must(t, opened.Close()) }()
	hash := sha256.New()
	size, err := io.Copy(hash, opened)
	must(t, err)
	return hex.EncodeToString(hash.Sum(nil)), size
}

// localPath is the absolute file of one published local member.
func (w *worker) localPath(file producedFile, member string) string {
	return filepath.Join(w.paths.repo, filepath.FromSlash(file.ObjectPath), member)
}

func (w *worker) localBytes(t *testing.T, file producedFile, member string) []byte {
	t.Helper()
	data, err := os.ReadFile(w.localPath(file, member))
	must(t, err)
	return data
}

// corruptLocal flips bytes of one member in place, keeping its size, and returns
// the original bytes so the scenario can restore them.
func (w *worker) corruptLocal(t *testing.T, file producedFile, member string) []byte {
	t.Helper()
	original := w.localBytes(t, file, member)
	if len(original) < 64 {
		t.Fatalf("local member %s is too small to corrupt", member)
	}
	corrupted := bytes.Clone(original)
	for i := len(corrupted) / 2; i < len(corrupted)/2+16; i++ {
		corrupted[i] ^= 0xff
	}
	must(t, os.WriteFile(w.localPath(file, member), corrupted, 0o600))
	return original
}

func (w *worker) restoreLocal(t *testing.T, file producedFile, member string, data []byte) {
	t.Helper()
	must(t, os.WriteFile(w.localPath(file, member), data, 0o600))
}

// published reports whether the local object directory of one backup exists.
func (w *worker) published(t *testing.T, file producedFile) bool {
	t.Helper()
	_, err := os.Lstat(filepath.Join(w.paths.repo, filepath.FromSlash(file.ObjectPath)))
	if err == nil {
		return true
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	must(t, err)
	return false
}

// raw is an independent FTP client. It tampers with and observes the real remote
// without the code under test, so a remote failure is a real remote state and
// never a double.
type raw struct {
	conn *client.ServerConn
}

func (w *worker) rawNormal(t *testing.T) *raw {
	return w.rawAccount(t, "normal", os.Getenv("INTERN_R1_FTP_PASSWORD"))
}

func (w *worker) rawQuota(t *testing.T) *raw {
	return w.rawAccount(t, "quota", os.Getenv("INTERN_R1_FTP_QUOTA_PASSWORD"))
}

func (w *worker) rawAccount(t *testing.T, user, password string) *raw {
	t.Helper()
	conn, err := client.Dial(os.Getenv("INTERN_R1_FTP_ADDRESS"), client.DialWithTimeout(10*time.Second))
	must(t, err)
	if err = conn.Login(user, password); err != nil {
		_ = conn.Quit() //nolint:errcheck // The login failure is the diagnostic.
		must(t, err)
	}
	t.Cleanup(func() {
		if quitErr := conn.Quit(); quitErr != nil && !errors.Is(quitErr, io.EOF) {
			t.Logf("raw FTP quit: %v", quitErr)
		}
	})
	return &raw{conn: conn}
}

// retrieve reads one remote object and returns its SHA-256 and byte count.
func (r *raw) retrieve(t *testing.T, key string) (string, int64) {
	t.Helper()
	response, err := r.conn.Retr(key)
	must(t, err)
	hash := sha256.New()
	size, err := io.Copy(hash, response)
	closeErr := response.Close()
	must(t, errors.Join(err, closeErr))
	return hex.EncodeToString(hash.Sum(nil)), size
}

func (r *raw) store(t *testing.T, key string, data []byte) {
	t.Helper()
	must(t, r.conn.Stor(key, bytes.NewReader(data)))
}

func (r *raw) remove(t *testing.T, key string) {
	t.Helper()
	must(t, r.conn.Delete(key))
}

// exists requires a real SIZE for the object and returns that length.
func (r *raw) exists(t *testing.T, key string) int64 {
	t.Helper()
	size, err := r.conn.FileSize(key)
	must(t, err)
	return size
}

func (r *raw) absent(t *testing.T, key string) {
	t.Helper()
	if _, err := r.conn.FileSize(key); err == nil {
		t.Fatalf("remote object %s still exists", key)
	}
}

// parts lists the leftover temporary names below one remote directory, so a
// cleanup is observed on the server rather than inferred from local state.
func (r *raw) parts(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := r.conn.List(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name, ".intern-upload-") {
			names = append(names, entry.Name)
		}
	}
	return names
}

// truncate replaces one remote object with a shorter body.
func (r *raw) truncate(t *testing.T, key string, data []byte) {
	t.Helper()
	r.remove(t, key)
	r.store(t, key, data)
}

// mkdirs creates every component of one remote directory.
func (r *raw) mkdirs(t *testing.T, dir string) {
	t.Helper()
	current := ""
	for _, part := range strings.Split(dir, "/") {
		if part == "" {
			continue
		}
		current += "/" + part
		// An existing directory is the expected outcome for every prefix here.
		_ = r.conn.MakeDir(current)
	}
}

// removeTree deletes the published objects below one remote directory. Reserved
// names are left alone: the ownership marker and temporary parts are not
// published members.
func (r *raw) removeTree(t *testing.T, dir string) {
	t.Helper()
	entries, err := r.conn.List(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name, ".") {
			continue
		}
		key := dir + "/" + entry.Name
		if entry.Type == client.EntryTypeFolder {
			r.removeTree(t, key)
			_ = r.conn.RemoveDir(key) // An emptied fixture directory is optional.
			continue
		}
		r.remove(t, key)
	}
}

// resetQuota empties the quota account without touching its ownership marker, so
// each quota scenario starts from the account's real free-file budget.
func (w *worker) resetQuota(t *testing.T) {
	t.Helper()
	w.rawQuota(t).removeTree(t, instanceID)
}
