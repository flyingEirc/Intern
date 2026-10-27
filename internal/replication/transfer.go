package replication

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"

	"github.com/flyingEirc/Intern/internal/fsutil"
	physicalstore "github.com/flyingEirc/Intern/internal/physical/store"
	"github.com/flyingEirc/Intern/internal/replication/internal/replsql"
	"github.com/flyingEirc/Intern/internal/storage/ftp"
)

// memberRecord is the durable state of one member of a replica.
type memberRecord struct {
	status string
	digest string
	size   int64
}

// localMember hashes one published member through the local repository and
// returns its ciphertext digest and byte count.
func (r *Replicator) localMember(objectPath, member string) (digest string, size int64, retErr error) {
	reader, err := r.Local.Open(objectPath + "/" + member)
	if err != nil {
		return "", 0, err
	}
	defer fsutil.Close(&retErr, reader)
	hash := sha256.New()
	if size, err = io.Copy(hash, reader); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

// rowDigest returns the recorded ciphertext size and digest for the two
// ciphertext members. The envelopes have no column; their local file is the
// authority.
func rowDigest(backup physicalstore.Backup, member string) (size int64, digest string, tracked bool) {
	switch member {
	case memberPayload:
		return backup.PayloadSize, backup.PayloadSHA256, true
	case memberMetadata:
		return backup.MetadataSize, backup.MetadataSHA256, true
	default:
		return 0, "", false
	}
}

// checkAgainstRow enforces the pre-upload check: the local ciphertext size and
// SHA-256 of payload.bin and metadata.bin must equal the completed backups row.
func checkAgainstRow(backup physicalstore.Backup, member, digest string, size int64) error {
	wantSize, wantDigest, tracked := rowDigest(backup, member)
	if !tracked {
		return nil
	}
	if wantSize < 0 || size != wantSize || digest != wantDigest {
		return terminal(CodeLocalCorrupt, errors.New("replication: local ciphertext differs from the completed backup record"))
	}
	return nil
}

// loadMembers reads this replica's member rows.
func (r *Replicator) loadMembers(ctx context.Context, backupID string) (map[string]memberRecord, error) {
	rows, err := replsql.New(r.DB).Members(ctx, replsql.MembersParams{BackupID: backupID, RepositoryID: r.RepositoryID})
	if err != nil {
		return nil, err
	}
	members := make(map[string]memberRecord, len(rows))
	for _, row := range rows {
		members[row.Member] = memberRecord{status: row.Status, digest: row.Digest, size: row.Size}
	}
	return members, nil
}

// prepareMembers hashes every member that still needs uploading, enforces the
// row comparison, and records its digest before any remote write.
func (r *Replicator) prepareMembers(ctx context.Context, backup physicalstore.Backup, members map[string]memberRecord) error {
	q := replsql.New(r.DB)
	for _, member := range memberOrder {
		if existing, ok := members[member]; ok && existing.status != memberPending {
			continue
		}
		digest, size, err := r.localMember(backup.ObjectPath, member)
		if err != nil {
			return terminal(CodeLocalMissing, errors.New("replication: local member is unreadable"))
		}
		if err = checkAgainstRow(backup, member, digest, size); err != nil {
			return err
		}
		if err = q.UpsertMember(ctx, replsql.UpsertMemberParams{
			BackupID: backup.ID, RepositoryID: r.RepositoryID, Member: member,
			Digest: digest, Size: size, UpdatedAt: stamp(r.clock()),
		}); err != nil {
			return err
		}
		members[member] = memberRecord{status: memberPending, digest: digest, size: size}
	}
	return nil
}

// transfer commits every member that is not yet committed, in member order. It
// reports whether all four commits are confirmed in this attempt; a member whose
// earlier commit stayed uncertain is only adopted by presence, so it is not
// confirmed until the readback proves it.
func (r *Replicator) transfer(ctx context.Context, backup physicalstore.Backup, members map[string]memberRecord) (bool, error) {
	confirmed := true
	for _, member := range memberOrder {
		record := members[member]
		switch record.status {
		case memberUploaded, memberVerified:
			continue
		case memberUncertain:
			present, err := r.adoptUncertain(ctx, backup, member)
			if err != nil {
				return false, err
			}
			if present {
				// Keep the uncertain state: only the readback may prove this key.
				confirmed = false
				continue
			}
			record.status = memberPending
		case memberPending:
		default:
			return false, terminal(CodeStateInvalid, fmt.Errorf("replication: unknown member state %q", record.status))
		}
		uncertain, err := r.upload(ctx, backup, member, record.digest)
		if err != nil {
			return false, err
		}
		if uncertain {
			// A final key this attempt did not publish is accepted only by the readback.
			members[member] = memberRecord{status: memberUncertain, digest: record.digest, size: record.size}
			confirmed = false
			continue
		}
		members[member] = memberRecord{status: memberUploaded, digest: record.digest, size: record.size}
	}
	return confirmed, nil
}

// adoptUncertain decides an uncertain commit by presence only. An existing final
// key is accepted solely by the later verification readback; a missing key needs
// a new temporary part, because the previous part may already be gone.
func (r *Replicator) adoptUncertain(ctx context.Context, backup physicalstore.Backup, member string) (bool, error) {
	key, err := r.objectKey(backup.ObjectPath, member)
	if err != nil {
		return false, terminal(CodeStateInvalid, err)
	}
	if _, err = r.Remote.Size(ctx, key); err == nil {
		return true, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, classify(err)
	}
	if err = r.setMember(ctx, backup.ID, member, memberPending); err != nil {
		return false, err
	}
	return false, nil
}

// upload streams one local member into a fresh remote temporary object and
// commits it. It reports true when the final key already exists, which is never
// overwritten and never claimed as uploaded by this attempt.
func (r *Replicator) upload(ctx context.Context, backup physicalstore.Backup, member, digest string) (bool, error) {
	key, err := r.objectKey(backup.ObjectPath, member)
	if err != nil {
		return false, terminal(CodeStateInvalid, err)
	}
	sink, err := r.Remote.Create(ctx, key)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return true, r.markUncertain(ctx, backup.ID, member)
		}
		return false, classify(err)
	}
	return false, r.send(ctx, sink, backup.ID, backup.ObjectPath, member, digest)
}

