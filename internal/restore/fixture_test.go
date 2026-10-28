package restore

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/flyingEirc/Intern/internal/artifact"
	"github.com/flyingEirc/Intern/internal/physical"
	"github.com/flyingEirc/Intern/internal/physical/store"
	"github.com/flyingEirc/Intern/internal/replication"
	"github.com/flyingEirc/Intern/internal/storage/ftp"
	"github.com/flyingEirc/Intern/internal/storage/local"
)

const (
	fixtureInstance = "instance-a"
	fixtureSource   = "source-a"
	fixtureChain    = "chain-0001"
	fixtureRepoID   = "ftp-1"
	fullLSN         = uint64(100)
	inc1LSN         = uint64(200)
	inc2LSN         = uint64(300)
)

// planFixture is one real migrated control-plane database, a real private local
// repository and two generated KEKs.
type planFixture struct {
	t           *testing.T
	db          *sql.DB
	store       *store.Store
	replication *replication.Catalog
	repoRoot    string
	repo        *local.Repository
	keyFile     string
	wrongKey    string
	workRoot    string
	exportRoot  string
	xtrabackup  string
	xbstream    string
	backups     []fixtureBackup
	remote      *fakeRemote
}

type fixtureBackup struct {
	ID           string
	ParentID     string
	Seq          int
	Kind         string
	ObjectPath   string
	PayloadSize  int64
	MetadataSize int64
	FromLSN      uint64
	ToLSN        uint64
}

func (b fixtureBackup) payloadDigest() string { return strings.Repeat("a", 48) + b.ID[len(b.ID)-16:] }

func (b fixtureBackup) metadataDigest() string { return strings.Repeat("b", 48) + b.ID[len(b.ID)-16:] }

func openFixtureDB(t *testing.T, filename string) *sql.DB {
	t.Helper()
	u := url.URL{Scheme: "file", Path: filename}
	q := u.Query()
	for _, pragma := range []string{"journal_mode(WAL)", "synchronous(FULL)", "foreign_keys(1)", "busy_timeout(5000)"} {
		q.Add("_pragma", pragma)
	}
	q.Set("_txlock", "immediate")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	mustNoError(t, err)
	db.SetMaxOpenConns(1)
	mustNoError(t, store.Migrate(t.Context(), db))
	mustNoError(t, replication.Migrate(t.Context(), db))
	t.Cleanup(func() { mustNoError(t, db.Close()) })
	return db
}

func mustNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func newPlanFixture(t *testing.T) *planFixture {
	t.Helper()
	root := t.TempDir()
	fixture := &planFixture{
		t: t, repoRoot: filepath.Join(root, "repo"), workRoot: filepath.Join(root, "work"),
		exportRoot: filepath.Join(root, "export"), keyFile: filepath.Join(root, "keys", "intern.key"),
		wrongKey: filepath.Join(root, "keys", "wrong.key"),
	}
	for _, dir := range []string{fixture.repoRoot, fixture.workRoot, fixture.exportRoot, filepath.Dir(fixture.keyFile)} {
		mustNoError(t, os.Mkdir(dir, 0o700))
	}
	mustNoError(t, artifact.GenerateKey(fixture.keyFile))
	mustNoError(t, artifact.GenerateKey(fixture.wrongKey))
	fixture.xtrabackup = writeExecutable(t, root, "xtrabackup")
	fixture.xbstream = writeExecutable(t, root, "xbstream")
	repo, err := local.Open(fixture.repoRoot)
	mustNoError(t, err)
	t.Cleanup(func() { mustNoError(t, repo.Close()) })
	fixture.repo = repo
	fixture.db = openFixtureDB(t, filepath.Join(root, "intern.db"))
	fixture.store = store.New(fixture.db)
	fixture.replication = replication.NewCatalog(fixture.db)
	keyID, err := artifact.KeyID(fixture.keyFile)
	mustNoError(t, err)
	mustNoError(t, insertFixtureChain(fixture, keyID))
	fixture.remote = newFakeRemote(fixture)
	return fixture
}

func writeExecutable(t *testing.T, root, name string) string {
	t.Helper()
	path := filepath.Join(root, "bin", name)
	mustNoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	mustNoError(t, os.WriteFile(path, []byte("#!/bin/sh\nexit 1\n"), 0o500))
	return path
}

