// Package resources runs the real preflight and probe operations that gate
// repository, source and policy changes. It holds no HTTP or wire types: the
// server maps these outcomes to responses and persisted records.
//
// A preflight reads the live MySQL server through the mounted socket and checks
// the local repository directory; a probe writes and removes a real object on the
// target repository. Both use the deployment KEK only to open a stored secret in
// memory for the duration of the call.
package resources

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/flyingEirc/Intern/app/web/internal/store"
	"github.com/flyingEirc/Intern/internal/artifact"
	"github.com/flyingEirc/Intern/internal/credential"
	"github.com/flyingEirc/Intern/internal/physical"
	"github.com/flyingEirc/Intern/internal/storage/ftp"
	"github.com/flyingEirc/Intern/internal/storage/local"
)

// PreflightTimeout bounds one preflight or probe run. The request deadline is
// shorter or equal; this is the outer cap for the native checks.
const PreflightTimeout = 60 * time.Second

// ErrUnavailable reports a request that needs a KEK, work root, XtraBackup
// executable or capacity policy that this deployment does not configure. The
// caller renders one fixed message so the gap is not described per request.
var ErrUnavailable = errors.New("preflight and credential operations are not configured")

// ErrLocalRepositoryRequired reports a preflight that named a repository which
// is not a local repository.
var ErrLocalRepositoryRequired = errors.New("a local repository is required")

// Settings holds the deployment capabilities the operations need. Any missing
// entry makes only the dependent operation unavailable; the server still starts.
type Settings struct {
	Sealer         *credential.Sealer
	KeyFile        string
	MetadataDir    string
	WorkRoot       string
	XtrabackupPath string
	MinWorkBytes   uint64
	MinBackupBytes uint64
}

// ValidateRepositoryRoot refuses a local repository directory that contains the
// KEK. The check runs when a local repository is created or updated, because
// that directory is new input; the metadata and work directories were already
// checked at startup.
func (s Settings) ValidateRepositoryRoot(root string) error {
	if s.KeyFile == "" {
		return nil
	}
	return artifact.ValidateKeyLocation(s.KeyFile, s.MetadataDir, s.WorkRoot, root)
}

// Credentials reports whether sealed secrets can be opened and written.
func (s Settings) Credentials() bool { return s.Sealer != nil }

// Preflight reports whether every physical preflight input is configured.
func (s Settings) Preflight() bool {
	return s.Credentials() && s.WorkRoot != "" && s.XtrabackupPath != "" && s.MinWorkBytes > 0 && s.MinBackupBytes > 0
}

// Service runs preflight and probe operations against the control-plane store.
type Service struct {
	Store    *store.Store
	Settings Settings
}

// Failure is one structured preflight failure. It carries the check name, a
// stable code, a fixed message and, for privilege checks, the missing privilege
// names only.
type Failure struct {
	Check   string
	Code    string
	Message string
	Missing []string
}

// PreflightOutcome is a completed preflight, successful or not. A failure is
// reported in Failure, not as a Go error: the caller decides the HTTP status.
type PreflightOutcome struct {
	Checks        []string
	Warnings      []store.PreflightWarning
	ServerUUID    string
	ServerVersion string
	// ToolVersion is the XtraBackup version the tool probe observed. The
	// physical chain writes it into the backup metadata and compares it on every
	// later member of the chain, so it is never the MySQL server version.
	ToolVersion string
	Failure     *Failure
}

// OK reports whether every check passed.
func (o PreflightOutcome) OK() bool { return o.Failure == nil }

// Record is the persistable form of the observation.
func (o PreflightOutcome) Record() store.PreflightRecord {
	record := store.PreflightRecord{
		OK: o.OK(), Checks: o.Checks, Warnings: o.Warnings,
		ServerUUID: o.ServerUUID, ServerVersion: o.ServerVersion,
	}
	if record.Checks == nil {
		record.Checks = []string{}
	}
	if record.Warnings == nil {
		record.Warnings = []store.PreflightWarning{}
	}
	return record
}

