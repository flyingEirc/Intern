-- name: LatestChain :one
SELECT * FROM chains WHERE source_id = ? ORDER BY started_at DESC, rowid DESC LIMIT 1;

-- name: InsertChain :exec
INSERT INTO chains (id, source_id, server_uuid, server_version, tool_version, started_at) VALUES (?, ?, ?, ?, ?, ?);

-- name: GetBackup :one
SELECT * FROM backups WHERE id = ?;

-- name: ChainBackups :many
SELECT * FROM backups WHERE chain_id = ? ORDER BY seq;

-- name: InsertBackup :exec
INSERT INTO backups (id, chain_id, seq, parent_id, kind, status, instance_id, run_id, object_path, key_id, created_at)
VALUES (?, ?, ?, ?, ?, 'running', ?, ?, ?, ?, ?);

-- name: CompleteBackup :execrows
UPDATE backups SET status = 'completed', from_lsn = ?, to_lsn = ?, last_lsn = ?, checkpoints = ?, xtrabackup_info = ?, payload_size = ?, payload_sha256 = ?, metadata_size = ?, metadata_sha256 = ?, local_present = 1, completed_at = ?
WHERE id = ? AND status = 'running';

-- name: FailBackup :execrows
UPDATE backups SET status = 'failed', failure_code = ? WHERE id = ? AND status = 'running';

-- name: InsertCleanup :exec
INSERT INTO cleanup_ledger (backup_id, instance_id, run_id, kind, target, created_at) VALUES (?, ?, ?, ?, ?, ?);

-- name: DeleteBackupCleanup :exec
DELETE FROM cleanup_ledger WHERE backup_id = ?;

-- name: PendingCleanup :many
SELECT l.* FROM cleanup_ledger l JOIN backups b ON b.id = l.backup_id
WHERE l.instance_id = ? AND b.status != 'completed' ORDER BY l.id;

-- name: CleanupDone :exec
DELETE FROM cleanup_ledger WHERE id = ?;

-- name: CleanupFailed :exec
UPDATE cleanup_ledger SET attempts = attempts + 1, last_error_code = ? WHERE id = ?;
