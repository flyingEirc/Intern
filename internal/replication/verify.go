package replication

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/flyingEirc/Intern/internal/artifact"
	"github.com/flyingEirc/Intern/internal/fsutil"
	physicalstore "github.com/flyingEirc/Intern/internal/physical/store"
	"github.com/flyingEirc/Intern/internal/replication/internal/replsql"
)

// identityManifest is the authenticated manifest of the payload artifact. Only
// the compared fields are declared; encoding/json/v2 ignores the rest.
type identityManifest struct {
	Version    int    `json:"version"`
	Role       string `json:"role"`
	InstanceID string `json:"instance_id"`
	SourceID   string `json:"source_id"`
	BackupID   string `json:"backup_id"`
	ChainID    string `json:"chain_id"`
	Seq        int    `json:"seq"`
}

// toolManifest is the authenticated manifest of the metadata artifact. It binds
// the payload ciphertext size and digest recorded in the backups row.
type toolManifest struct {
	Identity      identityManifest `json:"identity"`
	PayloadSize   int64            `json:"payload_size"`
	PayloadSHA256 string           `json:"payload_sha256"`
}

// verify reads every member back from the remote repository at offset zero and
// proves the copy before recording verified. Temps live under WorkRoot and are
// always removed.
func (r *Replicator) verify(ctx context.Context, backup physicalstore.Backup, members map[string]memberRecord) (retErr error) {
	work, err := os.MkdirTemp(r.WorkRoot, "verify-")
	if err != nil {
		return retryable(CodeRemoteTransport, err)
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(work)) }()
	if err = r.download(ctx, backup, members, work); err != nil {
		return err
	}
	identity, tool, err := r.authenticate(ctx, work)
	if err != nil {
		return err
	}
	return r.confirmManifests(ctx, backup, identity, tool)
}

// download retrieves each member at offset zero into a private work file and
// checks its digest and size against the recorded local member.
func (r *Replicator) download(ctx context.Context, backup physicalstore.Backup, members map[string]memberRecord, work string) error {
	for _, member := range memberOrder {
		record, ok := members[member]
		if !ok || record.digest == "" {
			return terminal(CodeStateInvalid, errors.New("replication: member digest is unrecorded"))
		}
		key, err := r.objectKey(backup.ObjectPath, member)
		if err != nil {
			return terminal(CodeStateInvalid, err)
		}
		digest, size, err := r.retrieve(ctx, key, filepath.Join(work, member))
		if err != nil {
			return err
		}
		if digest != record.digest || size != record.size {
			// An uncertain commit is accepted only by this readback, so a mismatch
			// there is a conflicting object at the final key, never a retry.
			if record.status == memberUncertain {
				return terminal(CodeRemoteConflict, errors.New("replication: final key holds a different object"))
			}
			return retryable(CodeVerifyManifest, errors.New("replication: remote member differs from the local member"))
		}
	}
	return nil
}

// retrieve copies one remote object at offset zero into path and returns its
// digest and byte count. A partial read is never success.
func (r *Replicator) retrieve(ctx context.Context, key, path string) (digest string, size int64, retErr error) {
	reader, err := r.Remote.Open(ctx, key, 0)
	if err != nil {
		return "", 0, classify(err)
	}
	defer fsutil.Close(&retErr, reader)
	file, err := fsutil.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", 0, err
	}
	defer fsutil.Close(&retErr, file)
	hash := sha256.New()
	if size, err = io.Copy(io.MultiWriter(file, hash), reader); err != nil {
		return "", 0, retryable(CodeRemoteTransport, err)
	}
	if err = file.Sync(); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

// authenticate runs both artifact authentications to EOF with the chain KEK and
// returns the authenticated manifests. A wrong key, a truncated stream, or a
// corrupted block fails here.
func (r *Replicator) authenticate(ctx context.Context, work string) ([]byte, []byte, error) {
	identity, err := artifact.ReadFiles(ctx,
		filepath.Join(work, memberEnvelope), filepath.Join(work, memberPayload), io.Discard, r.readOptions(work))
	if err != nil {
		return nil, nil, retryable(CodeVerifyArtifact, errors.New("replication: payload artifact did not authenticate"))
	}
	tool, err := artifact.ReadFiles(ctx,
		filepath.Join(work, memberMetaEnvelope), filepath.Join(work, memberMetadata), io.Discard, r.readOptions(work))
	if err != nil {
		return nil, nil, retryable(CodeVerifyArtifact, errors.New("replication: metadata artifact did not authenticate"))
	}
	return identity, tool, nil
}

func (r *Replicator) readOptions(work string) artifact.ReadOptions {
	return artifact.ReadOptions{KeyFile: r.KeyFile, WorkDirectory: work, MaxBytes: r.expandLimit(), Encrypt: true}
}

// confirmManifests decodes both authenticated manifests and compares them with
// the backup row and its chain.
func (r *Replicator) confirmManifests(ctx context.Context, backup physicalstore.Backup, identityRaw, toolRaw []byte) error {
	chain, err := r.Store.Chain(ctx, backup.ChainID)
	if err != nil {
		return terminal(CodeStateInvalid, err)
	}
	var identity identityManifest
	if err = json.Unmarshal(identityRaw, &identity); err != nil {
		return retryable(CodeVerifyArtifact, errors.New("replication: payload manifest is unreadable"))
	}
	if !matchesIdentity(identity, backup, chain.SourceID, r.InstanceID, "xbstream") {
		return retryable(CodeVerifyManifest, errors.New("replication: payload manifest differs from the backup record"))
	}
	var tool toolManifest
	if err = json.Unmarshal(toolRaw, &tool); err != nil {
		return retryable(CodeVerifyArtifact, errors.New("replication: metadata manifest is unreadable"))
	}
	if !matchesIdentity(tool.Identity, backup, chain.SourceID, r.InstanceID, "tool-metadata") {
		return retryable(CodeVerifyManifest, errors.New("replication: metadata manifest differs from the backup record"))
	}
	if tool.PayloadSize != backup.PayloadSize || tool.PayloadSHA256 != backup.PayloadSHA256 {
		return retryable(CodeVerifyManifest, errors.New("replication: payload binding differs from the backup record"))
	}
	return nil
}

func matchesIdentity(identity identityManifest, backup physicalstore.Backup, sourceID, instanceID, role string) bool {
	return identity.Version == 1 && identity.Role == role &&
		identity.InstanceID == instanceID && identity.SourceID == sourceID &&
		identity.BackupID == backup.ID && identity.ChainID == backup.ChainID && identity.Seq == backup.Seq
}

// recordVerified marks every member and then the replica verified. A verified
// replica clears any alert.
func (r *Replicator) recordVerified(ctx context.Context, backupID string) error {
	q := replsql.New(r.DB)
	now := stamp(r.clock())
	for _, member := range memberOrder {
		if err := q.SetMemberStatus(ctx, replsql.SetMemberStatusParams{
			Status: memberVerified, UpdatedAt: now, BackupID: backupID,
			RepositoryID: r.RepositoryID, Member: member,
		}); err != nil {
			return err
		}
	}
	return q.SetReplicaVerified(ctx, replsql.SetReplicaVerifiedParams{
		VerifiedAt: sql.NullString{String: now, Valid: true},
		UploadedAt: sql.NullString{String: now, Valid: true},
		UpdatedAt:  now, BackupID: backupID, RepositoryID: r.RepositoryID,
		LocalRepositoryID: r.LocalRepositoryID,
	})
}
