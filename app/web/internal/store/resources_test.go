package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flyingEirc/Intern/internal/artifact"
	"github.com/flyingEirc/Intern/internal/credential"
	"github.com/flyingEirc/Intern/internal/pgtest"
)

func testSealer(t *testing.T) *credential.Sealer {
	t.Helper()
	key := filepath.Join(t.TempDir(), "kek.json")
	if err := artifact.GenerateKey(key); err != nil {
		t.Fatal(err)
	}
	sealer, err := credential.Open(key)
	if err != nil {
		t.Fatal(err)
	}
	return sealer
}

// createTestSource seals with a fresh KEK, so it is only for rows whose secret is
// never opened again in the same test.
func createTestSource(t *testing.T, s *Store, name, secret string) Source {
	t.Helper()
	return createTestSourceWith(t, s, testSealer(t), name, secret)
}

func createTestSourceWith(t *testing.T, s *Store, sealer SecretSealer, name, secret string) Source {
	t.Helper()
	source, err := s.CreateSource(t.Context(), SourceInput{
		Name: name, Socket: "/run/mysqld/mysqld.sock", Datadir: "/var/lib/mysql",
		Username: "backup", Secret: []byte(secret),
	}, sealer, SystemAudit("sources.create", AuditDetail{}))
	if err != nil {
		t.Fatal(err)
	}
	return source
}

// TestStoredCiphertextIsBoundToItsRowAndField proves the AAD contract against
// the real database: swapping ciphertext between rows or fields is refused, and a
// different KEK cannot open the value.
func TestStoredCiphertextIsBoundToItsRowAndField(t *testing.T) {
	dir := pgtest.New(t)
	s := openTestStore(t, dir)
	sealer := testSealer(t)
	first := createTestSourceWith(t, s, sealer, "first", "{{SECRET_first_source}}")
	second := createTestSourceWith(t, s, sealer, "second", "{{SECRET_second_source}}")
	firstCiphertext := rawCiphertext(t, dir, "sources", first.ID)
	secondCiphertext := rawCiphertext(t, dir, "sources", second.ID)
	assertDistinctCiphertext(t, firstCiphertext, secondCiphertext)
	opened, err := s.SourceSecret(t.Context(), first.ID, sealer)
	if err != nil || string(opened) != "{{SECRET_first_source}}" {
		t.Fatalf("first secret = %q %v", opened, err)
	}
	assertRowSwapRefused(t, s, sealer, dir, second.ID, firstCiphertext, secondCiphertext)
	repository := createTestFTPRepository(t, s, sealer)
	assertFieldSwapRefused(t, s, sealer, dir, second.ID, rawCiphertext(t, dir, "repositories", repository.ID))
	assertWrongKEKRefused(t, s, first.ID, repository.ID)
	if opened, err = s.RepositorySecret(t.Context(), repository.ID, sealer); err != nil || string(opened) != "{{SECRET_ftp_repository}}" {
		t.Fatalf("repository secret = %q %v", opened, err)
	}
}

func assertDistinctCiphertext(t *testing.T, first, second []byte) {
	t.Helper()
	if string(first) == string(second) {
		t.Fatal("two rows share a ciphertext")
	}
	if strings.Contains(string(first), "{{SECRET_first_source}}") {
		t.Fatal("stored ciphertext contains the plaintext")
	}
}

func createTestFTPRepository(t *testing.T, s *Store, sealer SecretSealer) Repository {
	t.Helper()
	return mustRepository(t, s, sealer, RepositoryInput{
		Name: "ftp", Kind: RepositoryFTP, TLSMode: TLSModePlain,
		FTP:    &FTPRepositoryConfig{Host: "127.0.0.1", Port: 21, BasePath: "/b", Username: "u"},
		Secret: []byte("{{SECRET_ftp_repository}}"),
	})
}

