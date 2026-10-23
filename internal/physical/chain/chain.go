// Package chain publishes authenticated physical backups and coordinates their durable cleanup ledger.
//
// After streaming and metadata commit, Run removes the run directory and syncs
// WorkRoot before Publish and Complete. Cleanup failure deliberately costs the
// backup rather than leaving a completed row with an untracked work directory.
// Complete atomically removes the ledger only after this durable cleanup and
// authenticated directory publication have both succeeded.
package chain

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/flyingEirc/Intern/internal/artifact"
	"github.com/flyingEirc/Intern/internal/physical"
	"github.com/flyingEirc/Intern/internal/physical/store"
	"github.com/flyingEirc/Intern/internal/pipeline"
	"github.com/flyingEirc/Intern/internal/storage/local"
)

// Job requires a freshly admitted P2 identity and exclusive scheduling ownership.
// WorkRoot is an existing private directory, separate from the repository and KEK.
type Job struct {
	Store                *store.Store
	Repo                 *local.Repository
	Binary, WorkRoot     string
	Connection           physical.Connection
	Identity             physical.Identity
	ToolVersion          string
	InstanceID, SourceID string
	Kind                 string
	MaxChainLength       int
	KeyFile              string
	Parallel, Throttle   int
	PageTracking         bool
	Now                  func() time.Time
}

type Result struct {
	BackupID, ChainID, ObjectPath string
	Seq                           int
	Checkpoints                   physical.Checkpoints
	PayloadSize                   int64
	PayloadSHA256                 string
}

// identityManifest is deliberately independent of the still-running tool's metadata.
type identityManifest struct {
	Version       int    `json:"version"`
	Role          string `json:"role"`
	InstanceID    string `json:"instance_id"`
	SourceID      string `json:"source_id"`
	BackupID      string `json:"backup_id"`
	ChainID       string `json:"chain_id"`
	ParentID      string `json:"parent_id,omitempty"`
	Seq           int    `json:"seq"`
	Kind          string `json:"kind"`
	ServerUUID    string `json:"server_uuid"`
	ServerVersion string `json:"server_version"`
	ToolVersion   string `json:"tool_version"`
}

type toolManifest struct {
	Identity       identityManifest `json:"identity"`
	CheckpointsRaw []byte           `json:"checkpoints"`
	InfoRaw        []byte           `json:"xtrabackup_info"`
	PayloadSize    int64            `json:"payload_size"`
	PayloadSHA256  string           `json:"payload_sha256"`
}

type runHook func(point string) error

func (h runHook) call(point string) error {
	if h != nil {
		return h(point)
	}
	return nil
}

// Run's only success point is the completed SQLite transaction after durable publication.
func Run(ctx context.Context, job Job) (Result, error) {
	return run(ctx, job, nil)
}

type execution struct {
	job                   Job
	plan                  store.Plan
	result                Result
	runID, workDir, keyID string
	staging               *local.Staging
	published             bool
	hook                  runHook
}

func run(ctx context.Context, job Job, hook runHook) (Result, error) {
	if err := validateJob(job); err != nil {
		return Result{}, err
	}
	plan, err := job.Store.Plan(ctx, store.PlanRequest{SourceID: job.SourceID, ServerUUID: job.Identity.UUID, ServerVersion: job.Identity.Version, ToolVersion: job.ToolVersion, Kind: job.Kind, MaxChainLength: job.MaxChainLength})
	if err != nil {
		return Result{}, err
	}
	keyID, err := artifact.KeyID(job.KeyFile)
	if err != nil {
		return Result{}, errors.New("physical chain: key unavailable")
	}
	backupID, runID := randomID(), randomID()
	e := execution{job: job, plan: plan, runID: runID, workDir: filepath.Join(job.WorkRoot, "run-"+runID), keyID: keyID, hook: hook}
	e.result = Result{BackupID: backupID, ChainID: plan.ChainID, Seq: plan.Seq, ObjectPath: path.Join(job.InstanceID, job.SourceID, plan.ChainID, fmt.Sprintf("%04d-%s", plan.Seq, backupID))}
	begin := store.BeginRequest{Plan: plan, SourceID: job.SourceID, ServerUUID: job.Identity.UUID, ServerVersion: job.Identity.Version, ToolVersion: job.ToolVersion, InstanceID: job.InstanceID, RunID: runID, BackupID: backupID, ObjectPath: e.result.ObjectPath, StagingToken: runID, WorkDir: e.workDir, KeyID: keyID, CreatedAt: now(job)}
	if err = job.Store.Begin(ctx, begin); err != nil {
		return e.result, err
	}
	err = e.execute(ctx)
	return e.result, err
}