func insertFixtureChain(fixture *planFixture, keyID string) error {
	_, err := fixture.db.ExecContext(context.Background(),
		`INSERT INTO chains (id, source_id, server_uuid, server_version, tool_version, started_at) VALUES (?, ?, ?, ?, ?, ?)`,
		fixtureChain, fixtureSource, "8f0f5b3f-3b57-4b0f-9f39-0a3f6a1b2c3d", "8.4.11", "8.4.0-7", "2026-01-01T00:00:00.000000000Z")
	if err != nil {
		return err
	}
	sizes := [][2]int64{{1024, 256}, {512, 256}, {768, 256}}
	parent := ""
	for index := range 3 {
		backup := fixtureBackup{
			ID: seedID(index), Seq: index + 1, Kind: "physical-incremental",
			ObjectPath:  fixtureInstance + "/" + fixtureSource + "/" + fixtureChain + "/000" + string(rune('1'+index)) + "-" + seedID(index),
			PayloadSize: sizes[index][0], MetadataSize: sizes[index][1],
			FromLSN: fullLSN * uint64(index), ToLSN: fullLSN * uint64(index+1),
		}
		if index == 0 {
			backup.Kind, backup.FromLSN, backup.ToLSN = "physical-full", 0, fullLSN
		}
		backup.ParentID = parent
		if _, err = fixture.db.ExecContext(context.Background(),
			`INSERT INTO backups (id, chain_id, seq, parent_id, kind, status, instance_id, run_id, from_lsn, to_lsn, last_lsn,
			 checkpoints, xtrabackup_info, object_path, payload_size, payload_sha256, metadata_size, metadata_sha256, key_id,
			 local_present, created_at, completed_at)
			 VALUES (?, ?, ?, ?, ?, 'completed', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
			backup.ID, fixtureChain, backup.Seq, nullText(backup.ParentID), backup.Kind, fixtureInstance, "run-"+backup.ID,
			uintText(backup.FromLSN), uintText(backup.ToLSN), uintText(backup.ToLSN),
			[]byte("backup_type=incremental\nfrom_lsn="+uintText(backup.FromLSN)+"\nto_lsn="+uintText(backup.ToLSN)+"\nlast_lsn="+uintText(backup.ToLSN)+"\n"),
			[]byte("tool_name=xtrabackup\n"), backup.ObjectPath, backup.PayloadSize, backup.payloadDigest(),
			backup.MetadataSize, backup.metadataDigest(), keyID,
			"2026-01-01T00:00:0"+string(rune('0'+index))+".000000000Z", "2026-01-01T00:00:0"+string(rune('1'+index))+".000000000Z",
		); err != nil {
			return err
		}
		fixture.backups = append(fixture.backups, backup)
		parent = backup.ID
		writeLocalMembers(fixture.t, fixture.repoRoot, backup)
		if err = insertReplica(fixture.db, backup, "verified"); err != nil {
			return err
		}
	}
	return nil
}

func seedID(index int) string {
	raw := []byte{byte(index + 1)}
	return "0123456789abcdef0123456789abcdef"[:30] + hex.EncodeToString(raw)
}

func nullText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func uintText(value uint64) string { return strconv.FormatUint(value, 10) }

func writeLocalMembers(t *testing.T, root string, backup fixtureBackup) {
	t.Helper()
	dir := filepath.Join(root, filepath.FromSlash(backup.ObjectPath))
	mustNoError(t, os.MkdirAll(dir, 0o700))
	for name, body := range map[string][]byte{
		memberPayload:      repeated('p', backup.PayloadSize),
		memberMetadata:     repeated('m', backup.MetadataSize),
		memberEnvelope:     repeated('e', 100),
		memberMetaEnvelope: repeated('n', 120),
	} {
		mustNoError(t, os.WriteFile(filepath.Join(dir, name), body, 0o600))
	}
}

func repeated(value byte, size int64) []byte {
	return []byte(strings.Repeat(string(value), int(size)))
}

func insertReplica(db *sql.DB, backup fixtureBackup, status string) error {
	_, err := db.ExecContext(context.Background(),
		`INSERT INTO replicas (backup_id, repository_id, idem_key, status, attempts, permanent, next_attempt_at, uploaded_at, verified_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, 0, 0, ?, ?, ?, ?, ?)`,
		backup.ID, fixtureRepoID, "replicate:"+backup.ID+":"+fixtureRepoID, status,
		"2026-01-01T00:00:00.000000000Z", "2026-01-01T00:00:00.000000000Z", "2026-01-01T00:00:00.000000000Z",
		"2026-01-01T00:00:00.000000000Z", "2026-01-01T00:00:00.000000000Z")
	if err != nil {
		return err
	}
	for member, digest := range map[string]string{
		memberPayload: backup.payloadDigest(), memberMetadata: backup.metadataDigest(),
		memberEnvelope: strings.Repeat("c", 64), memberMetaEnvelope: strings.Repeat("d", 64),
	} {
		size := replicaSize(backup, member)
		if _, err = db.ExecContext(context.Background(),
			`INSERT INTO replica_members (backup_id, repository_id, member, status, digest, size, updated_at)
			 VALUES (?, ?, ?, 'verified', ?, ?, ?)`,
			backup.ID, fixtureRepoID, member, digest, size, "2026-01-01T00:00:00.000000000Z",
		); err != nil {
			return err
		}
	}
	return nil
}

func replicaSize(backup fixtureBackup, member string) int64 {
	switch member {
	case memberPayload:
		return backup.PayloadSize
	case memberMetadata:
		return backup.MetadataSize
	case memberEnvelope:
		return 100
	default:
		return 120
	}
}

// fakeRemote implements remoteReader without a network and records every call.
// It has no mutating operation, so a plan that only sees it cannot write.
type fakeRemote struct {
	sizes  map[string]int64
	data   map[string][]byte
	opens  int
	closed int
}

func newFakeRemote(fixture *planFixture) *fakeRemote {
	remote := &fakeRemote{sizes: map[string]int64{}, data: map[string][]byte{}}
	for _, backup := range fixture.backups {
		for _, member := range memberOrder {
			key := backup.ObjectPath + "/" + member
			remote.sizes[key] = replicaSize(backup, member)
			remote.data[key] = repeated('r', replicaSize(backup, member))
		}
	}
	return remote
}

func (f *fakeRemote) Size(_ context.Context, key string) (int64, error) {
	size, ok := f.sizes[key]
	if !ok {
		return 0, fs.ErrNotExist
	}
	return size, nil
}

func (f *fakeRemote) Open(_ context.Context, key string, offset int64) (io.ReadCloser, error) {
	f.opens++
	data, ok := f.data[key]
	if !ok {
		return nil, fs.ErrNotExist
	}
	if offset > int64(len(data)) {
		return nil, errors.New("offset past end")
	}
	return io.NopCloser(strings.NewReader(string(data[offset:]))), nil
}

func (f *fakeRemote) Close() error { f.closed++; return nil }

type fakeLocator struct {
	local    *local.Repository
	replicas []ReplicaRef
}

func (l *fakeLocator) Local(context.Context, string) (*local.Repository, error) {
	return l.local, nil
}

func (l *fakeLocator) Replicas(context.Context, string) ([]ReplicaRef, error) {
	return l.replicas, nil
}

func (f *planFixture) options() Options {
	return Options{
		Store: f.store, Replication: f.replication,
		Locator: &fakeLocator{local: f.repo, replicas: []ReplicaRef{{RepositoryID: fixtureRepoID, Open: f.refOpen}}},
		KeyFile: f.keyFile, WorkRoot: f.workRoot, ExportRoot: f.exportRoot,
		XtrabackupPath: f.xtrabackup, XbstreamPath: f.xbstream, ExpansionFactor: 8,
		probeTool: func(context.Context, string) (physical.ToolVersion, error) {
			return physical.ToolVersion{Version: "8.4.0-7"}, nil
		},
		remote: func(context.Context, ReplicaRef) (remoteReader, error) { return f.remote, nil },
	}
}

func (f *planFixture) refOpen(context.Context) (*ftp.Repository, error) {
	return nil, errors.New("the fake remote never opens a real FTP repository")
}

func (f *planFixture) exec(query string, args ...any) {
	f.t.Helper()
	_, err := f.db.ExecContext(context.Background(), query, args...)
	mustNoError(f.t, err)
}

func (f *planFixture) backup(index int) fixtureBackup { return f.backups[index] }
