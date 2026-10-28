package replication

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/flyingEirc/Intern/internal/artifact"
	physicalstore "github.com/flyingEirc/Intern/internal/physical/store"
	"github.com/flyingEirc/Intern/internal/pipeline"
	"github.com/flyingEirc/Intern/internal/replication/internal/replsql"
	"github.com/flyingEirc/Intern/internal/storage/ftp"
	"github.com/flyingEirc/Intern/internal/storage/local"
)

const (
	engineSourceID    = "source-a"
	engineRepoID      = "ftp-primary"
	engineLocalRepoID = "local-primary"
	// engineOtherLocalRepoID is a second local repository behind the same replica
	// repository, which is the shared-FTP shape.
	engineOtherLocalRepoID = "local-secondary"
	engineInstanceID       = "instance-1"
	engineServerUUID       = "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
	engineServerVer        = "8.4.11"
	engineToolVer          = "8.4.0-7"
	engineOtherSource      = "source-b"
	engineObjectPrefix     = engineInstanceID + "/"
)

// engineFixture is a real control-plane database with the physical and
// replication migrations, a real local object repository, a real KEK, and a
// real zstd/Tink artifact per published backup.
type engineFixture struct {
	t      *testing.T
	db     *sql.DB
	store  *physicalstore.Store
	repo   *local.Repository
	root   string
	work   string
	key    string
	now    time.Time
	serial int
	remote *fakeRemote
	r      *Replicator
}

func newEngine(t *testing.T) *engineFixture {
	t.Helper()
	dir := t.TempDir()
	f := &engineFixture{
		t: t, root: filepath.Join(dir, "objects"), work: filepath.Join(dir, "work"),
		key: filepath.Join(dir, "keys", "intern.key"),
		now: time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC),
	}
	for _, name := range []string{filepath.Dir(f.key), f.work} {
		if err := os.Mkdir(name, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := artifact.GenerateKey(f.key); err != nil {
		t.Fatal(err)
	}
	f.db = newControlDB(t, filepath.Join(dir, "intern.db"))
	if err := physicalstore.Migrate(t.Context(), f.db); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), f.db); err != nil {
		t.Fatal(err)
	}
	f.store = physicalstore.New(f.db)
	repository, err := local.Open(f.root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repository.Close() })
	f.repo = repository
	f.remote = newFakeRemote()
	f.r = f.replicator(nil)
	return f
}

// replicator binds the fixture clock and, when keyFile is empty, the fixture KEK.
func (f *engineFixture) replicator(keyFile *string) *Replicator {
	key := f.key
	if keyFile != nil {
		key = *keyFile
	}
	return &Replicator{
		DB: f.db, Store: f.store, Local: f.repo, Remote: f.remote,
		RepositoryID: engineRepoID, LocalRepositoryID: engineLocalRepoID,
		InstanceID: engineInstanceID,
		KeyFile:    key, WorkRoot: f.work,
		Policy: Policy{BaseDelay: time.Minute, MaxDelay: time.Hour, AlertAfter: 6 * time.Hour, MaxAttempts: 3},
		Now:    func() time.Time { return f.now },
	}
}

func (f *engineFixture) tick() { f.now = f.now.Add(time.Second) }

