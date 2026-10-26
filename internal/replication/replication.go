// Package replication copies completed local physical backups to a remote
// repository and proves each copy by reading it back.
//
// State lives in the caller's control-plane SQLite database, in its own goose
// table (replication_goose_db_version) applied after the physical migrations
// that own the backups table. A replica row is per (backup, repository); its
// idem_key makes enqueue repeatable.
//
// Copying is strictly FIFO per (source, repository): an item is processed only
// when every earlier item for that pair is verified, so the verified prefix
// never skips a gap. A blocked replication item never blocks local backups.
//
// Local objects are read through the published local repository. Remote objects
// go through the narrow Remote interface; NewFTPRemote adapts the real FTP
// repository, and a test double implements the same interface.
package replication

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/flyingEirc/Intern/internal/artifact"
	physicalstore "github.com/flyingEirc/Intern/internal/physical/store"
	"github.com/flyingEirc/Intern/internal/pipeline"
	"github.com/flyingEirc/Intern/internal/storage/ftp"
	"github.com/flyingEirc/Intern/internal/storage/local"
)

// Replica statuses. Only verified contributes to the remote protection point.
const (
	StatusPending   = "pending"
	StatusUploading = "uploading"
	StatusUploaded  = "uploaded"
	StatusVerified  = "verified"
	StatusFailed    = "failed"
	StatusExhausted = "exhausted"
)

// Member names in their fixed upload order. Every replica uses the same four
// keys, so a retry never describes a different object set.
const (
	memberPayload      = "payload.bin"
	memberEnvelope     = "envelope.json"
	memberMetadata     = "metadata.bin"
	memberMetaEnvelope = "metadata.envelope.json"
)

// Per-member publication states. An uncertain member is never re-uploaded; its
// final key is decided by the verification readback.
const (
	memberPending   = "pending"
	memberUploaded  = "uploaded"
	memberUncertain = "uncertain"
	memberVerified  = "verified"
)

// memberOrder is the fixed upload order: payload, envelope, metadata,
// metadata envelope.
var memberOrder = []string{memberPayload, memberEnvelope, memberMetadata, memberMetaEnvelope}

// Error codes are fixed strings. They never contain FTP replies, remote paths,
// or credentials; the numeric reply code is stored in its own column.
const (
	CodeLocalCorrupt    = "local_corrupt"
	CodeLocalMissing    = "local_missing"
	CodeStateInvalid    = "state_invalid"
	CodeInternal        = "internal_error"
	CodeCommitUncertain = "commit_uncertain"
	CodeRemoteConflict  = "remote_conflict"
	CodeRemoteQuota     = "remote_quota"
	CodeRemoteReply     = "remote_reply"
	CodeRemoteTransport = "remote_transport"
	CodeRemoteOwnership = "remote_ownership"
	CodeCleanupPending  = "cleanup_pending"
	CodeVerifyArtifact  = "verify_artifact"
	CodeVerifyManifest  = "verify_manifest"
)

// stepHook observes the fixed boundaries of one attempt. Each boundary runs only
// after that boundary's real I/O, and the hook may inject a failure there.
// Production builds leave it nil; only an integration-tagged entry point attaches
// one. A nil hook is the production path and adds no work.
type stepHook func(point string) error

func (h stepHook) call(point string) error {
	if h == nil {
		return nil
	}
	return h(point)
}

// Policy bounds retries and reporting. Zero values select the documented
// defaults: 1m base delay, 1h cap, 6h alert threshold, 10 attempts.
type Policy struct {
	BaseDelay, MaxDelay, AlertAfter time.Duration
	MaxAttempts                     int
}

// Documented defaults.
const (
	DefaultBaseDelay        = time.Minute
	DefaultMaxDelay         = time.Hour
	DefaultAlertAfter       = 6 * time.Hour
	DefaultMaxAttempts      = 10
	DefaultMaxExpandedBytes = 4 << 30
)

// Sink is the minimal publication surface of one remote object. *ftp.Sink
// satisfies it; pipeline.Sink is a strict superset.
type Sink interface {
	io.Writer
	Commit(ctx context.Context) (pipeline.Receipt, error)
	Abort() error
	TemporaryPath() string
}