// send writes the local member, commits the sink, and resolves the part row.
//
// The part obligation is recorded before the first payload byte is written, so a
// crash leaves a durable cleanup handoff rather than an untracked object.
func (r *Replicator) send(ctx context.Context, sink Sink, backupID, objectPath, member, digest string) error {
	part := sink.TemporaryPath()
	if part == "" {
		return errors.Join(terminal(CodeStateInvalid, errors.New("replication: remote sink has no temporary part")), sink.Abort())
	}
	partID, err := r.recordPart(ctx, backupID, member, part)
	if err != nil {
		return errors.Join(err, sink.Abort())
	}
	if err = r.hook.call("part"); err != nil {
		return r.abandon(ctx, sink, partID, classify(err))
	}
	reader, err := r.Local.Open(objectPath + "/" + member)
	if err != nil {
		return r.abandon(ctx, sink, partID, terminal(CodeLocalMissing, errors.New("replication: local member is unreadable")))
	}
	copied, copyErr := io.Copy(sink, reader)
	closeErr := reader.Close()
	if err = errors.Join(copyErr, closeErr); err != nil {
		return r.abandon(ctx, sink, partID, classify(err))
	}
	// The part holds the real member bytes here and no commit has happened yet.
	if err = r.hook.call("write"); err != nil {
		return r.abandon(ctx, sink, partID, classify(err))
	}
	receipt, err := sink.Commit(ctx)
	if err != nil {
		if errors.Is(err, ftp.ErrCommitUncertain) {
			return r.keepUncertain(ctx, backupID, member, partID, sink.Abort(), err)
		}
		return r.abandon(ctx, sink, partID, classify(err))
	}
	if receipt.Size != copied {
		return r.abandon(ctx, sink, partID, terminal(CodeVerifyManifest, errors.New("replication: committed size differs from the local member")))
	}
	return r.committed(ctx, backupID, member, partID, digest, copied)
}

// committed records a successful member commit and drops its part obligation.
func (r *Replicator) committed(ctx context.Context, backupID, member string, partID int64, digest string, size int64) error {
	if err := r.updateMember(ctx, backupID, member, memberUploaded, digest, size); err != nil {
		return err
	}
	return replsql.New(r.DB).DeletePart(ctx, partID)
}

// keepUncertain records an uncertain commit and ends this attempt. The final key
// is never deleted or rewritten; only the readback may accept it.
func (r *Replicator) keepUncertain(ctx context.Context, backupID, member string, partID int64, cleanupErr, cause error) error {
	if err := r.retainPart(ctx, partID, cleanupErr); err != nil {
		return errors.Join(cause, err)
	}
	if err := r.markUncertain(ctx, backupID, member); err != nil {
		return errors.Join(cause, err)
	}
	return retryable(CodeCommitUncertain, cause)
}

