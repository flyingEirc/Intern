// Package restore turns a completed physical backup chain into an isolated,
// authenticated datadir export and optionally proves it by starting a rehearsal
// server. It never writes a running datadir: every step works below a private
// work root, and the export is published with a non-replacing rename only after
// every link authenticated and the whole chain prepared.
package restore

import (
	"context"
	"io"

	"github.com/flyingEirc/Intern/internal/physical"
	"github.com/flyingEirc/Intern/internal/physical/store"
	"github.com/flyingEirc/Intern/internal/replication"
	"github.com/flyingEirc/Intern/internal/storage/ftp"
	"github.com/flyingEirc/Intern/internal/storage/local"
)

// DefaultExpansionFactor is the conservative prepared-size multiplier used when
// Options.ExpansionFactor is zero. It is an estimate, not a measurement.
const DefaultExpansionFactor = 8

// Member names of one backup, in their fixed order. The same four keys exist in
// the local repository and in every replica.
const (
	memberPayload      = "payload.bin"
	memberEnvelope     = "envelope.json"
	memberMetadata     = "metadata.bin"
	memberMetaEnvelope = "metadata.envelope.json"
)

var memberOrder = []string{memberPayload, memberEnvelope, memberMetadata, memberMetaEnvelope}

// Locator resolves where a completed backup's ciphertext can be read. W5
// implements it from runs.backup_id → runs.local_repository_id and replicas
// rows; P4 tests implement it directly.
type Locator interface {
	// Local returns the local repository that holds backupID's object, or (nil, nil) when none is recorded.
	// The returned repository is owned by the caller; restore never closes it.
	Local(ctx context.Context, backupID string) (*local.Repository, error)
	// Replicas returns FTP replicas of backupID in preference order (only status
	// rows; restore rechecks verified and each member's recorded size).
	Replicas(ctx context.Context, backupID string) ([]ReplicaRef, error)
}

// ReplicaRef names one replica repository and opens it on demand. Open unseals
// credentials in the caller; restore closes the returned repository after the
// link that used it.
type ReplicaRef struct {
	RepositoryID string
	Open         func(ctx context.Context) (*ftp.Repository, error)
}

// Options bounds one plan or execution. All roots must be absolute. WorkRoot is
// a private 0700 directory owned by the current uid; ExportRoot is an existing
// directory outside the work root, the metadata directory, every local
// repository root and the KEK.
type Options struct {
	Store          *store.Store
	Replication    *replication.Catalog
	Locator        Locator
	KeyFile        string
	WorkRoot       string
	ExportRoot     string
	XtrabackupPath string
	XbstreamPath   string
	MysqldPath     string
	// RestrictedRoots are additional absolute directories the export root must
	// be outside, typically the metadata directory and every local repository
	// root. W5 resolves them from its configuration.
	RestrictedRoots []string
	ExpansionFactor int
	Parallel        int
	UseMemory       int64

	// remote overrides ReplicaRef.Open inside this package's white-box tests.
	// Production leaves it nil and uses the real FTP repository.
	remote func(context.Context, ReplicaRef) (remoteReader, error)
	// probeTool overrides the native version probe in white-box tests that have
	// no native tool; production leaves it nil and runs the real binary.
	probeTool func(context.Context, string) (physical.ToolVersion, error)
	// fetchFault is set only by ExecuteWithFetchFault in integration builds. It
	// makes one fetch attempt fail after real bytes were written, so the caller
	// observes a genuine RetrFrom resume.
	fetchFault func(attempt int, key string, offset int64) (int64, error)
}

// Plan is the read-only, digest-bound description of one restore point.
type Plan struct {
	TargetBackupID       string     `json:"target_backup_id"`
	ChainID              string     `json:"chain_id"`
	SourceID             string     `json:"source_id"`
	ServerUUID           string     `json:"server_uuid"`
	ServerVersion        string     `json:"server_version"`
	ToolVersion          string     `json:"tool_version"`
	TargetToLSN          uint64     `json:"target_to_lsn"`
	Links                []PlanLink `json:"links"`
	RequiredWorkBytes    int64      `json:"required_work_bytes"`
	RequiredExportBytes  int64      `json:"required_export_bytes"`
	AvailableWorkBytes   int64      `json:"available_work_bytes"`
	AvailableExportBytes int64      `json:"available_export_bytes"`
	// ExportPath is where the export will appear once Execute publishes it.
	// Until then it contains the literal {restore_id} placeholder.
	ExportPath string   `json:"export_path"`
	Risks      []string `json:"risks"`
	// Digest binds links, sources, sizes and LSNs. Free-space numbers and
	// ExportPath are not part of it.
	Digest string `json:"digest"`
}

