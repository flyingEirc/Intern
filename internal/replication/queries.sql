-- Replication queue reads and writes. These queries also read the physical
-- chains/backups tables, so this entry lists the physical schema as well.

-- name: InsertReplica :exec
-- A row written before the local repository was recorded has an empty value and
-- is inert. Binding it here keeps its uploaded state, attempt history and part
-- obligations instead of adding a duplicate queue entry beside it.
INSERT INTO replicas (backup_id, repository_id, local_repository_id, idem_key, status, next_attempt_at, created_at, updated_at)
VALUES (?, ?, ?, ?, 'pending', ?, ?, ?)
ON CONFLICT(backup_id, repository_id) DO UPDATE SET
    local_repository_id = excluded.local_repository_id
WHERE replicas.local_repository_id = '';

-- name: GetReplica :one
SELECT * FROM replicas WHERE backup_id = ? AND repository_id = ? AND local_repository_id = ?;

-- name: ReplicasForRepository :many
SELECT r.*, b.chain_id, b.created_at AS backup_created_at, b.seq AS chain_seq, c.source_id
FROM replicas r
JOIN backups b ON b.id = r.backup_id
JOIN chains c ON c.id = b.chain_id
WHERE r.repository_id = ? AND r.local_repository_id = ?
ORDER BY c.source_id, b.created_at, b.seq;

-- name: ReplicasBySource :many
SELECT r.*, b.chain_id, b.created_at AS backup_created_at, b.seq AS chain_seq, c.source_id
FROM replicas r
JOIN backups b ON b.id = r.backup_id
JOIN chains c ON c.id = b.chain_id
WHERE r.repository_id = ? AND r.local_repository_id = ? AND c.source_id = ?
ORDER BY b.created_at, b.seq;

-- name: ReplicasByRepositorySource :many
-- The union of one source's replica rows on one replica repository, across
-- every local repository that replicates to it. The order is chain position,
-- not verification time: the chain a backup belongs to started first, then the
-- backup's seq, so a caller reads one contiguous stream and a pair that stopped
-- producing cannot pin the point.
SELECT r.*, b.chain_id, b.created_at AS backup_created_at, b.seq AS chain_seq, c.source_id
FROM replicas r
JOIN backups b ON b.id = r.backup_id
JOIN chains c ON c.id = b.chain_id
WHERE r.repository_id = ? AND c.source_id = ?
ORDER BY c.started_at, c.id, b.seq;

-- name: ReplicasByBackupIDs :many
SELECT * FROM replicas
WHERE repository_id = ? AND local_repository_id = ? AND backup_id IN (sqlc.slice(backup_ids));

-- name: SourceChains :many
SELECT * FROM chains WHERE source_id = ? ORDER BY started_at, rowid;

-- name: CompletedBackupsBySource :many
SELECT b.* FROM backups b
JOIN chains c ON c.id = b.chain_id
WHERE c.source_id = ? AND b.status = 'completed' AND b.local_present = 1
ORDER BY b.created_at, b.seq;

-- name: SetReplicaUploading :execrows
UPDATE replicas SET status = 'uploading', updated_at = ?
WHERE backup_id = ? AND repository_id = ? AND local_repository_id = ?
  AND status IN ('pending', 'failed', 'uploading');

-- name: SetReplicaUploaded :execrows
-- uploaded keeps failed_since: the copy is committed but still unprotected until
-- the readback proves it, so the alert clock must not restart here. The row is
-- scoped to this (backup, repository, local repository) triple, so zero affected
-- rows is an error the caller must not ignore.
UPDATE replicas
SET status = 'uploaded', uploaded_at = COALESCE(uploaded_at, ?), next_attempt_at = ?, last_error_code = NULL,
    last_reply_code = NULL, updated_at = ?
WHERE backup_id = ? AND repository_id = ? AND local_repository_id = ?;

