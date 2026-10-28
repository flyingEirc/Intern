package restore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/flyingEirc/Intern/internal/artifact"
	"github.com/flyingEirc/Intern/internal/fsutil"
	"github.com/flyingEirc/Intern/internal/physical/store"
)

// identityManifest is the authenticated manifest of the payload artifact.
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

// toolManifest is the authenticated manifest of the metadata artifact.
type toolManifest struct {
	Identity       identityManifest `json:"identity"`
	CheckpointsRaw []byte           `json:"checkpoints"`
	InfoRaw        []byte           `json:"xtrabackup_info"`
	PayloadSize    int64            `json:"payload_size"`
	PayloadSHA256  string           `json:"payload_sha256"`
}

func artifactRead(ctx context.Context, opts Options, dir string, output io.Writer, limit int64, encrypt bool) ([]byte, error) {
	return artifact.ReadFiles(ctx,
		filepath.Join(dir, memberEnvelope), filepath.Join(dir, memberPayload), output,
		artifact.ReadOptions{KeyFile: opts.KeyFile, WorkDirectory: dir, MaxBytes: limit, Encrypt: encrypt})
}

func artifactReadMetadata(ctx context.Context, opts Options, dir string, output io.Writer, limit int64) ([]byte, error) {
	return artifact.ReadFiles(ctx,
		filepath.Join(dir, memberMetaEnvelope), filepath.Join(dir, memberMetadata), output,
		artifact.ReadOptions{KeyFile: opts.KeyFile, WorkDirectory: dir, MaxBytes: limit, Encrypt: true})
}

// Authentication sub-step labels. They appear only in the cause chain of an
// execution failure, never in ExecError.Error(), so a native run can tell which
// of the checks refused while the public error text stays free of any tool
// diagnostic, path or credential.
const (
	authStepRecord       = "record"
	authStepMember       = "member"
	authStepPayload      = "payload"
	authStepToolMetadata = "tool_metadata"
)

// authFailure attaches one fixed sub-step label to a refusal.
func authFailure(step string, err error) error {
	return fmt.Errorf("restore: authenticate %s: %w", step, err)
}

// authenticate checks all four members against the stored record and both
// artifacts to EOF before any extract. For an FTP source the recorded member
// digests are checked as well; the local repository has no separate envelope
// record, so the artifacts themselves authenticate those bytes.
func (e *execution) authenticate(ctx context.Context, link PlanLink, dir string) error {
	backup, err := e.opts.Store.Backup(ctx, link.BackupID)
	if err != nil || !backupMatchesLink(backup, link) {
		return authFailure(authStepRecord, errors.New("the backup row changed"))
	}
	if err = e.authenticateMembers(ctx, link, dir, backup); err != nil {
		return err
	}
	if err = e.authenticatePayload(ctx, dir, backup); err != nil {
		return err
	}
	return e.authenticateToolMetadata(ctx, dir, backup)
}

func backupMatchesLink(backup store.Backup, link PlanLink) bool {
	return backup.Status == "completed" &&
		backup.PayloadSize == link.PayloadSize && backup.MetadataSize == link.MetadataSize &&
		backup.ObjectPath == link.objectPath
}

func (e *execution) authenticatePayload(ctx context.Context, dir string, backup store.Backup) error {
	identityRaw, err := artifactRead(ctx, e.opts, dir, io.Discard, e.expandLimit(), true)
	if err != nil {
		return authFailure(authStepPayload, errors.New("the payload artifact did not authenticate"))
	}
	var identity identityManifest
	if err = json.Unmarshal(identityRaw, &identity); err != nil {
		return authFailure(authStepPayload, errors.New("the payload manifest is unreadable"))
	}
	if !matchesIdentity(identity, backup, e.plan, "xbstream") {
		return authFailure(authStepPayload, errors.New("the payload manifest differs from the backup record"))
	}
	return nil
}

func (e *execution) authenticateToolMetadata(ctx context.Context, dir string, backup store.Backup) error {
	var body bytes.Buffer
	metaRaw, err := artifactReadMetadata(ctx, e.opts, dir, &body, e.expandLimit())
	if err != nil {
		return authFailure(authStepToolMetadata, errors.New("the metadata artifact did not authenticate"))
	}
	if !bytes.Equal(metaRaw, body.Bytes()) {
		return authFailure(authStepToolMetadata, errors.New("the metadata body and its authenticated manifest disagree"))
	}
	var tool toolManifest
	if err = json.Unmarshal(metaRaw, &tool); err != nil {
		return authFailure(authStepToolMetadata, errors.New("the tool manifest is unreadable"))
	}
	if !matchesIdentity(tool.Identity, backup, e.plan, "tool-metadata") {
		return authFailure(authStepToolMetadata, errors.New("the tool manifest differs from the backup record"))
	}
	if !bytes.Equal(tool.CheckpointsRaw, backup.CheckpointsRaw) || !bytes.Equal(tool.InfoRaw, backup.InfoRaw) {
		return authFailure(authStepToolMetadata, errors.New("the tool manifest does not bind the stored tool bytes"))
	}
	if tool.PayloadSize != backup.PayloadSize || tool.PayloadSHA256 != backup.PayloadSHA256 {
		return authFailure(authStepToolMetadata, errors.New("the tool manifest does not bind the payload"))
	}
	return nil
}