// Remote is the remote-object surface replication needs: create one object,
// read it back, size it, and retry a temporary-key cleanup.
type Remote interface {
	Create(ctx context.Context, key string) (Sink, error)
	Open(ctx context.Context, key string, offset int64) (io.ReadCloser, error)
	Size(ctx context.Context, key string) (int64, error)
	CleanupPart(ctx context.Context, key string) error
}

// ftpRepository is the concrete FTP surface FTPRemote delegates to.
type ftpRepository interface {
	Create(ctx context.Context, key string) (*ftp.Sink, error)
	Open(ctx context.Context, key string, offset int64) (io.ReadCloser, error)
	Size(ctx context.Context, key string) (int64, error)
	CleanupPart(ctx context.Context, key string) error
}

// The real FTP repository must satisfy the narrow interfaces without any
// adapter on the data path.
var (
	_ Sink          = (*ftp.Sink)(nil)
	_ ftpRepository = (*ftp.Repository)(nil)
)

// FTPRemote adapts the real FTP repository to Remote. It changes no sink
// lifecycle, part cleanup, or commit semantics; the caller still owns Close.
type FTPRemote struct{ Repository *ftp.Repository }

// NewFTPRemote adapts an opened FTP repository.
func NewFTPRemote(repository *ftp.Repository) *FTPRemote { return &FTPRemote{Repository: repository} }

var _ Remote = (*FTPRemote)(nil)

func (f *FTPRemote) Create(ctx context.Context, key string) (Sink, error) {
	if f == nil || f.Repository == nil {
		return nil, errors.New("replication: no FTP repository")
	}
	sink, err := f.Repository.Create(ctx, key)
	if err != nil {
		return nil, err
	}
	return sink, nil
}

func (f *FTPRemote) Open(ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
	if f == nil || f.Repository == nil {
		return nil, errors.New("replication: no FTP repository")
	}
	return f.Repository.Open(ctx, key, offset)
}

func (f *FTPRemote) Size(ctx context.Context, key string) (int64, error) {
	if f == nil || f.Repository == nil {
		return 0, errors.New("replication: no FTP repository")
	}
	return f.Repository.Size(ctx, key)
}

func (f *FTPRemote) CleanupPart(ctx context.Context, key string) error {
	if f == nil || f.Repository == nil {
		return errors.New("replication: no FTP repository")
	}
	return f.Repository.CleanupPart(ctx, key)
}

// Replicator owns replica state for one remote repository. Remote keys are the
// local object path plus the member name, so the remote instance must be the
// local instance. One caller runs Step; Now injects the clock.
type Replicator struct {
	DB               *sql.DB
	Store            *physicalstore.Store
	Local            *local.Repository
	Remote           Remote
	RepositoryID     string
	InstanceID       string
	KeyFile          string
	WorkRoot         string
	MaxExpandedBytes int64
	Policy           Policy
	Now              func() time.Time
	// hook is nil in production and set only by an integration-tagged entry point.
	hook stepHook
}

//go:embed migrations/*.sql
var migrations embed.FS

const versionTable = "replication_goose_db_version"

// Migrate runs only replication migrations on the caller's control-plane
// database, after the physical migrations that create the backups table. The
// caller owns the database and its process-wide migration lock.
func Migrate(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return errors.New("replication: database is required")
	}
	current, latest, err := Version(ctx, db)
	if err != nil {
		return err
	}
	if current > latest {
		return fmt.Errorf("replication database version %d is newer than embedded latest version %d", current, latest)
	}
	fsys, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, fsys,
		goose.WithDisableGlobalRegistry(true), goose.WithTableName(versionTable))
	if err != nil {
		return err
	}
	_, err = provider.Up(ctx)
	return err
}

// Version is read-only, including when the version table does not yet exist.
// A nil database returns the embedded latest version without opening a database.
func Version(ctx context.Context, db *sql.DB) (current, latest int64, err error) {
	entries, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return 0, 0, err
	}
	for _, entry := range entries {
		prefix, _, _ := strings.Cut(strings.TrimPrefix(entry, "migrations/"), "_")
		version, parseErr := strconv.ParseInt(prefix, 10, 64)
		if parseErr != nil {
			return 0, 0, parseErr
		}
		latest = max(latest, version)
	}
	if db == nil {
		return 0, latest, nil
	}
	var exists bool
	if err = db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM sqlite_schema WHERE type = 'table' AND name = ?)", versionTable).Scan(&exists); err != nil || !exists {
		return 0, latest, err
	}
	err = db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version_id), 0) FROM replication_goose_db_version v
