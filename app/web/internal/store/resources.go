package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/google/uuid"
	"modernc.org/sqlite"

	"github.com/flyingEirc/Intern/app/web/internal/store/internal/storesql"
)

// SecretSealer seals and opens a secret bound to table:id:field. It is the
// credential package's Sealer; the store only needs these two operations so the
// control-plane database never handles a KEK itself.
type SecretSealer interface {
	Seal(table, id, field string, plaintext []byte) ([]byte, error)
	Unseal(table, id, field string, ciphertext []byte) ([]byte, error)
}

// secretField is the single sealed column name on repositories and sources.
const secretField = "secret_ciphertext"

// Resource kinds and the only TLS mode each supports. FTPS is not implemented.
const (
	RepositoryLocal = "local"
	RepositoryFTP   = "ftp"

	TLSModeNone  = "none"
	TLSModePlain = "plain"
)

// LocalRepositoryConfig is the stored and returned configuration of a local
// repository: one absolute directory owned by this instance.
type LocalRepositoryConfig struct {
	Root string `json:"root"`
}

// FTPRepositoryConfig is the stored and returned configuration of a plain FTP
// repository. The password is never part of it.
type FTPRepositoryConfig struct {
	Host     string `json:"host"`
	Port     int64  `json:"port"`
	BasePath string `json:"base_path"`
	Username string `json:"username"`
}

// Address is the host:port form ftp.Open accepts.
func (c FTPRepositoryConfig) Address() string {
	return net.JoinHostPort(c.Host, strconv.FormatInt(c.Port, 10))
}

// Repository is one backup target. The sealed secret is not part of it: callers
// learn only whether one is stored.
type Repository struct {
	ID                   string                 `json:"id"`
	Name                 string                 `json:"name"`
	Kind                 string                 `json:"kind"`
	TLSMode              string                 `json:"tls_mode"`
	Local                *LocalRepositoryConfig `json:"local,omitempty"`
	FTP                  *FTPRepositoryConfig   `json:"ftp,omitempty"`
	HasSecret            bool                   `json:"has_secret"`
	PlaintextCredentials bool                   `json:"plaintext_credentials"`
	CreatedAt            string                 `json:"created_at"`
	UpdatedAt            string                 `json:"updated_at"`
}

// RepositoryInput is a create or full replacement of a repository. Secret is
// plaintext; the store seals it. A nil Secret on update keeps the stored one.
type RepositoryInput struct {
	Name    string
	Kind    string
	TLSMode string
	Root    string
	FTP     *FTPRepositoryConfig
	Secret  []byte
}

// Source is a MySQL instance reachable over a local socket. The sealed password
// is not part of it.
type Source struct {
	ID            string           `json:"id"`
	Name          string           `json:"name"`
	Socket        string           `json:"socket"`
	Datadir       string           `json:"datadir"`
	Username      string           `json:"username"`
	HasSecret     bool             `json:"has_secret"`
	ServerUUID    *string          `json:"server_uuid"`
	ServerVersion *string          `json:"server_version"`
	LastPreflight *PreflightRecord `json:"last_preflight"`
	CreatedAt     string           `json:"created_at"`
	UpdatedAt     string           `json:"updated_at"`
}

// SourceInput is a create or full replacement of a source.
type SourceInput struct {
	Name     string
	Socket   string
	Datadir  string
	Username string
	Secret   []byte
}

// PreflightWarning is one safe, actionable preflight warning.
type PreflightWarning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// PreflightRecord is the observation persisted with a source or an enabled
// policy. It never contains credentials, DSNs or raw tool output. The server
// identity is that of the MySQL instance the preflight observed.
type PreflightRecord struct {
	OK            bool               `json:"ok"`
	Checks        []string           `json:"checks"`
	Warnings      []PreflightWarning `json:"warnings"`
	ServerUUID    string             `json:"server_uuid,omitempty"`
	ServerVersion string             `json:"server_version,omitempty"`
}