func newControlDB(t *testing.T, name string) *sql.DB {
	t.Helper()
	u := url.URL{Scheme: "file", Path: name}
	q := u.Query()
	for _, pragma := range []string{"journal_mode(WAL)", "synchronous(FULL)", "foreign_keys(1)", "busy_timeout(5000)"} {
		q.Add("_pragma", pragma)
	}
	q.Set("_txlock", "immediate")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

// backupRecord is one completed local backup of the fixture.
type backupRecord struct {
	id, chainID, objectPath string
	payloadSize             int64
	payloadDigest           string
	metadataSize            int64
	metadataDigest          string
}

// newBackup publishes one completed backup of a fresh or continuing chain using
// real artifact streams, exactly as the physical chain does.
func (f *engineFixture) newBackup(sourceID string, member int) backupRecord {
	f.t.Helper()
	return f.newBackupIn(f.repo, sourceID, member)
}

// newBackupIn is newBackup into one chosen local repository, so a test can place
// two repositories' objects behind one replica repository.
func (f *engineFixture) newBackupIn(repo *local.Repository, sourceID string, member int) backupRecord {
	f.t.Helper()
	kind := "incremental"
	if member == 0 {
		kind = "full"
	}
	plan, err := f.store.Plan(f.t.Context(), physicalstore.PlanRequest{
		SourceID: sourceID, ServerUUID: engineServerUUID, ServerVersion: engineServerVer,
		ToolVersion: engineToolVer, Kind: kind, MaxChainLength: 100,
	})
	if err != nil {
		f.t.Fatalf("plan: %v", err)
	}
	f.serial++
	backupID := fmt.Sprintf("backup-%d", f.serial)
	objectPath := path.Join(engineInstanceID, sourceID, plan.ChainID, fmt.Sprintf("%04d-%s", plan.Seq, backupID))
	if err = f.store.Begin(f.t.Context(), physicalstore.BeginRequest{
		Plan: plan, SourceID: sourceID, ServerUUID: engineServerUUID, ServerVersion: engineServerVer,
		ToolVersion: engineToolVer, InstanceID: engineInstanceID, RunID: backupID, BackupID: backupID,
		ObjectPath: objectPath, StagingToken: backupID,
		WorkDir: filepath.Join(f.work, "run-"+backupID), KeyID: "key-id", CreatedAt: f.now,
	}); err != nil {
		f.t.Fatalf("begin: %v", err)
	}
	record := f.publish(repo, objectPath, backupID, sourceID, plan.ChainID, plan.Seq)
	record.chainID = plan.ChainID
	f.tick()
	if err = f.store.Complete(f.t.Context(), physicalstore.Completion{
		BackupID: backupID, FromLSN: uint64(member * 100), ToLSN: uint64((member + 1) * 100),
		LastLSN: uint64((member + 1) * 100), CheckpointsRaw: checkpointBytes(member),
		InfoRaw: []byte("tool_name = xtrabackup\n"), PayloadSize: record.payloadSize,
		PayloadSHA256: record.payloadDigest, MetadataSize: record.metadataSize,
		MetadataSHA256: record.metadataDigest, CompletedAt: f.now,
	}); err != nil {
		f.t.Fatalf("complete: %v", err)
	}
	f.tick()
	return record
}

func checkpointBytes(member int) []byte {
	kind := "incremental"
	if member == 0 {
		kind = "full-backuped"
	}
	return fmt.Appendf(nil, "backup_type = %s\nfrom_lsn = %d\nto_lsn = %d\nlast_lsn = %d\n",
		kind, member*100, (member+1)*100, (member+1)*100)
}

// publish builds one chain-like object: both members are real zstd/Tink streams
// written through the artifact pipeline into the local staging directory, exactly
// as the physical chain publishes them.
func (f *engineFixture) publish(repo *local.Repository, objectPath, backupID, sourceID, chainID string, seq int) backupRecord {
	f.t.Helper()
	staging, err := repo.BeginOwned(objectPath, backupID)
	if err != nil {
		f.t.Fatalf("begin object: %v", err)
	}
	identity := identityManifest{
		Version: 1, Role: "xbstream", InstanceID: engineInstanceID, SourceID: sourceID,
		BackupID: backupID, ChainID: chainID, Seq: seq,
	}
	payload := f.writeArtifact(staging, memberPayload, memberEnvelope, payloadBytes(objectPath),
		marshalManifest(f.t, identity))
	toolIdentity := identity
	toolIdentity.Role = "tool-metadata"
	toolBody := marshalManifest(f.t, toolManifest{
		Identity: toolIdentity, PayloadSize: payload.size, PayloadSHA256: payload.digest,
	})
	metadata := f.writeArtifact(staging, memberMetadata, memberMetaEnvelope, bytesSource(toolBody), toolBody)
	if err = staging.Publish(f.t.Context()); err != nil {
		f.t.Fatalf("publish object: %v", err)
	}
	return backupRecord{
		id: backupID, objectPath: objectPath, payloadSize: payload.size, payloadDigest: payload.digest,
		metadataSize: metadata.size, metadataDigest: metadata.digest,
	}
}

// ciphertext is one published artifact's ciphertext size and digest.
type ciphertext struct {
	size   int64
	digest string
}

// writeArtifact runs one real artifact stream into a staging sink and writes its
// v1 envelope beside it. No stage is added by the caller: the pipeline only moves
// the ciphertext the artifact stream produced.
func (f *engineFixture) writeArtifact(staging *local.Staging, payloadName, envelopeName string, payload pipeline.Source, manifest []byte) ciphertext {
	f.t.Helper()
	stream, err := artifact.NewStream(artifact.Options{Compress: true, Encrypt: true, KeyFile: f.key}, manifest)
	if err != nil {
		f.t.Fatalf("new artifact stream: %v", err)
	}
	sink, err := staging.Create(payloadName)
	if err != nil {
		f.t.Fatalf("create %s: %v", payloadName, err)
	}
	if _, err = pipeline.Run(f.t.Context(), payload, stream.Stages(), sink); err != nil {
		f.t.Fatalf("write %s: %v", payloadName, err)
	}
	envelope, err := stream.Envelope()
	if err != nil {
		f.t.Fatalf("seal %s: %v", payloadName, err)
	}
	envelopeSink, err := staging.Create(envelopeName)
	if err != nil {
		f.t.Fatalf("create %s: %v", envelopeName, err)
	}
	if _, err = envelopeSink.Write(envelope); err != nil {
		f.t.Fatalf("write %s: %v", envelopeName, err)
	}
	if _, err = envelopeSink.Commit(f.t.Context()); err != nil {
		f.t.Fatalf("commit %s: %v", envelopeName, err)
	}
	return ciphertext{size: stream.Size(), digest: stream.SHA256()}
}

// fakeRemote is an in-memory remote repository behind the Remote interface. It
// models temporary parts, commits, and readback, and can inject failures.
type fakeRemote struct {
	mu                   sync.Mutex
	objects              map[string][]byte
	parts                map[string][]byte
	creates              map[string]int
	deny                 map[string]error
	onWrite              func(part string)
	uncertain            map[string]bool
	uncertainKeepsObject bool
	afterCommit          func(key string)
	cleanupErr           error
	failOpen             string
	failWrite            string
	commitSizeDelta      int64
	totalParts           int
}

func newFakeRemote() *fakeRemote {
	return &fakeRemote{
		objects: make(map[string][]byte), parts: make(map[string][]byte), creates: make(map[string]int),
		deny: make(map[string]error), uncertain: make(map[string]bool),
	}
}

func (f *fakeRemote) Create(_ context.Context, key string) (Sink, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for prefix, err := range f.deny {
		if strings.HasPrefix(key, prefix) {
			return nil, err
		}
	}
	if _, exists := f.objects[key]; exists {
		return nil, fmt.Errorf("remote object exists: %w", fs.ErrExist)
	}
	f.creates[key]++
	part := fmt.Sprintf(".intern-upload-%032x.part", f.totalParts)
	f.totalParts++
	f.parts[part] = nil
	return &fakeSink{remote: f, key: key, part: part}, nil
}

func (f *fakeRemote) Open(_ context.Context, key string, offset int64) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOpen != "" && key == f.failOpen {
		return nil, fmt.Errorf("remote read refused: %w", fs.ErrNotExist)
	}
	data, ok := f.objects[key]
	if !ok {
		return nil, fmt.Errorf("remote object absent: %w", fs.ErrNotExist)
	}
	if offset < 0 || offset > int64(len(data)) {
		return nil, fs.ErrInvalid
	}
	return io.NopCloser(bytes.NewReader(data[offset:])), nil
}

