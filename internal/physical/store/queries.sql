-- name: LatestChain :one
SELECT * FROM chains WHERE source_id = sqlc.arg(sourceID) ORDER BY started_at DESC, id DESC LIMIT 1;

-- name: InsertChain :exec
INSERT INTO chains (id, source_id, server_uuid, server_version, tool_version, started_at) VALUES (sqlc.arg(id), sqlc.arg(source_id), sqlc.arg(server_uuid), sqlc.arg(server_version), sqlc.arg(tool_version), sqlc.arg(started_at));

-- name: GetBackup :one
SELECT * FROM backups WHERE id = sqlc.arg(id);

-- name: ChainBackups :many
SELECT * FROM backups WHERE chain_id = sqlc.arg(chainID) ORDER BY seq;

-- name: Chain :one
SELECT * FROM chains WHERE id = sqlc.arg(id);

-- name: SourceChains :many
SELECT * FROM chains WHERE source_id = sqlc.arg(sourceID) ORDER BY started_at, id;

-- name: SetChainPinned :execrows
UPDATE chains SET pinned = sqlc.arg(pinned) WHERE id = sqlc.arg(id);

-- name: ChainObjectCleanup :many
SELECT l.id, l.backup_id, l.target FROM cleanup_ledger l JOIN backups b ON b.id = l.backup_id
WHERE b.chain_id = sqlc.arg(chainID) AND l.kind = 'object' AND b.status = 'completed' ORDER BY l.id;

-- name: InstanceObjectCleanup :many
SELECT l.id, l.backup_id, l.target FROM cleanup_ledger l JOIN backups b ON b.id = l.backup_id
WHERE l.instance_id = sqlc.arg(instanceID) AND l.kind = 'object' AND b.status = 'completed' ORDER BY l.id;

-- name: ReserveChainObject :exec
INSERT INTO cleanup_ledger (backup_id, instance_id, run_id, kind, target, created_at)
VALUES (sqlc.arg(backup_id), sqlc.arg(instance_id), sqlc.arg(run_id), 'object', sqlc.arg(target), sqlc.arg(created_at)) ON CONFLICT(kind, target) DO NOTHING;

-- name: ClearChainLocalPresent :execrows
UPDATE backups SET local_present = 0 WHERE chain_id = sqlc.arg(chainID) AND local_present = 1;

-- name: LocalDeleteDone :exec
DELETE FROM cleanup_ledger WHERE id = sqlc.arg(id);

-- name: LocalDeleteFailed :exec
UPDATE cleanup_ledger SET attempts = attempts + 1, last_error_code = sqlc.arg(last_error_code) WHERE id = sqlc.arg(id);

-- name: InsertBackup :exec
INSERT INTO backups (id, chain_id, seq, parent_id, kind, status, instance_id, run_id, object_path, key_id, created_at)
VALUES (sqlc.arg(id), sqlc.arg(chain_id), sqlc.arg(seq), sqlc.arg(parent_id), sqlc.arg(kind), 'running', sqlc.arg(instance_id), sqlc.arg(run_id), sqlc.arg(object_path), sqlc.arg(key_id), sqlc.arg(created_at));

-- name: CompleteBackup :execrows
UPDATE backups SET status = 'completed', from_lsn = sqlc.arg(from_lsn), to_lsn = sqlc.arg(to_lsn), last_lsn = sqlc.arg(last_lsn), checkpoints = sqlc.arg(checkpoints), xtrabackup_info = sqlc.arg(xtrabackup_info), payload_size = sqlc.arg(payload_size), payload_sha256 = sqlc.arg(payload_sha256), metadata_size = sqlc.arg(metadata_size), metadata_sha256 = sqlc.arg(metadata_sha256), local_present = 1, completed_at = sqlc.arg(completed_at)
WHERE id = sqlc.arg(id) AND status = 'running';

-- name: FailBackup :execrows
UPDATE backups SET status = 'failed', failure_code = sqlc.arg(failure_code) WHERE id = sqlc.arg(id) AND status = 'running';

-- name: InsertCleanup :exec
INSERT INTO cleanup_ledger (backup_id, instance_id, run_id, kind, target, created_at) VALUES (sqlc.arg(backup_id), sqlc.arg(instance_id), sqlc.arg(run_id), sqlc.arg(kind), sqlc.arg(target), sqlc.arg(created_at));

-- name: DeleteBackupCleanup :exec
DELETE FROM cleanup_ledger WHERE backup_id = sqlc.arg(backupID);

-- name: PendingCleanup :many
SELECT l.* FROM cleanup_ledger l JOIN backups b ON b.id = l.backup_id
WHERE l.instance_id = sqlc.arg(instanceID) AND b.status != 'completed' ORDER BY l.id;

-- name: CleanupDone :exec
DELETE FROM cleanup_ledger WHERE id = sqlc.arg(id);

-- name: CleanupFailed :exec
UPDATE cleanup_ledger SET attempts = attempts + 1, last_error_code = sqlc.arg(last_error_code) WHERE id = sqlc.arg(id);