// PlanLink is one authenticated chain member and the source chosen for it.
type PlanLink struct {
	BackupID     string `json:"backup_id"`
	Seq          int    `json:"seq"`
	Kind         string `json:"kind"`
	FromLSN      uint64 `json:"from_lsn"`
	ToLSN        uint64 `json:"to_lsn"`
	Source       string `json:"source"` // "local" or "ftp"
	RepositoryID string `json:"repository_id,omitempty"`
	PayloadSize  int64  `json:"payload_size"`
	MetadataSize int64  `json:"metadata_size"`

	// objectPath is the repository path of this member set. It is not part of
	// the wire contract or the digest; Execute re-plans and reads it here.
	objectPath string
}

// Request is one explicit restore execution. The plan must still match a fresh
// BuildPlan by Digest.
type Request struct {
	RestoreID string
	Plan      Plan
	Rehearse  bool
	VerifySQL []string
}

// Phase is one observable execution stage.
type Phase string

const (
	PhaseFetching       Phase = "fetching"
	PhaseAuthenticating Phase = "authenticating"
	PhaseExtracting     Phase = "extracting"
	PhasePreparing      Phase = "preparing"
	PhaseExporting      Phase = "exporting"
	PhaseRehearsing     Phase = "rehearsing"
)

// Progress is called synchronously at phase boundaries. Data contains only
// fixed labels and identifiers, never credentials or KEK material.
type Progress func(phase Phase, data map[string]any)

// Result describes a published export and, when requested, its rehearsal.
type Result struct {
	ExportPath    string        `json:"export_path"`
	ExportBytes   int64         `json:"export_bytes"`
	PreparedToLSN uint64        `json:"prepared_to_lsn"`
	PrepareArgs   [][]string    `json:"prepare_args"`
	Verify        *VerifyResult `json:"verify"`
}

// VerifyResult is the rehearsal outcome, separate from the restore status.
type VerifyResult struct {
	Status               string        `json:"status"` // passed | failed
	Started              bool          `json:"started"`
	SystemTablesReadable bool          `json:"system_tables_readable"`
	Schemas              []SchemaStat  `json:"schemas"`
	CheckTable           []CheckStat   `json:"check_table"`
	Queries              []QueryResult `json:"queries"`
	FailureCode          string        `json:"failure_code,omitempty"`
}

// SchemaStat is the exact table and row count of one user schema.
type SchemaStat struct {
	Name   string `json:"name"`
	Tables int    `json:"tables"`
	Rows   int64  `json:"rows"`
}

// CheckStat is one CHECK TABLE outcome.
type CheckStat struct {
	Table  string `json:"table"`
	Status string `json:"status"`
}

// QueryResult is one requested read-only query and its raw result.
type QueryResult struct {
	SQL       string     `json:"sql"`
	Columns   []string   `json:"columns"`
	Rows      [][]string `json:"rows"`
	ErrorCode string     `json:"error_code,omitempty"`
}

// SideEffects states exactly which restore-owned paths this execution removed
// or published. A true removal flag means this restore id has no such path when
// the call returns, including when the path was never created. A published
// export is never reported as removed, and the flags are reported even when the
// cleanup itself failed, in which case the execution code is cleanup_failed.
type SideEffects struct {
	WorkDirRemoved       bool `json:"work_dir_removed"`
	PendingExportRemoved bool `json:"pending_export_removed"`
	ExportPublished      bool `json:"export_published"`
}

// remoteReader is the FTP surface restore needs. *ftp.Repository satisfies it.
type remoteReader interface {
	Size(ctx context.Context, key string) (int64, error)
	Open(ctx context.Context, key string, offset int64) (io.ReadCloser, error)
	Close() error
}
