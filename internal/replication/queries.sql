-- Replication queue reads and writes. These queries also read the physical
-- chains/backups tables, so this entry lists the physical schema as well.

-- name: InsertReplica :exec
INSERT INTO replicas (backup_id, repository_id, idem_key, status, next_attempt_at, created_at, updated_at)
VALUES (?, ?, ?, 'pending', ?, ?, ?)
ON CONFLICT DO NOTHING;

-- name: GetReplica :one
SELECT * FROM replicas WHERE backup_id = ? AND repository_id = ?;

-- name: ReplicasForRepository :many
SELECT r.*, b.chain_id, b.created_at AS backup_created_at, b.seq AS chain_seq, c.source_id
FROM replicas r
JOIN backups b ON b.id = r.backup_id
JOIN chains c ON c.id = b.chain_id
WHERE r.repository_id = ?
ORDER BY c.source_id, b.created_at, b.seq;

-- name: ReplicasBySource :many
SELECT r.*, b.chain_id, b.created_at AS backup_created_at, b.seq AS chain_seq, c.source_id
FROM replicas r
JOIN backups b ON b.id = r.backup_id
JOIN chains c ON c.id = b.chain_id
WHERE r.repository_id = ? AND c.source_id = ?
ORDER BY b.created_at, b.seq;

-- name: ReplicasByBackupIDs :many
SELECT * FROM replicas WHERE repository_id = ? AND backup_id IN (sqlc.slice(backup_ids));

-- name: SourceChains :many
SELECT * FROM chains WHERE source_id = ? ORDER BY started_at, rowid;

-- name: CompletedBackupsBySource :many
SELECT b.* FROM backups b
JOIN chains c ON c.id = b.chain_id
WHERE c.source_id = ? AND b.status = 'completed' AND b.local_present = 1
ORDER BY b.created_at, b.seq;

-- name: SetReplicaUploading :execrows
UPDATE replicas SET status = 'uploading', updated_at = ?
WHERE backup_id = ? AND repository_id = ? AND status IN ('pending', 'failed', 'uploading');

-- name: SetReplicaUploaded :exec
-- uploaded keeps failed_since: the copy is committed but still unprotected until
-- the readback proves it, so the alert clock must not restart here.
UPDATE replicas
SET status = 'uploaded', uploaded_at = COALESCE(uploaded_at, ?), next_attempt_at = ?, last_error_code = NULL,
    last_reply_code = NULL, updated_at = ?
WHERE backup_id = ? AND repository_id = ?;

-- name: SetReplicaVerified :exec
UPDATE replicas
SET status = 'verified', verified_at = ?, uploaded_at = COALESCE(uploaded_at, ?),
    permanent = 0, failed_since = NULL, alert_since = NULL, last_error_code = NULL,
    last_reply_code = NULL, updated_at = ?
WHERE backup_id = ? AND repository_id = ?;

-- name: SetReplicaFailed :exec
UPDATE replicas
SET status = ?, attempts = ?, permanent = ?, next_attempt_at = ?,
    failed_since = COALESCE(failed_since, ?), last_error_code = ?, last_reply_code = ?,
    updated_at = ?
WHERE backup_id = ? AND repository_id = ?;

-- name: SetReplicaAlert :exec
UPDATE replicas SET alert_since = ?
WHERE backup_id = ? AND repository_id = ? AND alert_since IS NULL;

-- name: ResetReplicaAttempt :exec
-- A manual retry restarts the attempt budget and the schedule but keeps the
-- history counters: the item has been unprotected since failed_since.
UPDATE replicas
SET status = 'pending', attempts = 0, permanent = 0, next_attempt_at = ?, updated_at = ?
WHERE backup_id = ? AND repository_id = ?;

-- name: ResetUploadingReplicas :exec
UPDATE replicas SET status = 'pending', next_attempt_at = ?, updated_at = ?
WHERE repository_id = ? AND status = 'uploading';

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
SELECT * FROM replica_parts WHERE repository_id = ? ORDER BY id;

-- name: DeletePart :exec
DELETE FROM replica_parts WHERE id = ?;

-- name: PartFailed :exec
UPDATE replica_parts SET attempts = attempts + 1, last_error_code = ? WHERE id = ?;