func (f *fakeRemote) Size(_ context.Context, key string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.objects[key]
	if !ok {
		return 0, fmt.Errorf("remote object absent: %w", fs.ErrNotExist)
	}
	return int64(len(data)), nil
}

func (f *fakeRemote) CleanupPart(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cleanupErr != nil {
		return f.cleanupErr
	}
	delete(f.parts, key)
	return nil
}

// notifyComplete fires the post-commit hook once all four members of one object
// directory are committed, so a test can corrupt the remote copy between the
// last commit and the readback.
func (f *fakeRemote) notifyComplete(key string) {
	dir := key[:strings.LastIndex(key, "/")]
	f.mu.Lock()
	present := 0
	for _, member := range memberOrder {
		if _, ok := f.objects[dir+"/"+member]; ok {
			present++
		}
	}
	hook := f.afterCommit
	f.mu.Unlock()
	if present == len(memberOrder) && hook != nil {
		hook(key)
	}
}

func (f *fakeRemote) object(key string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.objects[key]
	return data, ok
}

func (f *fakeRemote) put(key string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = data
}

func (f *fakeRemote) partCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.parts)
}

func (f *fakeRemote) createCount(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creates[key]
}

// fakeSink is one remote temporary object.
type fakeSink struct {
	remote *fakeRemote
	key    string
	part   string
	mu     sync.Mutex
	data   []byte
	state  int
}