WHERE is_applied = 1 AND id = (SELECT MAX(id) FROM replication_goose_db_version WHERE version_id = v.version_id)`).Scan(&current)
	return current, latest, err
}

// stampLayout is a fixed-width UTC timestamp, so SQLite text order is time order.
const stampLayout = "2006-01-02T15:04:05.000000000Z"

func stamp(value time.Time) string { return value.UTC().Format(stampLayout) }

func nullableStamp(value time.Time) sql.NullString {
	return sql.NullString{String: stamp(value), Valid: true}
}

func parseStamp(value sql.NullString) (*time.Time, error) {
	if !value.Valid {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value.String)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

func requiredStamp(value string) (time.Time, error) { return time.Parse(time.RFC3339Nano, value) }

// clock returns the injected time, normalized to UTC.
func (r *Replicator) clock() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

// effectivePolicy fills the documented defaults for zero or negative values.
func (r *Replicator) effectivePolicy() Policy {
	p := r.Policy
	if p.BaseDelay <= 0 {
		p.BaseDelay = DefaultBaseDelay
	}
	if p.MaxDelay <= 0 {
		p.MaxDelay = DefaultMaxDelay
	}
	if p.AlertAfter <= 0 {
		p.AlertAfter = DefaultAlertAfter
	}
	if p.MaxAttempts <= 0 {
		p.MaxAttempts = DefaultMaxAttempts
	}
	return p
}

func (r *Replicator) expandLimit() int64 {
	if r.MaxExpandedBytes <= 0 {
		return DefaultMaxExpandedBytes
	}
	return r.MaxExpandedBytes
}

// check validates the ownership and filesystem prerequisites of one attempt.
func (r *Replicator) check() error {
	if r.DB == nil || r.Store == nil || r.Local == nil || r.Remote == nil {
		return errors.New("replication: database, store, local repository and remote are required")
	}
	for _, field := range []struct{ name, value string }{
		{"repository", r.RepositoryID},
		{"instance", r.InstanceID},
		{"key file", r.KeyFile},
		{"work root", r.WorkRoot},
	} {
		if field.value == "" {
			return fmt.Errorf("replication: %s is required", field.name)
		}
	}
	return r.checkWorkRoot()
}

// checkWorkRoot requires an existing private work root, separate from the KEK.
func (r *Replicator) checkWorkRoot() error {
	if !filepath.IsAbs(r.WorkRoot) {
		return errors.New("replication: work root must be absolute")
	}
	info, err := os.Lstat(r.WorkRoot)
	if err != nil {
		return fmt.Errorf("replication: work root is unusable: %w", err)
	}
	if !info.IsDir() || info.Mode() != os.ModeDir|0o700 {
		return errors.New("replication: work root must be a non-symlink private directory (mode 0700)")
	}
	if err = artifact.ValidateKeyLocation(r.KeyFile, r.WorkRoot); err != nil {
		return errors.New("replication: KEK must be separate from the work root")
	}
	return nil
}

// objectKey joins the local object path and one member name and rejects paths
// that do not belong to this instance.
func (r *Replicator) objectKey(objectPath, member string) (string, error) {
	if !fs.ValidPath(objectPath) {
		return "", errors.New("replication: invalid local object path")
	}
	instance, rest, found := strings.Cut(objectPath, "/")
	if !found || rest == "" || instance != r.InstanceID {
		return "", errors.New("replication: object path belongs to another instance")
	}
	return objectPath + "/" + member, nil
}

// replicaFailure carries a fixed code, whether another attempt may help, and the
// underlying cause for the caller's log. It is never rendered to the user.
type replicaFailure struct {
	code      string
	retryable bool
	cause     error
}

func (e *replicaFailure) Error() string { return "replication: " + e.code }

func (e *replicaFailure) Unwrap() error { return e.cause }

func retryable(code string, cause error) error {
	return &replicaFailure{code: code, retryable: true, cause: cause}
}

func terminal(code string, cause error) error {
	return &replicaFailure{code: code, retryable: false, cause: cause}
}

// asReplicaFailure extracts a replica failure, treating an unclassified cause as
// a retryable internal error.
func asReplicaFailure(err error) *replicaFailure {
	var failure *replicaFailure
	if errors.As(err, &failure) {
		return failure
	}
	return &replicaFailure{code: CodeInternal, retryable: true, cause: err}
}