// assertRowSwapRefused moves one row's ciphertext onto another row.
func assertRowSwapRefused(t *testing.T, s *Store, sealer SecretSealer, dir, target string, foreign, original []byte) {
	t.Helper()
	writeCiphertext(t, dir, "sources", target, foreign)
	if _, err := s.SourceSecret(t.Context(), target, sealer); err == nil {
		t.Fatal("swapped ciphertext decrypted for another row")
	}
	writeCiphertext(t, dir, "sources", target, original)
}

// assertFieldSwapRefused moves a repository password onto a source row.
func assertFieldSwapRefused(t *testing.T, s *Store, sealer SecretSealer, dir, target string, foreign []byte) {
	t.Helper()
	writeCiphertext(t, dir, "sources", target, foreign)
	if _, err := s.SourceSecret(t.Context(), target, sealer); err == nil {
		t.Fatal("a column-foreign ciphertext decrypted")
	}
}

func assertWrongKEKRefused(t *testing.T, s *Store, sourceID, repositoryID string) {
	t.Helper()
	other := testSealer(t)
	if _, err := s.SourceSecret(t.Context(), sourceID, other); err == nil {
		t.Fatal("another KEK opened the stored secret")
	}
	if _, err := s.RepositorySecret(t.Context(), repositoryID, other); err == nil {
		t.Fatal("another KEK opened the repository secret")
	}
}

// TestSecretRotationReplacesCiphertextInPlace covers the rotation contract: a new
// password yields new ciphertext for the same row and the old one no longer opens.
func TestSecretRotationReplacesCiphertextInPlace(t *testing.T) {
	dir := pgtest.New(t)
	s := openTestStore(t, dir)
	sealer := testSealer(t)
	source := createTestSourceWith(t, s, sealer, "source", "{{SECRET_before_rotation}}")
	before := string(rawCiphertext(t, dir, "sources", source.ID))
	updated, err := s.UpdateSource(t.Context(), source.ID, SourceInput{
		Name: source.Name, Socket: source.Socket, Datadir: source.Datadir,
		Username: source.Username, Secret: []byte("{{SECRET_after_rotation}}"),
	}, sealer, SystemAudit("sources.update", AuditDetail{}))
	if err != nil {
		t.Fatal(err)
	}
	after := string(rawCiphertext(t, dir, "sources", source.ID))
	if before == after {
		t.Fatal("rotation kept the old ciphertext")
	}
	opened, err := s.SourceSecret(t.Context(), updated.ID, sealer)
	if err != nil || string(opened) != "{{SECRET_after_rotation}}" {
		t.Fatalf("rotated secret = %q %v", opened, err)
	}
	// The AAD does not change with rotation, so the previous ciphertext remains a
	// valid sealed value for this row in isolation. What rotation guarantees is
	// that it is no longer stored: the row now holds the new value only.
	// An update without a password keeps the stored ciphertext.
	if _, err = s.UpdateSource(t.Context(), source.ID, SourceInput{
		Name: "renamed", Socket: source.Socket, Datadir: source.Datadir, Username: source.Username,
	}, sealer, SystemAudit("sources.update", AuditDetail{})); err != nil {
		t.Fatal(err)
	}
	if string(rawCiphertext(t, dir, "sources", source.ID)) != after {
		t.Fatal("an omitted password rewrote the ciphertext")
	}
}