// ProbeFailure is one repository probe failure. ReplyCode is the FTP reply code
// when the server answered; it is zero for local and transport failures.
type ProbeFailure struct {
	Code      string
	ReplyCode int
}

// ProbeOutcome is a completed repository probe. A failure is reported in
// Failure, not as a Go error.
type ProbeOutcome struct {
	Failure *ProbeFailure
}

// OK reports whether the probe wrote, read and removed a temporary object.
func (o ProbeOutcome) OK() bool { return o.Failure == nil }

// PreflightSource checks the live MySQL source and the local repository that
// will hold its backups. It persists nothing; the caller owns the transaction.
func (s *Service) PreflightSource(ctx context.Context, sourceID, localRepositoryID string) (PreflightOutcome, error) {
	if !s.Settings.Preflight() {
		return PreflightOutcome{}, ErrUnavailable
	}
	source, err := s.Store.Source(ctx, sourceID)
	if err != nil {
		return PreflightOutcome{}, err
	}
	repository, err := s.Store.Repository(ctx, localRepositoryID)
	if err != nil {
		return PreflightOutcome{}, err
	}
	if repository.Kind != store.RepositoryLocal || repository.Local == nil {
		return PreflightOutcome{}, ErrLocalRepositoryRequired
	}
	password, err := s.Store.SourceSecret(ctx, source.ID, s.Settings.Sealer)
	if err != nil {
		return PreflightOutcome{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, PreflightTimeout)
	defer cancel()
	result, err := physical.Preflight(ctx, physical.PreflightOptions{
		Connection: physical.Connection{
			Socket: source.Socket, DataDir: source.Datadir, Username: source.Username, Password: string(password),
		},
		Binary:         s.Settings.XtrabackupPath,
		WorkRoot:       s.Settings.WorkRoot,
		BackupRoot:     repository.Local.Root,
		MinWorkBytes:   s.Settings.MinWorkBytes,
		MinBackupBytes: s.Settings.MinBackupBytes,
	})
	outcome := PreflightOutcome{
		Checks: result.Checks, ServerUUID: result.Identity.UUID, ServerVersion: result.Identity.Version,
		ToolVersion: result.Tool.Version,
	}
	for _, warning := range result.WarningDetails {
		outcome.Warnings = append(outcome.Warnings, store.PreflightWarning{Code: warning.Code, Message: warning.Message})
	}
	if err != nil {
		var failure *physical.PreflightError
		if !errors.As(err, &failure) {
			return PreflightOutcome{}, fmt.Errorf("preflight failed: %w", err)
		}
		outcome.Failure = &Failure{Check: failure.Check, Code: failure.Code, Message: failure.Message, Missing: failure.Missing}
	}
	return outcome, nil
}

// ProbeRepository writes, reads and removes a temporary object on the target
// repository. Local repositories are probed through the local storage package
// and need no sealer. FTP repositories decrypt the stored password and are
// opened with this instance's namespace marker.
func (s *Service) ProbeRepository(ctx context.Context, repositoryID string) (ProbeOutcome, error) {
	repository, err := s.Store.Repository(ctx, repositoryID)
	if err != nil {
		return ProbeOutcome{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, PreflightTimeout)
	defer cancel()
	switch repository.Kind {
	case store.RepositoryLocal:
		if repository.Local == nil {
			return ProbeOutcome{Failure: &ProbeFailure{Code: "storage_root_missing"}}, nil
		}
		return probeLocal(ctx, repository.Local.Root), nil
	case store.RepositoryFTP:
		if !s.Settings.Credentials() {
			return ProbeOutcome{}, ErrUnavailable
		}
		return s.probeFTP(ctx, repository)
	default:
		return ProbeOutcome{}, fmt.Errorf("unsupported repository kind %q", repository.Kind)
	}
}

// PolicyPreflight checks the policy's source and local repository and, when the
// policy replicates, the FTP repository's writability. A policy is enabled only
// when this returns a successful outcome.
func (s *Service) PolicyPreflight(ctx context.Context, policy store.Policy) (PreflightOutcome, error) {
	source, err := s.PreflightSource(ctx, policy.SourceID, policy.LocalRepositoryID)
	if err != nil || !source.OK() {
		return source, err
	}
	if policy.ReplicaRepositoryID == nil {
		return source, nil
	}
	probe, err := s.ProbeRepository(ctx, *policy.ReplicaRepositoryID)
	if err != nil {
		return PreflightOutcome{}, err
	}
	if probe.OK() {
		return source, nil
	}
	failure := &Failure{
		Check: "replica", Code: probe.Failure.Code,
		Message: "The replica repository did not accept a probe write; check its permissions and capacity.",
	}
	source.Failure = failure
	return source, nil
}

func probeLocal(ctx context.Context, root string) ProbeOutcome {
	// local.Open creates a missing root at mode 0700 and refuses an existing
	// root with any other mode or a symlinked root, so one call both proves the
	// directory is usable and establishes it.
	repository, err := local.Open(root)
	if err != nil {
		return ProbeOutcome{Failure: &ProbeFailure{Code: "storage_unwritable"}}
	}
	outcome := probeStaging(ctx, repository)
	// Close joins the internal goroutine state. A close failure means the
	// repository could not be released cleanly, which is a probe failure only
	// when every earlier step succeeded.
	if err = repository.Close(); err != nil && outcome.OK() {
		return ProbeOutcome{Failure: &ProbeFailure{Code: "storage_cleanup_failed"}}
	}
	return outcome
}

func probeStaging(ctx context.Context, repository *local.Repository) ProbeOutcome {
	staging, err := repository.Begin("intern-probe-" + probeToken())
	if err != nil {
		return ProbeOutcome{Failure: &ProbeFailure{Code: "storage_unwritable"}}
	}
	if err = writeProbeObject(ctx, staging); err != nil {
		return ProbeOutcome{Failure: &ProbeFailure{Code: "storage_unwritable"}}
	}
	if err = staging.Abort(); err != nil {
		return ProbeOutcome{Failure: &ProbeFailure{Code: "storage_cleanup_failed"}}
	}
	return ProbeOutcome{}
}

func writeProbeObject(ctx context.Context, staging *local.Staging) error {
	sink, err := staging.Create("probe")
	if err != nil {
		return err
	}
	if _, err = sink.Write([]byte("intern repository probe")); err != nil {
		return err
	}
	if _, err = sink.Commit(ctx); err != nil {
		return err
	}
	return nil
}

func probeToken() string {
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic("cryptographic random source failed")
	}
	return hex.EncodeToString(id[:])
}

func (s *Service) probeFTP(ctx context.Context, repository store.Repository) (ProbeOutcome, error) {
	password, err := s.Store.RepositorySecret(ctx, repository.ID, s.Settings.Sealer)
	if err != nil {
		return ProbeOutcome{}, err
	}
	repositoryHandle, err := ftp.Open(ctx, ftp.Config{
		Address: repository.FTP.Address(), Username: repository.FTP.Username,
		Password: string(password), BasePath: repository.FTP.BasePath,
		InstanceID: s.Store.InstanceID(), TLSMode: repository.TLSMode,
	})
	if err != nil {
		return ProbeOutcome{Failure: ftpFailure(err)}, nil
	}
	err = repositoryHandle.Probe(ctx)
	closeErr := repositoryHandle.Close()
	if err != nil {
		return ProbeOutcome{Failure: ftpFailure(err)}, nil
	}
	if closeErr != nil {
		return ProbeOutcome{Failure: ftpFailure(closeErr)}, nil
	}
	return ProbeOutcome{}, nil
}

// ftpFailure keeps only the stable error kind and the reply code. Raw replies,
// paths, addresses and credentials stay out of the response.
func ftpFailure(err error) *ProbeFailure {
	var failure *ftp.Error
	if errors.As(err, &failure) {
		code := failure.Kind
		if code == "" {
			code = "ftp_failed"
		}
		return &ProbeFailure{Code: code, ReplyCode: failure.ReplyCode}
	}
	if errors.Is(err, fs.ErrInvalid) {
		return &ProbeFailure{Code: "invalid"}
	}
	return &ProbeFailure{Code: "ftp_failed"}
}