func (e *execution) execute(ctx context.Context) (err error) {
	defer func() {
		if !e.published {
			err = errors.Join(err, e.abort(ctx))
		}
	}()
	if err = os.Mkdir(e.workDir, 0o700); err != nil {
		return errors.New("physical chain: cannot create run directory")
	}
	e.staging, err = e.job.Repo.BeginOwned(e.result.ObjectPath, e.runID)
	if err != nil {
		return errors.New("physical chain: cannot begin staging")
	}
	manifest := e.identity()
	metadata, stream, err := writeBackup(ctx, e.job, e.plan, e.workDir, e.staging, manifest, e.keyID, e.hook)
	if err != nil {
		return err
	}
	e.result.Checkpoints, e.result.PayloadSize, e.result.PayloadSHA256 = metadata.Checkpoints, stream.Size(), stream.SHA256()
	metaStream, err := writeMetadata(ctx, e.job, e.staging, manifest, metadata, stream, e.keyID, e.hook)
	if err != nil {
		return err
	}
	if err = removeRun(e.job.WorkRoot, e.workDir, e.runID); err != nil {
		return errors.New("physical chain: run cleanup failed before publication")
	}
	if err = e.publish(ctx); err != nil {
		return err
	}
	return e.complete(ctx, metadata, stream, metaStream)
}

func (e *execution) identity() identityManifest {
	job, plan := e.job, e.plan
	manifest := identityManifest{Version: 1, Role: "xbstream", InstanceID: job.InstanceID, SourceID: job.SourceID, BackupID: e.result.BackupID, ChainID: plan.ChainID, Seq: plan.Seq, Kind: plan.Kind, ServerUUID: job.Identity.UUID, ServerVersion: job.Identity.Version, ToolVersion: job.ToolVersion}
	if plan.Parent != nil {
		manifest.ParentID = plan.Parent.ID
	}
	return manifest
}

func (e *execution) abort(ctx context.Context) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	var err error
	if e.staging != nil {
		err = e.staging.Abort()
	}
	err = errors.Join(err, e.job.Store.Fail(cleanupCtx, e.result.BackupID, "backup_failed"))
	return errors.Join(err, cleanupBackup(cleanupCtx, e.job.Store, e.job.Repo, e.job.WorkRoot, e.job.InstanceID, e.result.BackupID))
}

func (e *execution) publish(ctx context.Context) error {
	if err := e.hook.call("before_publish"); err != nil {
		return err
	}
	if err := e.staging.Publish(ctx); err != nil {
		return errors.New("physical chain: publication failed")
	}
	// Once Publish succeeds, an uncertain completion remains running with all
	// ownership records. Recovery, never late cancellation, reclaims it.
	e.published = true
	return e.hook.call("published")
}

func (e *execution) complete(ctx context.Context, metadata physical.Metadata, stream, metaStream *artifact.Stream) error {
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	cp := metadata.Checkpoints
	if err := e.hook.call("before_complete"); err != nil {
		return err
	}
	err := e.job.Store.Complete(finishCtx, store.Completion{BackupID: e.result.BackupID, CheckpointsRaw: metadata.CheckpointsRaw, InfoRaw: metadata.InfoRaw, FromLSN: cp.FromLSN, ToLSN: cp.ToLSN, LastLSN: cp.LastLSN, PayloadSize: stream.Size(), PayloadSHA256: stream.SHA256(), MetadataSize: metaStream.Size(), MetadataSHA256: metaStream.SHA256(), CompletedAt: now(e.job)})
	if err != nil {
		return errors.New("physical chain: completion pending recovery")
	}
	return nil
}

func validateJob(job Job) error {
	if job.Store == nil || job.Repo == nil || !filepath.IsAbs(job.WorkRoot) || !safeComponent(job.InstanceID) || !safeComponent(job.SourceID) {
		return errors.New("physical chain: invalid ownership or work root")
	}
	if err := artifact.ValidateKeyLocation(job.KeyFile, job.WorkRoot); err != nil {
		return errors.New("physical chain: invalid key location")
	}
	root, err := openWorkRoot(job.WorkRoot)
	if err != nil {
		return errors.New("physical chain: invalid private work root")
	}
	return root.Close()
}

func safeComponent(value string) bool {
	return value != "" && strings.IndexFunc(value, invalidOwnershipRune) < 0
}

func invalidOwnershipRune(c rune) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return false
	default:
		return c != '-' && c != '_'
	}
}

func randomID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic("physical chain: random generator failed")
	}
	return hex.EncodeToString(value[:])
}

func now(job Job) time.Time {
	if job.Now != nil {
		return job.Now().UTC()
	}
	return time.Now().UTC()
}