// Policy binds one source to a local repository and an optional FTP replica.
type Policy struct {
	ID                  string           `json:"id"`
	Name                string           `json:"name"`
	SourceID            string           `json:"source_id"`
	LocalRepositoryID   string           `json:"local_repository_id"`
	ReplicaRepositoryID *string          `json:"replica_repository_id"`
	FullCron            string           `json:"full_cron"`
	IncrementalCron     string           `json:"incremental_cron"`
	Timezone            string           `json:"timezone"`
	MaxChainLength      int64            `json:"max_chain_length"`
	Parallel            int64            `json:"parallel"`
	Throttle            int64            `json:"throttle"`
	ZstdLevel           int64            `json:"zstd_level"`
	PageTracking        bool             `json:"page_tracking"`
	Enabled             bool             `json:"enabled"`
	LastPreflight       *PreflightRecord `json:"last_preflight"`
	CreatedAt           string           `json:"created_at"`
	UpdatedAt           string           `json:"updated_at"`
}

// PolicyInput is a create or full replacement of a policy. Enabled is honoured
// only together with the Preflight that authorised it.
type PolicyInput struct {
	Name                string
	SourceID            string
	LocalRepositoryID   string
	ReplicaRepositoryID *string
	FullCron            string
	IncrementalCron     string
	Timezone            string
	MaxChainLength      int64
	Parallel            int64
	Throttle            int64
	ZstdLevel           int64
	PageTracking        bool
	Enabled             bool
	Preflight           *PreflightRecord
}

// ErrReferenced reports a delete refused because another row still uses the
// resource. Physical chains and replication replicas are checked by raw SQL
// because their tables belong to other modules.
var ErrReferenced = errors.New("resource is still referenced")

// ErrUnavailable reports a secret write or open that needs a KEK this
// deployment does not configure. The server maps it to 503 unavailable.
var ErrUnavailable = errors.New("credential encryption is not configured")

// resourceError maps a missing row to ErrNotFound and a unique-name violation
// to ErrConflict. SQLite reports both UNIQUE and PRIMARY KEY conflicts as 2067.
func resourceError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) && (sqliteErr.Code() == 2067 || sqliteErr.Code() == 1555) {
		return ErrConflict
	}
	return err
}