func TestResourceNamesAndKindsAreEnforcedBySchema(t *testing.T) {
	dir := pgtest.New(t)
	s := openTestStore(t, dir)
	sealer := testSealer(t)
	root := filepath.Join(t.TempDir(), "repository")
	local := RepositoryInput{Name: "local", Kind: RepositoryLocal, TLSMode: TLSModeNone, Root: root}
	if _, err := s.CreateRepository(t.Context(), local, sealer, SystemAudit("repositories.create", AuditDetail{})); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRepository(t.Context(), local, sealer, SystemAudit("repositories.create", AuditDetail{})); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate name = %v", err)
	}
	if _, err := s.CreateRepository(t.Context(), RepositoryInput{
		Name: "local-plain", Kind: RepositoryLocal, TLSMode: TLSModePlain, Root: root,
	}, sealer, SystemAudit("repositories.create", AuditDetail{})); err == nil {
		t.Fatal("a local repository accepted a TLS mode")
	}
	if _, err := s.CreateRepository(t.Context(), RepositoryInput{
		Name: "ftp-no-secret", Kind: RepositoryFTP, TLSMode: TLSModePlain,
		FTP: &FTPRepositoryConfig{Host: "127.0.0.1", Port: 21, BasePath: "/b", Username: "u"},
	}, sealer, SystemAudit("repositories.create", AuditDetail{})); err == nil {
		t.Fatal("an FTP repository was created without a secret")
	}
	source := createTestSourceWith(t, s, sealer, "source", "{{SECRET_unique_source}}")
	if _, err := s.CreateSource(t.Context(), SourceInput{
		Name: "SOURCE", Socket: source.Socket, Datadir: source.Datadir, Username: source.Username, Secret: []byte("x"),
	}, sealer, SystemAudit("sources.create", AuditDetail{})); !errors.Is(err, ErrConflict) {
		t.Fatalf("case-insensitive duplicate = %v", err)
	}
}

// policyFixture holds the rows one policy test needs plus the store under test.
type policyFixture struct {
	store  *Store
	local  Repository
	ftp    Repository
	source Source
}

func newPolicyFixture(t *testing.T) policyFixture {
	t.Helper()
	s := openTestStore(t, pgtest.New(t))
	sealer := testSealer(t)
	return policyFixture{
		store: s,
		local: mustRepository(t, s, sealer, RepositoryInput{
			Name: "local", Kind: RepositoryLocal, TLSMode: TLSModeNone, Root: filepath.Join(t.TempDir(), "repository"),
		}),
		ftp: mustRepository(t, s, sealer, RepositoryInput{
			Name: "ftp", Kind: RepositoryFTP, TLSMode: TLSModePlain,
			FTP: &FTPRepositoryConfig{Host: "127.0.0.1", Port: 21, BasePath: "/b", Username: "u"}, Secret: []byte("{{SECRET_ftp}}"),
		}),
		source: createTestSourceWith(t, s, sealer, "source", "{{SECRET_policy_source}}"),
	}
}