func (s *fakeSink) TemporaryPath() string { return s.part }

func (s *fakeSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != 0 {
		return 0, fs.ErrClosed
	}
	if s.remote.onWrite != nil {
		s.remote.onWrite(s.part)
	}
	if s.remote.failWrite == s.key {
		return 0, fmt.Errorf("remote write refused: %w", fs.ErrPermission)
	}
	s.data = append(s.data, p...)
	s.remote.mu.Lock()
	s.remote.parts[s.part] = s.data
	s.remote.mu.Unlock()
	return len(p), nil
}

func (s *fakeSink) Commit(_ context.Context) (pipeline.Receipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != 0 {
		return pipeline.Receipt{}, fs.ErrClosed
	}
	s.remote.mu.Lock()
	uncertain := s.remote.uncertain[s.key]
	keeps := s.remote.uncertainKeepsObject
	delta := s.remote.commitSizeDelta
	if !uncertain || keeps {
		stored := append([]byte(nil), s.data...)
		if delta > 0 && len(stored) > int(delta) {
			stored = stored[:len(stored)-int(delta)]
		}
		s.remote.objects[s.key] = stored
	}
	delete(s.remote.parts, s.part)
	s.remote.mu.Unlock()
	if uncertain {
		s.state = 2
		return pipeline.Receipt{}, fmt.Errorf("remote commit uncertain: %w", ftp.ErrCommitUncertain)
	}
	s.state = 1
	s.remote.notifyComplete(s.key)
	return pipeline.Receipt{Path: s.key, Size: int64(len(s.data))}, nil
}

func (s *fakeSink) Abort() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == 1 {
		return nil
	}
	s.state = 2
	return s.remote.CleanupPart(context.Background(), s.part)
}

// bytesSource is a pipeline.Source over a byte slice, like the physical chain's
// manifest and metadata sources.
func bytesSource(data []byte) pipeline.Source { return &sliceSource{data: data} }

type sliceSource struct {
	data []byte
}

func (s *sliceSource) Run(_ context.Context, w io.Writer) error {
	_, err := w.Write(s.data)
	return err
}