func repositoryFromRow(row storesql.Repository) (Repository, error) {
	repository := Repository{
		ID: row.ID, Name: row.Name, Kind: row.Kind, TLSMode: row.TlsMode,
		HasSecret: len(row.SecretCiphertext) > 0, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
	switch row.Kind {
	case RepositoryLocal:
		var config LocalRepositoryConfig
		if err := json.Unmarshal([]byte(row.ConfigJson), &config); err != nil {
			return Repository{}, fmt.Errorf("decode local repository config: %w", err)
		}
		repository.Local = &config
	case RepositoryFTP:
		var config FTPRepositoryConfig
		if err := json.Unmarshal([]byte(row.ConfigJson), &config); err != nil {
			return Repository{}, fmt.Errorf("decode FTP repository config: %w", err)
		}
		repository.FTP = &config
		repository.PlaintextCredentials = true
	default:
		return Repository{}, fmt.Errorf("unknown repository kind %q", row.Kind)
	}
	return repository, nil
}

func (in RepositoryInput) configJSON() (string, error) {
	var config any
	switch in.Kind {
	case RepositoryLocal:
		config = LocalRepositoryConfig{Root: in.Root}
	case RepositoryFTP:
		if in.FTP == nil {
			return "", errors.New("FTP repository requires a configuration")
		}
		config = *in.FTP
	default:
		return "", fmt.Errorf("unknown repository kind %q", in.Kind)
	}
	encoded, err := json.Marshal(config)
	return string(encoded), err
}

func sourceFromRow(row storesql.Source) (Source, error) {
	source := Source{
		ID: row.ID, Name: row.Name, Socket: row.Socket, Datadir: row.Datadir,
		Username: row.Username, HasSecret: len(row.SecretCiphertext) > 0,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
	if row.ServerUuid.Valid {
		uuid := row.ServerUuid.String
		source.ServerUUID = &uuid
	}
	if row.ServerVersion.Valid {
		version := row.ServerVersion.String
		source.ServerVersion = &version
	}
	record, err := decodePreflight(row.LastPreflightJson)
	if err != nil {
		return Source{}, err
	}
	source.LastPreflight = record
	return source, nil
}

func policyFromRow(row storesql.Policy) (Policy, error) {
	policy := Policy{
		ID: row.ID, Name: row.Name, SourceID: row.SourceID, LocalRepositoryID: row.LocalRepositoryID,
		FullCron: row.FullCron, IncrementalCron: row.IncrementalCron, Timezone: row.Timezone,
		MaxChainLength: row.MaxChainLength, Parallel: row.Parallel, Throttle: row.Throttle,
		ZstdLevel: row.ZstdLevel, PageTracking: row.PageTracking != 0, Enabled: row.Enabled != 0,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
	if row.ReplicaRepositoryID.Valid {
		replica := row.ReplicaRepositoryID.String
		policy.ReplicaRepositoryID = &replica
	}
	record, err := decodePreflight(row.LastPreflightJson)
	if err != nil {
		return Policy{}, err
	}
	policy.LastPreflight = record
	return policy, nil
}

func decodePreflight(value sql.NullString) (*PreflightRecord, error) {
	if !value.Valid {
		return nil, nil
	}
	var record PreflightRecord
	if err := json.Unmarshal([]byte(value.String), &record); err != nil {
		return nil, fmt.Errorf("decode stored preflight: %w", err)
	}
	return &record, nil
}

func encodePreflight(record *PreflightRecord) (sql.NullString, error) {
	if record == nil {
		return sql.NullString{}, nil
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return sql.NullString{}, err
	}
	return sql.NullString{String: string(encoded), Valid: true}, nil
}

func timestamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func newID() (string, error) {
	id, err := uuid.NewV7()
	return id.String(), err
}

func sealSecret(sealer SecretSealer, table, id string, secret []byte) ([]byte, error) {
	if len(secret) == 0 {
		return nil, errors.New("a secret is required")
	}
	if sealer == nil {
		return nil, ErrUnavailable
	}
	return sealer.Seal(table, id, secretField, secret)
}

func openSecret(sealer SecretSealer, table, id string, ciphertext []byte) ([]byte, error) {
	if len(ciphertext) == 0 {
		return nil, errors.New("no stored secret")
	}
	if sealer == nil {
		return nil, ErrUnavailable
	}
	return sealer.Unseal(table, id, secretField, ciphertext)
}

// CreateRepository seals the supplied secret and commits the row with its
// success audit in one transaction.
func (s *Store) CreateRepository(ctx context.Context, in RepositoryInput, sealer SecretSealer, entry AuditLog) (Repository, error) {
	config, err := in.configJSON()
	if err != nil {
		return Repository{}, err
	}
	id, err := newID()
	if err != nil {
		return Repository{}, err
	}
	var sealed []byte
	if in.Kind == RepositoryFTP {
		if sealed, err = sealSecret(sealer, "repositories", id, in.Secret); err != nil {
			return Repository{}, err
		}
	}
	var created Repository
	err = s.withTx(ctx, func(ctx context.Context, q *storesql.Queries) error {
		var writeErr error
		created, writeErr = insertRepository(ctx, q, id, in, config, sealed, entry)
		return writeErr
	})
	if err != nil {
		return Repository{}, err
	}
	return created, nil
}

func insertRepository(ctx context.Context, q *storesql.Queries, id string, in RepositoryInput, config string, sealed []byte, entry AuditLog) (Repository, error) {
	now := timestamp()
	row, err := q.CreateRepository(ctx, storesql.CreateRepositoryParams{
		ID: id, Name: in.Name, Kind: in.Kind, ConfigJson: config,
		SecretCiphertext: sealed, TlsMode: in.TLSMode, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		return Repository{}, resourceError(err)
	}
	created, err := repositoryFromRow(row)
	if err != nil {
		return Repository{}, err
	}
	return created, insertSuccessAudit(ctx, q, entry, "repository", created.ID)
}

// UpdateRepository replaces the mutable fields. A new secret is sealed against
// the same row id, so the previous ciphertext cannot be reused elsewhere.
func (s *Store) UpdateRepository(ctx context.Context, id string, in RepositoryInput, sealer SecretSealer, entry AuditLog) (Repository, error) {
	config, err := in.configJSON()
	if err != nil {
		return Repository{}, err
	}
	var sealed []byte
	if len(in.Secret) != 0 {
		if sealed, err = sealSecret(sealer, "repositories", id, in.Secret); err != nil {
			return Repository{}, err
		}
	}
	var updated Repository
	err = s.withTx(ctx, func(ctx context.Context, q *storesql.Queries) error {
		var writeErr error
		updated, writeErr = replaceRepository(ctx, q, id, in, config, sealed, entry)
		return writeErr
	})
	if err != nil {
		return Repository{}, err
	}
	return updated, nil
}

func replaceRepository(ctx context.Context, q *storesql.Queries, id string, in RepositoryInput, config string, sealed []byte, entry AuditLog) (Repository, error) {
	row, err := q.UpdateRepository(ctx, storesql.UpdateRepositoryParams{
		Name: in.Name, ConfigJson: config, SecretCiphertext: sealed, UpdatedAt: timestamp(), ID: id,
	})
	if err != nil {
		return Repository{}, resourceError(err)
	}
	updated, err := repositoryFromRow(row)
	if err != nil {
		return Repository{}, err
	}
	return updated, insertSuccessAudit(ctx, q, entry, "repository", updated.ID)
}

// Repository reads one repository without its sealed secret.
func (s *Store) Repository(ctx context.Context, id string) (Repository, error) {
	row, err := storesql.New(s.db).GetRepository(ctx, id)
	if err != nil {
		return Repository{}, resourceError(err)
	}
	return repositoryFromRow(row)
}

// Repositories returns at most limit repositories older than before.
func (s *Store) Repositories(ctx context.Context, before string, limit int) ([]Repository, error) {
	rows, err := storesql.New(s.db).ListRepositories(ctx, storesql.ListRepositoriesParams{Before: before, RowLimit: int64(limit)})
	if err != nil {
		return nil, err
	}
	repositories := make([]Repository, len(rows))
	for i, row := range rows {
		repository, convertErr := repositoryFromRow(row)
		if convertErr != nil {
			return nil, convertErr
		}
		repositories[i] = repository
	}
	return repositories, nil
}

// RepositorySecret decrypts the stored FTP password in memory.
func (s *Store) RepositorySecret(ctx context.Context, id string, sealer SecretSealer) ([]byte, error) {
	row, err := storesql.New(s.db).GetRepository(ctx, id)
	if err != nil {
		return nil, resourceError(err)
	}
	return openSecret(sealer, "repositories", row.ID, row.SecretCiphertext)
}

// DeleteRepository refuses a repository that a policy, a run snapshot or a
// replica row still uses. The run snapshot columns are the pair a run was
// claimed with: replication resolves every existing backup's pair from them,
// and a replica row binds the local repository its bytes live in, so deleting
// either side would strand those rows, block the repository's protection point
// forever, and leave nothing any API call could repair.
func (s *Store) DeleteRepository(ctx context.Context, id string, entry AuditLog) error {
	return s.withTxDB(ctx, func(ctx context.Context, q *storesql.Queries, tx *sql.Tx) error {
		if _, err := q.GetRepository(ctx, id); err != nil {
			return resourceError(err)
		}
		references, err := s.repositoryReferences(ctx, q, tx, id)
		if err != nil {
			return err
		}
		if references != 0 {
			return ErrReferenced
		}
		if err = q.DeleteRepository(ctx, id); err != nil {
			return resourceError(err)
		}
		return insertSuccessAudit(ctx, q, entry, "repository", id)
	})
}

// repositoryReferences counts every row that still points at one repository:
// its policies, the runs claimed with it locally or as their replica, and the
// replica rows queued on it or bound to it as their local repository.
func (s *Store) repositoryReferences(ctx context.Context, q *storesql.Queries, tx *sql.Tx, id string) (int64, error) {
	policies, err := q.CountPoliciesByRepository(ctx, id)
	if err != nil {
		return 0, err
	}
	total := policies
	for _, count := range []string{
		`SELECT COUNT(*) FROM runs WHERE local_repository_id = ?`,
		`SELECT COUNT(*) FROM runs WHERE replica_repository_id = ?`,
		`SELECT COUNT(*) FROM replicas WHERE repository_id = ?`,
		`SELECT COUNT(*) FROM replicas WHERE local_repository_id = ?`,
	} {
		rows, err := countRows(ctx, tx, count, id)
		if err != nil {
			return 0, err
		}
		total += rows
	}
	return total, nil
}

// CreateSource seals the MySQL password and commits the row with its success
// audit in one transaction.
func (s *Store) CreateSource(ctx context.Context, in SourceInput, sealer SecretSealer, entry AuditLog) (Source, error) {
	id, err := newID()
	if err != nil {
		return Source{}, err
	}
	sealed, err := sealSecret(sealer, "sources", id, in.Secret)
	if err != nil {
		return Source{}, err
	}
	var created Source
	err = s.withTx(ctx, func(ctx context.Context, q *storesql.Queries) error {
		var writeErr error
		created, writeErr = insertSource(ctx, q, id, in, sealed, entry)
		return writeErr
	})
	if err != nil {
		return Source{}, err
	}
	return created, nil
}

func insertSource(ctx context.Context, q *storesql.Queries, id string, in SourceInput, sealed []byte, entry AuditLog) (Source, error) {
	now := timestamp()
	row, err := q.CreateSource(ctx, storesql.CreateSourceParams{
		ID: id, Name: in.Name, Socket: in.Socket, Datadir: in.Datadir, Username: in.Username,
		SecretCiphertext: sealed, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		return Source{}, resourceError(err)
	}
	created, err := sourceFromRow(row)
	if err != nil {
		return Source{}, err
	}
	return created, insertSuccessAudit(ctx, q, entry, "source", created.ID)
}

// UpdateSource replaces the mutable fields; a supplied password is re-sealed.
func (s *Store) UpdateSource(ctx context.Context, id string, in SourceInput, sealer SecretSealer, entry AuditLog) (Source, error) {
	var sealed []byte
	var err error
	if len(in.Secret) != 0 {
		if sealed, err = sealSecret(sealer, "sources", id, in.Secret); err != nil {
			return Source{}, err
		}
	}
	var updated Source
	err = s.withTx(ctx, func(ctx context.Context, q *storesql.Queries) error {
		var writeErr error
		updated, writeErr = replaceSource(ctx, q, id, in, sealed, entry)
		return writeErr
	})
	if err != nil {
		return Source{}, err
	}
	return updated, nil
}

func replaceSource(ctx context.Context, q *storesql.Queries, id string, in SourceInput, sealed []byte, entry AuditLog) (Source, error) {
	row, err := q.UpdateSource(ctx, storesql.UpdateSourceParams{
		Name: in.Name, Socket: in.Socket, Datadir: in.Datadir, Username: in.Username,
		SecretCiphertext: sealed, UpdatedAt: timestamp(), ID: id,
	})
	if err != nil {
		return Source{}, resourceError(err)
	}
	updated, err := sourceFromRow(row)
	if err != nil {
		return Source{}, err
	}
	return updated, insertSuccessAudit(ctx, q, entry, "source", updated.ID)
}

// Source reads one source without its sealed password.
func (s *Store) Source(ctx context.Context, id string) (Source, error) {
	row, err := storesql.New(s.db).GetSource(ctx, id)
	if err != nil {
		return Source{}, resourceError(err)
	}
	return sourceFromRow(row)
}

// Sources returns at most limit sources older than before.
func (s *Store) Sources(ctx context.Context, before string, limit int) ([]Source, error) {
	rows, err := storesql.New(s.db).ListSources(ctx, storesql.ListSourcesParams{Before: before, RowLimit: int64(limit)})
	if err != nil {
		return nil, err
	}
	sources := make([]Source, len(rows))
	for i, row := range rows {
		source, convertErr := sourceFromRow(row)
		if convertErr != nil {
			return nil, convertErr
		}
		sources[i] = source
	}
	return sources, nil
}

// SourceSecret decrypts the stored MySQL password in memory.
func (s *Store) SourceSecret(ctx context.Context, id string, sealer SecretSealer) ([]byte, error) {
	row, err := storesql.New(s.db).GetSource(ctx, id)
	if err != nil {
		return nil, resourceError(err)
	}
	return openSecret(sealer, "sources", row.ID, row.SecretCiphertext)
}

// RecordSourcePreflight stores last_preflight_json for every completed run.
// Server identity columns are updated only when the record carries them; a
// failed identity check leaves the previous identity in place. A successful
// run commits its success audit in the same transaction; a failed run persists
// the observation without a success audit.
func (s *Store) RecordSourcePreflight(ctx context.Context, id string, record PreflightRecord, entry AuditLog) (Source, error) {
	encoded, err := encodePreflight(&record)
	if err != nil {
		return Source{}, err
	}
	var updated Source
	err = s.withTx(ctx, func(ctx context.Context, q *storesql.Queries) error {
		var writeErr error
		updated, writeErr = storeSourcePreflight(ctx, q, id, record, encoded, entry)
		return writeErr
	})
	if err != nil {
		return Source{}, err
	}
	return updated, nil
}

func storeSourcePreflight(ctx context.Context, q *storesql.Queries, id string, record PreflightRecord, encoded sql.NullString, entry AuditLog) (Source, error) {
	row, err := q.RecordSourcePreflight(ctx, storesql.RecordSourcePreflightParams{
		ServerUuid: nullable(record.ServerUUID), ServerVersion: nullable(record.ServerVersion),
		LastPreflightJson: encoded, UpdatedAt: timestamp(), ID: id,
	})
	if err != nil {
		return Source{}, resourceError(err)
	}
	updated, err := sourceFromRow(row)
	if err != nil {
		return Source{}, err
	}
	if !record.OK {
		return updated, nil
	}
	return updated, insertSuccessAudit(ctx, q, entry, "source", updated.ID)
}

// DeleteSource refuses a source that a policy or any physical chain uses.
func (s *Store) DeleteSource(ctx context.Context, id string, entry AuditLog) error {
	return s.withTxDB(ctx, func(ctx context.Context, q *storesql.Queries, tx *sql.Tx) error {
		if _, err := q.GetSource(ctx, id); err != nil {
			return resourceError(err)
		}
		policies, err := q.CountPoliciesBySource(ctx, id)
		if err != nil {
			return err
		}
		chains, err := countRows(ctx, tx, `SELECT COUNT(*) FROM chains WHERE source_id = ?`, id)
		if err != nil {
			return err
		}
		if policies != 0 || chains != 0 {
			return ErrReferenced
		}
		if err = q.DeleteSource(ctx, id); err != nil {
			return resourceError(err)
		}
		return insertSuccessAudit(ctx, q, entry, "source", id)
	})
}

// CreatePolicy commits the policy and its success audit together. Enabled is
// stored only with the preflight record that authorised it.
func (s *Store) CreatePolicy(ctx context.Context, in PolicyInput, entry AuditLog) (Policy, error) {
	id, err := newID()
	if err != nil {
		return Policy{}, err
	}
	replica, preflight, err := policyParams(in)
	if err != nil {
		return Policy{}, err
	}
	var created Policy
	err = s.withTx(ctx, func(ctx context.Context, q *storesql.Queries) error {
		var writeErr error
		created, writeErr = insertPolicy(ctx, q, id, in, replica, preflight, entry)
		return writeErr
	})
	if err != nil {
		return Policy{}, err
	}
	return created, nil
}

func insertPolicy(ctx context.Context, q *storesql.Queries, id string, in PolicyInput, replica, preflight sql.NullString, entry AuditLog) (Policy, error) {
	now := timestamp()
	row, err := q.CreatePolicy(ctx, storesql.CreatePolicyParams{
		ID: id, Name: in.Name, SourceID: in.SourceID, LocalRepositoryID: in.LocalRepositoryID,
		ReplicaRepositoryID: replica, FullCron: in.FullCron, IncrementalCron: in.IncrementalCron,
		Timezone: in.Timezone, MaxChainLength: in.MaxChainLength, Parallel: in.Parallel,
		Throttle: in.Throttle, ZstdLevel: in.ZstdLevel, PageTracking: encodeBool(in.PageTracking),
		Enabled: encodeBool(in.Enabled), LastPreflightJson: preflight, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		return Policy{}, resourceError(err)
	}
	created, err := policyFromRow(row)
	if err != nil {
		return Policy{}, err
	}
	return created, insertSuccessAudit(ctx, q, entry, "policy", created.ID)
}

// UpdatePolicy replaces every mutable field, including the enabled decision and
// the preflight that authorised it.
func (s *Store) UpdatePolicy(ctx context.Context, id string, in PolicyInput, entry AuditLog) (Policy, error) {
	replica, preflight, err := policyParams(in)
	if err != nil {
		return Policy{}, err
	}
	var updated Policy
	err = s.withTx(ctx, func(ctx context.Context, q *storesql.Queries) error {
		var writeErr error
		updated, writeErr = replacePolicy(ctx, q, id, in, replica, preflight, entry)
		return writeErr
	})
	if err != nil {
		return Policy{}, err
	}
	return updated, nil
}

func replacePolicy(ctx context.Context, q *storesql.Queries, id string, in PolicyInput, replica, preflight sql.NullString, entry AuditLog) (Policy, error) {
	row, err := q.UpdatePolicy(ctx, storesql.UpdatePolicyParams{
		Name: in.Name, SourceID: in.SourceID, LocalRepositoryID: in.LocalRepositoryID,
		ReplicaRepositoryID: replica, FullCron: in.FullCron, IncrementalCron: in.IncrementalCron,
		Timezone: in.Timezone, MaxChainLength: in.MaxChainLength, Parallel: in.Parallel,
		Throttle: in.Throttle, ZstdLevel: in.ZstdLevel, PageTracking: encodeBool(in.PageTracking),
		Enabled: encodeBool(in.Enabled), LastPreflightJson: preflight, UpdatedAt: timestamp(), ID: id,
	})
	if err != nil {
		return Policy{}, resourceError(err)
	}
	updated, err := policyFromRow(row)
	if err != nil {
		return Policy{}, err
	}
	return updated, insertSuccessAudit(ctx, q, entry, "policy", updated.ID)
}

// EnablePolicy stores the enable decision and the authorising preflight with its
// success audit in one transaction. Enabling without a successful preflight record
// is refused, so the flag cannot be turned on from a caller bug.
func (s *Store) EnablePolicy(ctx context.Context, id string, enabled bool, record *PreflightRecord, entry AuditLog) (Policy, error) {
	if enabled && (record == nil || !record.OK) {
		return Policy{}, errors.New("enabling a policy requires a successful preflight")
	}
	if !enabled {
		record = nil
	}
	encoded, err := encodePreflight(record)
	if err != nil {
		return Policy{}, err
	}
	var updated Policy
	err = s.withTx(ctx, func(ctx context.Context, q *storesql.Queries) error {
		var writeErr error
		updated, writeErr = setPolicyEnabled(ctx, q, id, enabled, encoded, entry)
		return writeErr
	})
	if err != nil {
		return Policy{}, err
	}
	return updated, nil
}

func setPolicyEnabled(ctx context.Context, q *storesql.Queries, id string, enabled bool, encoded sql.NullString, entry AuditLog) (Policy, error) {
	row, err := q.SetPolicyEnabled(ctx, storesql.SetPolicyEnabledParams{
		Enabled: encodeBool(enabled), LastPreflightJson: encoded, UpdatedAt: timestamp(), ID: id,
	})
	if err != nil {
		return Policy{}, resourceError(err)
	}
	updated, err := policyFromRow(row)
	if err != nil {
		return Policy{}, err
	}
	return updated, insertSuccessAudit(ctx, q, entry, "policy", updated.ID)
}

func (s *Store) Policy(ctx context.Context, id string) (Policy, error) {
	row, err := storesql.New(s.db).GetPolicy(ctx, id)
	if err != nil {
		return Policy{}, resourceError(err)
	}
	return policyFromRow(row)
}

// Policies returns at most limit policies older than before.
func (s *Store) Policies(ctx context.Context, before string, limit int) ([]Policy, error) {
	rows, err := storesql.New(s.db).ListPolicies(ctx, storesql.ListPoliciesParams{Before: before, RowLimit: int64(limit)})
	if err != nil {
		return nil, err
	}
	policies := make([]Policy, len(rows))
	for i, row := range rows {
		policy, convertErr := policyFromRow(row)
		if convertErr != nil {
			return nil, convertErr
		}
		policies[i] = policy
	}
	return policies, nil
}

// DeletePolicy removes a policy. Runs reference it, so a policy with run
// history is refused.
func (s *Store) DeletePolicy(ctx context.Context, id string, entry AuditLog) error {
	return s.withTxDB(ctx, func(ctx context.Context, q *storesql.Queries, tx *sql.Tx) error {
		if _, err := q.GetPolicy(ctx, id); err != nil {
			return resourceError(err)
		}
		// runs.policy_id references policies, so a policy with run history is
		// refused rather than turning the foreign-key violation into a 500.
		runs, err := countRows(ctx, tx, `SELECT COUNT(*) FROM runs WHERE policy_id = ?`, id)
		if err != nil {
			return err
		}
		if runs != 0 {
			return ErrReferenced
		}
		if err = q.DeletePolicy(ctx, id); err != nil {
			return resourceError(err)
		}
		return insertSuccessAudit(ctx, q, entry, "policy", id)
	})
}

func policyParams(in PolicyInput) (sql.NullString, sql.NullString, error) {
	replica := sql.NullString{}
	if in.ReplicaRepositoryID != nil {
		replica = sql.NullString{String: *in.ReplicaRepositoryID, Valid: true}
	}
	preflight, err := encodePreflight(in.Preflight)
	if err != nil {
		return replica, sql.NullString{}, err
	}
	if in.Enabled && !preflight.Valid {
		return replica, sql.NullString{}, errors.New("an enabled policy requires a successful preflight")
	}
	if !in.Enabled {
		preflight = sql.NullString{}
	}
	return replica, preflight, nil
}

func nullable(value string) sql.NullString {
	if value == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: value, Valid: true}
}

func encodeBool(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

// countRows runs one bounded existence count inside the caller's transaction.
// The chains and replicas tables belong to the physical and replication
// migrations, so those counts cannot be sqlc queries over the Web schema.
func countRows(ctx context.Context, tx *sql.Tx, query, argument string) (int64, error) {
	var count int64
	err := tx.QueryRowContext(ctx, query, argument).Scan(&count)
	return count, err
}