func (e *execution) authenticateMembers(ctx context.Context, link PlanLink, dir string, backup store.Backup) error {
	if err := checkFetchedMembers(dir, backup); err != nil {
		return authFailure(authStepMember, err)
	}
	if link.Source != "ftp" {
		return nil
	}
	if err := e.checkReplicaMembers(ctx, link, dir); err != nil {
		return authFailure(authStepMember, err)
	}
	return nil
}

// checkFetchedMembers requires every member to be a regular nonempty file and
// the recorded ciphertext sizes and digests to match the backup row.
func checkFetchedMembers(dir string, backup store.Backup) error {
	sizes := map[string]int64{memberPayload: backup.PayloadSize, memberMetadata: backup.MetadataSize}
	digests := map[string]string{memberPayload: backup.PayloadSHA256, memberMetadata: backup.MetadataSHA256}
	for _, member := range memberOrder {
		path := filepath.Join(dir, member)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 {
			return errors.New("a fetched member is missing or not a regular file")
		}
		if size, ok := sizes[member]; ok && info.Size() != size {
			return errors.New("a fetched member size differs from the record")
		}
		if digest, ok := digests[member]; ok {
			actual, err := fileDigest(path)
			if err != nil || actual != digest {
				return errors.New("a fetched member digest differs from the record")
			}
		}
	}
	return nil
}

func (e *execution) checkReplicaMembers(ctx context.Context, link PlanLink, dir string) error {
	record, err := e.opts.Replication.Replica(ctx, link.BackupID, link.RepositoryID)
	if err != nil || record.Status != "verified" {
		return errors.New("the replica is no longer verified")
	}
	for _, member := range memberOrder {
		path := filepath.Join(dir, member)
		info, err := os.Lstat(path)
		if err != nil {
			return errors.New("a fetched member is missing")
		}
		recorded, ok := record.ReplicaMemberByName(member)
		if !ok || recorded.Size != info.Size() {
			return errors.New("a replica member size differs from the recorded member")
		}
		actual, err := fileDigest(path)
		if err != nil || actual != recorded.SHA256 {
			return errors.New("a replica member digest differs from the recorded member")
		}
	}
	return nil
}

func fileDigest(path string) (digest string, retErr error) {
	file, err := fsutil.Open(path)
	if err != nil {
		return "", err
	}
	defer fsutil.Close(&retErr, file)
	hash := sha256.New()
	if _, err = io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func matchesIdentity(identity identityManifest, backup store.Backup, plan Plan, role string) bool {
	if identity.Version != 1 || identity.Role != role {
		return false
	}
	if !matchesBackupIdentity(identity, backup, plan.SourceID) {
		return false
	}
	return matchesChainIdentity(identity, plan)
}

// manifestKinds maps the kind a backup tool manifest records onto the kind the
// catalog row stores. The chain writes the planning kind it was handed ("full",
// "incremental") into the manifest, while the backups row is written with the
// catalog vocabulary "physical-" + planning kind
// (internal/physical/store/transitions.go). They are the same fact in two
// vocabularies, so the identity check maps one onto the other; every other
// pairing, including a full manifest against an incremental row, stays a refusal.
var manifestKinds = map[string]string{"full": "physical-full", "incremental": "physical-incremental"}

func kindsMatch(manifestKind, recordKind string) bool {
	return manifestKind == recordKind || manifestKinds[manifestKind] == recordKind
}

func matchesBackupIdentity(identity identityManifest, backup store.Backup, sourceID string) bool {
	return identity.InstanceID == backup.InstanceID && identity.SourceID == sourceID &&
		identity.BackupID == backup.ID && identity.ChainID == backup.ChainID &&
		identity.Seq == backup.Seq && kindsMatch(identity.Kind, backup.Kind) && identity.ParentID == backup.ParentID
}

func matchesChainIdentity(identity identityManifest, plan Plan) bool {
	return identity.ServerUUID == plan.ServerUUID &&
		identity.ServerVersion == plan.ServerVersion && identity.ToolVersion == plan.ToolVersion
}