func marshalManifest(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func payloadBytes(objectPath string) pipeline.Source {
	return bytesSource([]byte("xbstream-plaintext:" + objectPath))
}

func (f *engineFixture) memberStatuses(backupID string) map[string]string {
	f.t.Helper()
	rows, err := replsql.New(f.db).Members(f.t.Context(), replsql.MembersParams{BackupID: backupID, RepositoryID: engineRepoID})
	if err != nil {
		f.t.Fatal(err)
	}
	statuses := make(map[string]string, len(rows))
	for _, row := range rows {
		statuses[row.Member] = row.Status
	}
	return statuses
}

// failedSince reads the durable first-failure time, which is the alert clock.
func (f *engineFixture) failedSince(backupID string) *time.Time {
	f.t.Helper()
	var value sql.NullString
	if err := f.db.QueryRowContext(f.t.Context(),
		"SELECT failed_since FROM replicas WHERE backup_id = ? AND repository_id = ?", backupID, engineRepoID).Scan(&value); err != nil {
		f.t.Fatal(err)
	}
	if !value.Valid {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value.String)
	if err != nil {
		f.t.Fatal(err)
	}
	return &parsed
}

func (f *engineFixture) partRows() int {
	f.t.Helper()
	var count int
	if err := f.db.QueryRowContext(f.t.Context(), "SELECT COUNT(*) FROM replica_parts WHERE repository_id = ?", engineRepoID).Scan(&count); err != nil {
		f.t.Fatal(err)
	}
	return count
}

// advanceToDue moves the fixture clock to the item's next attempt so a retry is
// due without waiting for wall-clock time.
func (f *engineFixture) advanceToDue(backupID string) {
	f.t.Helper()
	replica, err := f.r.Replica(f.t.Context(), backupID)
	if err != nil {
		f.t.Fatal(err)
	}
	if replica.NextAttemptAt != nil && replica.NextAttemptAt.After(f.now) {
		f.now = *replica.NextAttemptAt
	}
}

func (f *engineFixture) mustStep() {
	f.t.Helper()
	processed, err := f.r.Step(f.t.Context())
	if !processed || err != nil {
		f.t.Fatalf("step = %v, %v", processed, err)
	}
}

// step runs one attempt and returns its outcome.
func (f *engineFixture) step() (bool, error) { return f.r.Step(f.t.Context()) }

// assertStatus requires one durable replica status, read straight from the
// table so the assertion does not depend on the engine's own accessor, then
// returns the engine's record for detailed checks.
func (f *engineFixture) assertStatus(backupID, want string) Replica {
	f.t.Helper()
	var status string
	if err := f.db.QueryRowContext(f.t.Context(),
		"SELECT status FROM replicas WHERE backup_id = ? AND repository_id = ?", backupID, engineRepoID).Scan(&status); err != nil {
		f.t.Fatalf("replica %s: %v", backupID, err)
	}
	if status != want {
		f.t.Fatalf("replica %s status = %q; want %q", backupID, status, want)
	}
	replica, err := f.r.Replica(f.t.Context(), backupID)
	if err != nil {
		f.t.Fatalf("replica %s: %v", backupID, err)
	}
	return replica
}

// assertFails requires an attempted item with a failure.
func (f *engineFixture) assertFails() error {
	f.t.Helper()
	processed, err := f.step()
	if !processed || err == nil {
		f.t.Fatalf("step = %v, %v; want an attempted failure", processed, err)
	}
	return err
}

// assertIdle requires that no item was attempted.
func (f *engineFixture) assertIdle() {
	f.t.Helper()
	if processed, err := f.step(); processed || err != nil {
		f.t.Fatalf("step = %v, %v; want no attempted item", processed, err)
	}
}

// assertVerified advances to the retry time and requires a verified replica.
func (f *engineFixture) assertVerified(backupID string) Replica {
	f.t.Helper()
	f.advanceToDue(backupID)
	f.mustStep()
	return f.assertStatus(backupID, StatusVerified)
}

// assertLocalIntact requires the completed row and the local payload to be
// unchanged by a failed replication attempt.
func (f *engineFixture) assertLocalIntact(record backupRecord) {
	f.t.Helper()
	backup, err := f.store.Backup(f.t.Context(), record.id)
	if err != nil {
		f.t.Fatalf("local backup row: %v", err)
	}
	if backup.Status != "completed" || !backup.LocalPresent || backup.PayloadSHA256 != record.payloadDigest {
		f.t.Fatalf("local row changed: %+v", backup)
	}
	if _, err = os.Stat(f.localFile(record.objectPath, memberPayload)); err != nil {
		f.t.Fatalf("local payload removed: %v", err)
	}
}

func (f *engineFixture) assertNoRemote(record backupRecord) {
	f.t.Helper()
	if _, ok := f.remote.object(record.objectPath + "/" + memberPayload); ok {
		f.t.Fatal("payload was published remotely")
	}
}

func (f *engineFixture) localFile(objectPath, member string) string {
	return filepath.Join(f.root, filepath.FromSlash(objectPath), member)
}