// retainPart records the outcome of a failed commit. A leftover part keeps its
// row for a later CleanupPart retry; a cleaned part drops it.
func (r *Replicator) retainPart(ctx context.Context, partID int64, cleanupErr error) error {
	if cleanupErr == nil {
		return replsql.New(r.DB).DeletePart(ctx, partID)
	}
	err := replsql.New(r.DB).PartFailed(ctx, replsql.PartFailedParams{
		LastErrorCode: sql.NullString{String: CodeCleanupPending, Valid: true}, ID: partID,
	})
	return errors.Join(err, cleanupErr)
}

// abandon aborts a failed attempt, keeps or drops its part, and reports the cause.
func (r *Replicator) abandon(ctx context.Context, sink Sink, partID int64, cause error) error {
	if err := r.retainPart(ctx, partID, sink.Abort()); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// markUncertain records a member whose publication outcome is unknown.
func (r *Replicator) markUncertain(ctx context.Context, backupID, member string) error {
	return r.setMember(ctx, backupID, member, memberUncertain)
}

func (r *Replicator) setMember(ctx context.Context, backupID, member, status string) error {
	return replsql.New(r.DB).SetMemberStatus(ctx, replsql.SetMemberStatusParams{
		Status: status, UpdatedAt: stamp(r.clock()), BackupID: backupID,
		RepositoryID: r.RepositoryID, Member: member,
	})
}

func (r *Replicator) updateMember(ctx context.Context, backupID, member, status, digest string, size int64) error {
	return replsql.New(r.DB).SetMemberCommitted(ctx, replsql.SetMemberCommittedParams{
		Status: status, UpdatedAt: stamp(r.clock()), BackupID: backupID,
		RepositoryID: r.RepositoryID, Member: member, Digest: digest, Size: size,
	})
}

// recordPart persists the temporary key before any payload byte is written.
func (r *Replicator) recordPart(ctx context.Context, backupID, member, part string) (int64, error) {
	q := replsql.New(r.DB)
	if err := q.InsertPart(ctx, replsql.InsertPartParams{
		BackupID: backupID, RepositoryID: r.RepositoryID, Member: member,
		Part: part, CreatedAt: stamp(r.clock()),
	}); err != nil {
		return 0, err
	}
	row, err := q.PartByPart(ctx, part)
	if err != nil {
		return 0, err
	}
	return row.ID, nil
}

// markUploaded requires all four members to be committed, then records uploaded
// on this replica's own row. The copy is committed but still unprotected until
// the readback proves it.
func (r *Replicator) markUploaded(ctx context.Context, backupID string, members map[string]memberRecord) error {
	for _, member := range memberOrder {
		switch members[member].status {
		case memberUploaded, memberVerified:
		default:
			return terminal(CodeStateInvalid, errors.New("replication: member is not committed"))
		}
	}
	now := r.clock()
	changed, err := replsql.New(r.DB).SetReplicaUploaded(ctx, replsql.SetReplicaUploadedParams{
		UploadedAt: nullableStamp(now), NextAttemptAt: stamp(now), UpdatedAt: stamp(now),
		BackupID: backupID, RepositoryID: r.RepositoryID, LocalRepositoryID: r.LocalRepositoryID,
	})
	if err != nil {
		return err
	}
	return oneRow(changed, "replica is not uploaded")
}

// classify converts a remote failure into a fixed, safe code.
func classify(err error) error {
	if err == nil {
		return nil
	}
	var failure *replicaFailure
	if errors.As(err, &failure) {
		return err
	}
	var remote *ftp.Error
	if errors.As(err, &remote) {
		return classifyFTP(remote, err)
	}
	switch {
	case errors.Is(err, fs.ErrPermission), errors.Is(err, fs.ErrClosed):
		return retryable(CodeRemoteOwnership, err)
	}
	return retryable(CodeRemoteTransport, err)
}

func classifyFTP(remote *ftp.Error, err error) error {
	switch remote.Kind {
	case "quota":
		return retryable(CodeRemoteQuota, err)
	case "ownership":
		return retryable(CodeRemoteOwnership, err)
	case "invalid", "exists":
		return terminal(CodeRemoteConflict, err)
	case "reply", "short_write":
		return retryable(CodeRemoteReply, err)
	case "commit_uncertain":
		return retryable(CodeCommitUncertain, err)
	}
	return retryable(CodeRemoteTransport, err)
}

// replyCode extracts a safe numeric FTP reply code, or zero.
func replyCode(err error) int {
	var remote *ftp.Error
	if errors.As(err, &remote) {
		return remote.ReplyCode
	}
	return 0
}