-- name: SetReplicaVerified :execrows
UPDATE replicas
SET status = 'verified', verified_at = ?, uploaded_at = COALESCE(uploaded_at, ?),
    permanent = 0, failed_since = NULL, alert_since = NULL, last_error_code = NULL,
    last_reply_code = NULL, updated_at = ?
WHERE backup_id = ? AND repository_id = ? AND local_repository_id = ?;

-- name: SetReplicaFailed :execrows
UPDATE replicas
SET status = ?, attempts = ?, permanent = ?, next_attempt_at = ?,
    failed_since = COALESCE(failed_since, ?), last_error_code = ?, last_reply_code = ?,
    updated_at = ?
WHERE backup_id = ? AND repository_id = ? AND local_repository_id = ?;

-- name: SetReplicaAlert :exec
UPDATE replicas SET alert_since = ?
WHERE backup_id = ? AND repository_id = ? AND local_repository_id = ? AND alert_since IS NULL;

-- name: ResetReplicaAttempt :execrows
-- A manual retry restarts the attempt budget and the schedule but keeps the
-- history counters: the item has been unprotected since failed_since. Zero
-- affected rows is sql.ErrNoRows for the caller: the pair has no such replica.
UPDATE replicas
SET status = 'pending', attempts = 0, permanent = 0, next_attempt_at = ?, updated_at = ?
WHERE backup_id = ? AND repository_id = ? AND local_repository_id = ?;

-- name: ResetUploadingReplicas :exec
UPDATE replicas SET status = 'pending', next_attempt_at = ?, updated_at = ?
WHERE repository_id = ? AND local_repository_id = ? AND status = 'uploading';

-- name: UpsertMember :exec
INSERT INTO replica_members (backup_id, repository_id, member, status, digest, size, updated_at)
VALUES (?, ?, ?, 'pending', ?, ?, ?)
ON CONFLICT(backup_id, repository_id, member) DO UPDATE SET
    digest = excluded.digest, size = excluded.size, updated_at = excluded.updated_at;

-- name: Members :many
SELECT * FROM replica_members WHERE backup_id = ? AND repository_id = ?;

-- name: SetMemberStatus :exec
UPDATE replica_members SET status = ?, updated_at = ?
WHERE backup_id = ? AND repository_id = ? AND member = ?;

-- name: SetMemberCommitted :exec
UPDATE replica_members SET status = ?, updated_at = ?
WHERE backup_id = ? AND repository_id = ? AND member = ? AND digest = ? AND size = ?;

-- name: InsertPart :exec
INSERT INTO replica_parts (backup_id, repository_id, member, part, created_at)
VALUES (?, ?, ?, ?, ?);

-- name: PartByPart :one
SELECT * FROM replica_parts WHERE part = ?;

-- name: Parts :many
-- A part belongs to one replica row, so joining that row scopes the cleanup to
-- this (local, remote) pair instead of every pair sharing the FTP repository.
SELECT p.* FROM replica_parts p
JOIN replicas r ON r.backup_id = p.backup_id AND r.repository_id = p.repository_id
WHERE p.repository_id = ? AND r.local_repository_id = ?
ORDER BY p.id;

-- name: DeletePart :exec
DELETE FROM replica_parts WHERE id = ?;

-- name: PartFailed :exec
UPDATE replica_parts SET attempts = attempts + 1, last_error_code = ? WHERE id = ?;

-- name: PendingLocalDeletesForPair :many
-- Recovery must reclaim each reservation in the local repository that holds the
-- object. A reservation only exists after every member was verified on this
-- replica repository, so its members always have a replica row for the pair; the
-- join is therefore exact, and a reservation belonging to another local
-- repository sharing this FTP repository is left to that pair.
SELECT l.id, l.backup_id, l.target FROM cleanup_ledger l
JOIN backups b ON b.id = l.backup_id
JOIN replicas r ON r.backup_id = l.backup_id AND r.repository_id = ?
WHERE r.local_repository_id = ? AND l.instance_id = ? AND l.kind = 'object' AND b.status = 'completed'
ORDER BY l.id;