func (f policyFixture) create(t *testing.T) Policy {
	t.Helper()
	policy, err := f.store.CreatePolicy(t.Context(), PolicyInput{
		Name: "policy", SourceID: f.source.ID, LocalRepositoryID: f.local.ID, ReplicaRepositoryID: &f.ftp.ID,
		FullCron: "0 2 * * *", IncrementalCron: "0 * * * *", Timezone: "UTC",
	}, SystemAudit("policies.create", AuditDetail{}))
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func TestPolicyReferencesBlockDeletion(t *testing.T) {
	f := newPolicyFixture(t)
	policy := f.create(t)
	if policy.Enabled || policy.ReplicaRepositoryID == nil || *policy.ReplicaRepositoryID != f.ftp.ID {
		t.Fatalf("policy = %+v", policy)
	}
	if err := f.store.DeleteRepository(t.Context(), f.local.ID, SystemAudit("repositories.delete", AuditDetail{})); !errors.Is(err, ErrReferenced) {
		t.Fatalf("referenced repository delete = %v", err)
	}
	if err := f.store.DeleteSource(t.Context(), f.source.ID, SystemAudit("sources.delete", AuditDetail{})); !errors.Is(err, ErrReferenced) {
		t.Fatalf("referenced source delete = %v", err)
	}
}

func TestPolicyEnableRequiresASuccessfulPreflightRecord(t *testing.T) {
	f := newPolicyFixture(t)
	policy := f.create(t)
	if _, err := f.store.EnablePolicy(t.Context(), policy.ID, true, nil, SystemAudit("policies.enable", AuditDetail{})); err == nil {
		t.Fatal("enable without a preflight record succeeded")
	}
	failed := PreflightRecord{OK: false, Checks: []string{}, Warnings: []PreflightWarning{}}
	if _, err := f.store.EnablePolicy(t.Context(), policy.ID, true, &failed, SystemAudit("policies.enable", AuditDetail{})); err == nil {
		t.Fatal("enable with a failed preflight record succeeded")
	}
	record := PreflightRecord{OK: true, Checks: []string{"identity"}, Warnings: []PreflightWarning{}}
	enabled, err := f.store.EnablePolicy(t.Context(), policy.ID, true, &record, SystemAudit("policies.enable", AuditDetail{}))
	if err != nil || !enabled.Enabled || enabled.LastPreflight == nil {
		t.Fatalf("enable = %+v %v", enabled, err)
	}
}

// publishP8State inserts one completed physical backup and the completed run
// that published it, with the run's claim snapshot naming one (local, replica)
// pair, plus the queued replica row the pair holds for that backup.
func publishP8State(t *testing.T, s *Store, policyID, sourceID, localID, replicaID, backupID string) {
	t.Helper()
	now := ScheduleStamp(time.Now())
	statements := []string{
		`INSERT INTO chains (id, source_id, server_uuid, server_version, tool_version, started_at)
		 VALUES ('chain-p8', $1, 'server-uuid', '8.4.11', '8.4.0-7', $2)`,
		`INSERT INTO backups (id, chain_id, seq, kind, status, instance_id, run_id, object_path, key_id,
		                      local_present, created_at, completed_at)
		 VALUES ($1, 'chain-p8', 1, 'physical-full', 'completed', 'instance-p8', $2, $3, 'key', 1, $4, $5)`,
		`INSERT INTO runs (id, policy_id, source_id, kind, "trigger", idempotency_key, request_hash, status,
		                   scheduled_at, created_at, started_at, finished_at, backup_id, chain_id, payload_size,
		                   local_repository_id, replica_repository_id)
		 VALUES ('run-p8', $1, $2, 'full', 'cron', 'run-p8-key', 'hash', 'completed', $3, $4, $5, $6, $7, 'chain-p8',
		         512, $8, $9)`,
		`INSERT INTO replicas (backup_id, repository_id, local_repository_id, idem_key, status,
		                       next_attempt_at, created_at, updated_at)
		 VALUES ($1, $2, $3, 'replicate:run-p8', 'pending', $4, $5, $6)`,
	}
	arguments := [][]any{
		{sourceID, now},
		{backupID, backupID, "instance-p8/" + sourceID + "/chain-p8/" + backupID, now, now},
		{policyID, sourceID, now, now, now, now, backupID, localID, replicaID},
		{backupID, replicaID, localID, now, now, now},
	}
	for i, statement := range statements {
		if _, err := s.DB().ExecContext(t.Context(), statement, arguments[i]...); err != nil {
			t.Fatal(err)
		}
	}
}

// TestRunSnapshotAndReplicaReferencesBlockRepositoryDeletion is P8: a
// repository a run was claimed with stays referenced through the run's
// snapshot — locally as the producing repository, remotely as the replica — and
// a queued copy keeps its local repository referenced even after the run rows
// are gone, because replication resolves the pair from those rows.
func TestRunSnapshotAndReplicaReferencesBlockRepositoryDeletion(t *testing.T) {
	f := newPolicyFixture(t)
	policy := f.create(t)
	// Repositories the policy can be moved to, so its own references are out of
	// the way when the test isolates the run snapshot and the queued copy.
	spare := mustRepository(t, f.store, testSealer(t), RepositoryInput{
		Name: "spare", Kind: RepositoryLocal, TLSMode: TLSModeNone, Root: filepath.Join(t.TempDir(), "spare"),
	})
	other := mustRepository(t, f.store, testSealer(t), RepositoryInput{
		Name: "other", Kind: RepositoryFTP, TLSMode: TLSModePlain,
		FTP: &FTPRepositoryConfig{Host: "127.0.0.1", Port: 21, BasePath: "/b", Username: "u"}, Secret: []byte("{{SECRET_other_ftp}}"),
	})
	publishP8State(t, f.store, policy.ID, f.source.ID, f.local.ID, f.ftp.ID, "backup-p8")
	movePolicyOff(t, f.store, policy, spare.ID, other.ID)

	assertDeleteRefused(t, f.store, f.local.ID, "producing repository")
	assertDeleteRefused(t, f.store, f.ftp.ID, "replica repository")

	// The queued copy is gone: the replica repository stays referenced only by
	// the run's snapshot column.
	clearReplicaRows(t, f.store)
	assertDeleteRefused(t, f.store, f.ftp.ID, "replica repository after the copy row went")

	// The copy now sits on another repository, so only its local binding still
	// names the producing repository. With the run rows gone the snapshot
	// references disappear with them, and the replica repository is deletable.
	queueCopyOn(t, f.store, other.ID, f.local.ID)
	clearRunRows(t, f.store)
	assertDeleteSucceeds(t, f.store, f.ftp.ID, "replica repository without run rows")
	assertDeleteRefused(t, f.store, f.local.ID, "producing repository bound by the copy row")

	// Without the queued copy nothing references either side any more.
	clearReplicaRows(t, f.store)
	assertDeleteSucceeds(t, f.store, f.local.ID, "producing repository without references")
}

// movePolicyOff re-points one policy at repositories the test is not deleting,
// through the same store call the PUT handler makes.
func movePolicyOff(t *testing.T, s *Store, policy Policy, localID, replicaID string) {
	t.Helper()
	if _, err := s.UpdatePolicy(t.Context(), policy.ID, PolicyInput{
		Name: policy.Name, SourceID: policy.SourceID, LocalRepositoryID: localID, ReplicaRepositoryID: &replicaID,
		FullCron: "0 2 * * *", IncrementalCron: "0 * * * *", Timezone: "UTC",
	}, SystemAudit("policies.update", AuditDetail{})); err != nil {
		t.Fatal(err)
	}
}

func assertDeleteRefused(t *testing.T, s *Store, id, what string) {
	t.Helper()
	if err := s.DeleteRepository(t.Context(), id, SystemAudit("repositories.delete", AuditDetail{})); !errors.Is(err, ErrReferenced) {
		t.Fatalf("%s delete = %v, want ErrReferenced", what, err)
	}
}

func assertDeleteSucceeds(t *testing.T, s *Store, id, what string) {
	t.Helper()
	if err := s.DeleteRepository(t.Context(), id, SystemAudit("repositories.delete", AuditDetail{})); err != nil {
		t.Fatalf("%s delete = %v, want success", what, err)
	}
}

func clearReplicaRows(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.DB().ExecContext(t.Context(), `DELETE FROM replicas`); err != nil {
		t.Fatal(err)
	}
}

func clearRunRows(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.DB().ExecContext(t.Context(), `DELETE FROM runs`); err != nil {
		t.Fatal(err)
	}
}

func queueCopyOn(t *testing.T, s *Store, repositoryID, localRepositoryID string) {
	t.Helper()
	if _, err := s.DB().ExecContext(t.Context(), `INSERT INTO replicas
(backup_id, repository_id, local_repository_id, idem_key, status, next_attempt_at, created_at, updated_at)
VALUES ('backup-p8', $1, $2, 'replicate:run-p8', 'pending', $3, $4, $5)`,
		repositoryID, localRepositoryID, ScheduleStamp(time.Now()), ScheduleStamp(time.Now()), ScheduleStamp(time.Now())); err != nil {
		t.Fatal(err)
	}
}

func TestDeletedPolicyReleasesItsReferences(t *testing.T) {
	f := newPolicyFixture(t)
	policy := f.create(t)
	if err := f.store.DeletePolicy(t.Context(), policy.ID, SystemAudit("policies.delete", AuditDetail{})); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DeleteSource(t.Context(), f.source.ID, SystemAudit("sources.delete", AuditDetail{})); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{f.local.ID, f.ftp.ID} {
		if err := f.store.DeleteRepository(t.Context(), id, SystemAudit("repositories.delete", AuditDetail{})); err != nil {
			t.Fatalf("delete repository %s: %v", id, err)
		}
	}
	if _, err := f.store.Repository(t.Context(), f.local.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted repository = %v", err)
	}
}

func TestResourcePaginationIsBounded(t *testing.T) {
	s := openTestStore(t, pgtest.New(t))
	for _, name := range []string{"a", "b", "c"} {
		createTestSource(t, s, name, "{{SECRET_"+name+"}}")
	}
	first, err := s.Sources(t.Context(), "", 2)
	if err != nil || len(first) != 2 {
		t.Fatalf("page = %d %v", len(first), err)
	}
	next, err := s.Sources(t.Context(), first[len(first)-1].ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(next) != 1 || next[0].ID >= first[1].ID {
		t.Fatalf("cursor page = %+v", next)
	}
}

func mustRepository(t *testing.T, s *Store, sealer SecretSealer, in RepositoryInput) Repository {
	t.Helper()
	repository, err := s.CreateRepository(t.Context(), in, sealer, SystemAudit("repositories.create", AuditDetail{}))
	if err != nil {
		t.Fatal(err)
	}
	return repository
}

func rawCiphertext(t *testing.T, dsn, table, id string) []byte {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var ciphertext []byte
	query := `SELECT COALESCE(secret_ciphertext, ''::bytea) FROM ` + table + ` WHERE id = $1`
	if err = db.QueryRowContext(t.Context(), query, id).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	return ciphertext
}

func writeCiphertext(t *testing.T, dsn, table, id string, ciphertext []byte) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	query := `UPDATE ` + table + ` SET secret_ciphertext = $1 WHERE id = $2`
	if _, err = db.ExecContext(t.Context(), query, ciphertext, id); err != nil {
		t.Fatal(err)
	}
}

func TestSecretWriteWithoutSealerIsUnavailable(t *testing.T) {
	s := openTestStore(t, pgtest.New(t))
	_, err := s.CreateSource(t.Context(), SourceInput{
		Name: "source", Socket: "/run/mysqld/mysqld.sock", Datadir: "/var/lib/mysql",
		Username: "backup", Secret: []byte("{{SECRET_unconfigured}}"),
	}, nil, SystemAudit("sources.create", AuditDetail{}))
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("create source without sealer = %v", err)
	}
	_, err = s.CreateRepository(t.Context(), RepositoryInput{
		Name: "ftp", Kind: RepositoryFTP, TLSMode: TLSModePlain,
		FTP:    &FTPRepositoryConfig{Host: "127.0.0.1", Port: 21, BasePath: "/b", Username: "u"},
		Secret: []byte("{{SECRET_unconfigured_ftp}}"),
	}, nil, SystemAudit("repositories.create", AuditDetail{}))
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("create ftp without sealer = %v", err)
	}
	local, err := s.CreateRepository(t.Context(), RepositoryInput{
		Name: "local", Kind: RepositoryLocal, TLSMode: TLSModeNone, Root: filepath.Join(t.TempDir(), "root"),
	}, nil, SystemAudit("repositories.create", AuditDetail{}))
	if err != nil {
		t.Fatalf("create local without sealer = %v", err)
	}
	if local.HasSecret {
		t.Fatal("local repository stored a secret")
	}
}