func writeBackup(ctx context.Context, job Job, plan store.Plan, workDir string, staging *local.Staging, manifest identityManifest, keyID string, hook runHook) (physical.Metadata, *artifact.Stream, error) {
	raw, err := json.Marshal(manifest)
	if err != nil {
		return physical.Metadata{}, nil, err
	}
	stream, err := artifact.NewStream(artifact.Options{Compress: true, Encrypt: true, KeyFile: job.KeyFile}, raw)
	if err != nil || stream.KeyID() != keyID {
		return physical.Metadata{}, nil, errors.New("physical chain: stream key unavailable or changed")
	}
	source := &physical.BackupSource{Binary: job.Binary, WorkRoot: workDir, Connection: job.Connection, Parallel: job.Parallel, Throttle: job.Throttle, PageTracking: job.PageTracking}
	if plan.Parent != nil {
		source.ParentCheckpoints = plan.Parent.CheckpointsRaw
	}
	if err = writeStream(ctx, staging, "payload.bin", source, stream, hook); err != nil {
		return physical.Metadata{}, nil, err
	}
	metadata, err := source.Metadata()
	if err != nil {
		return physical.Metadata{}, nil, err
	}
	if metadata.Info.Fields["server_version"] != job.Identity.Version || metadata.Info.Fields["tool_version"] != job.ToolVersion {
		return physical.Metadata{}, nil, errors.New("physical chain: tool metadata version changed")
	}
	if err = writeEnvelope(ctx, staging, "envelope.json", stream); err != nil {
		return physical.Metadata{}, nil, err
	}
	return metadata, stream, nil
}

func writeMetadata(ctx context.Context, job Job, staging *local.Staging, identity identityManifest, metadata physical.Metadata, payload *artifact.Stream, keyID string, hook runHook) (*artifact.Stream, error) {
	identity.Role = "tool-metadata"
	raw, err := json.Marshal(toolManifest{Identity: identity, CheckpointsRaw: metadata.CheckpointsRaw, InfoRaw: metadata.InfoRaw, PayloadSize: payload.Size(), PayloadSHA256: payload.SHA256()})
	if err != nil {
		return nil, err
	}
	stream, err := artifact.NewStream(artifact.Options{Compress: true, Encrypt: true, KeyFile: job.KeyFile}, raw)
	if err != nil || stream.KeyID() != keyID {
		return nil, errors.New("physical chain: metadata key unavailable or changed")
	}
	if err = writeStream(ctx, staging, "metadata.bin", bytesSource(raw), stream, nil); err != nil {
		return nil, err
	}
	if err = hook.call("metadata"); err != nil {
		return nil, err
	}
	if err = writeEnvelope(ctx, staging, "metadata.envelope.json", stream); err != nil {
		return nil, err
	}
	return stream, nil
}

type bytesSource []byte

func (s bytesSource) Run(_ context.Context, w io.Writer) error {
	_, err := io.Copy(w, bytes.NewReader(s))
	return err
}

func writeEnvelope(ctx context.Context, staging *local.Staging, name string, stream *artifact.Stream) error {
	raw, err := stream.Envelope()
	if err != nil {
		return err
	}
	sink, err := staging.Create(name)
	if err != nil {
		return errors.New("physical chain: cannot create envelope")
	}
	_, err = pipeline.Run(ctx, bytesSource(raw), nil, sink)
	return err
}

func writeStream(ctx context.Context, staging *local.Staging, name string, source pipeline.Source, stream *artifact.Stream, hook runHook) error {
	sink, err := staging.Create(name)
	if err != nil {
		return errors.New("physical chain: cannot create payload")
	}
	stages := stream.Stages()
	if hook != nil {
		started := new(atomic.Bool)
		source = &observedSource{Source: source, started: started}
		sink = &observedSink{Sink: sink, hook: hook, started: started}
		original := stages[0]
		stages[0] = func(next io.Writer) (io.WriteCloser, error) {
			writer, openErr := original(next)
			if openErr != nil {
				return nil, openErr
			}
			return &observedClose{WriteCloser: writer, hook: hook}, nil
		}
	}
	_, err = pipeline.Run(ctx, source, stages, sink)
	return err
}

type observedSource struct {
	pipeline.Source
	started *atomic.Bool
}

func (s *observedSource) Run(ctx context.Context, w io.Writer) error {
	s.started.Store(true)
	return s.Source.Run(ctx, w)
}

type observedSink struct {
	pipeline.Sink
	hook    runHook
	started *atomic.Bool
}

func (s *observedSink) Write(p []byte) (int, error) {
	n, err := s.Sink.Write(p)
	if err == nil && s.started.Load() {
		err = s.hook.call("write")
	}
	return n, err
}

func (s *observedSink) Commit(ctx context.Context) (pipeline.Receipt, error) {
	receipt, err := s.Sink.Commit(ctx)
	if err == nil {
		err = s.hook.call("fsync")
	}
	return receipt, err
}

type observedClose struct {
	io.WriteCloser
	hook runHook
}

func (s *observedClose) Close() error {
	if err := s.WriteCloser.Close(); err != nil {
		return err
	}
	return s.hook.call("close")
}
