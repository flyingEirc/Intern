-- name: GetInstance :one
SELECT id, created_at FROM instance WHERE singleton = 1;

-- name: InsertInstance :exec
INSERT INTO instance (singleton, id, created_at) VALUES (1, ?, ?);

-- name: CreateUser :one
INSERT INTO users (id, username, password_hash, role, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?) RETURNING *;

-- name: GetUser :one
SELECT * FROM users WHERE id = ?;

-- name: GetUserByUsername :one
SELECT * FROM users WHERE username = ?;

-- name: ListUsers :many
SELECT * FROM users ORDER BY username COLLATE NOCASE, id;

-- name: UpdateUser :one
UPDATE users SET
    role = COALESCE(sqlc.narg(role), role),
    disabled = COALESCE(sqlc.narg(disabled), disabled),
    password_hash = COALESCE(sqlc.narg(password_hash), password_hash),
    session_epoch = session_epoch + 1,
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id)
AND NOT (
    role = 'admin' AND disabled = 0
    AND (COALESCE(?1, role) <> 'admin' OR COALESCE(?2, disabled) = 1)
    AND (SELECT COUNT(*) FROM users WHERE role = 'admin' AND disabled = 0) = 1
)
RETURNING *;

-- name: ChangePassword :one
UPDATE users SET password_hash = ?, session_epoch = session_epoch + 1, updated_at = ?
WHERE id = ? AND session_epoch = ? AND disabled = 0 RETURNING *;

-- name: RevokeUserSessions :one
UPDATE users SET session_epoch = session_epoch + 1, updated_at = ? WHERE id = ? RETURNING *;

-- name: FindSession :one
SELECT data FROM sessions WHERE token = ? AND expiry > ?;

-- name: GetSessionExpiry :one
SELECT expiry FROM sessions WHERE token = ?;

-- name: TombstoneActive :one
SELECT expiry FROM session_tombstones WHERE token = ? AND expiry > ?;

-- name: CommitSession :exec
INSERT INTO sessions (token, data, expiry) VALUES (?, ?, ?)
ON CONFLICT (token) DO UPDATE SET data = excluded.data, expiry = excluded.expiry;

-- name: DeleteSession :exec
DELETE FROM sessions WHERE token = ?;

-- name: UpsertSessionTombstone :exec
INSERT INTO session_tombstones (token, expiry) VALUES (?, ?)
ON CONFLICT (token) DO UPDATE SET expiry = MAX(session_tombstones.expiry, excluded.expiry);

-- name: DeleteExpiredSessions :exec
DELETE FROM sessions WHERE expiry <= ?;

-- name: DeleteExpiredTombstones :exec
DELETE FROM session_tombstones WHERE expiry <= ?;

-- name: InsertAuditLog :exec
INSERT INTO audit_logs (
    ts, actor_type, actor_id, actor_name, action, resource_type, resource_id,
    ip, result, status, request_id, detail_json
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListAuditLogs :many
SELECT * FROM audit_logs WHERE id < ? ORDER BY id DESC LIMIT ?;

-- name: CreateRepository :one
INSERT INTO repositories (id, name, kind, config_json, secret_ciphertext, tls_mode, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?) RETURNING *;

-- name: GetRepository :one
SELECT * FROM repositories WHERE id = ?;

-- name: ListRepositories :many
SELECT * FROM repositories WHERE (CAST(sqlc.arg(before) AS TEXT) = '' OR id < CAST(sqlc.arg(before) AS TEXT))
ORDER BY id DESC LIMIT sqlc.arg(row_limit);

-- name: UpdateRepository :one
-- kind and tls_mode are immutable; the secret is replaced only when supplied.
UPDATE repositories SET
    name = sqlc.arg(name),
    config_json = sqlc.arg(config_json),
    secret_ciphertext = COALESCE(sqlc.narg(secret_ciphertext), secret_ciphertext),
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id) RETURNING *;

-- name: DeleteRepository :exec
DELETE FROM repositories WHERE id = ?;

-- name: CountPoliciesByRepository :one
SELECT COUNT(*) FROM policies WHERE local_repository_id = ?1 OR replica_repository_id = ?1;

-- name: CreateSource :one
INSERT INTO sources (id, name, socket, datadir, username, secret_ciphertext, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?) RETURNING *;

-- name: GetSource :one
SELECT * FROM sources WHERE id = ?;

-- name: ListSources :many
SELECT * FROM sources WHERE (CAST(sqlc.arg(before) AS TEXT) = '' OR id < CAST(sqlc.arg(before) AS TEXT))
ORDER BY id DESC LIMIT sqlc.arg(row_limit);

-- name: UpdateSource :one
-- The sealed password is replaced only when supplied.
UPDATE sources SET
    name = sqlc.arg(name),
    socket = sqlc.arg(socket),
    datadir = sqlc.arg(datadir),
    username = sqlc.arg(username),
    secret_ciphertext = COALESCE(sqlc.narg(secret_ciphertext), secret_ciphertext),
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id) RETURNING *;

-- name: RecordSourcePreflight :one
UPDATE sources SET
    server_uuid = COALESCE(sqlc.arg(server_uuid), server_uuid),
    server_version = COALESCE(sqlc.arg(server_version), server_version),
    last_preflight_json = sqlc.arg(last_preflight_json),
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id) RETURNING *;

-- name: DeleteSource :exec
DELETE FROM sources WHERE id = ?;

-- name: CountPoliciesBySource :one
SELECT COUNT(*) FROM policies WHERE source_id = ?;

-- name: CreatePolicy :one
INSERT INTO policies (
    id, name, source_id, local_repository_id, replica_repository_id, full_cron,
    incremental_cron, timezone, max_chain_length, parallel, throttle, zstd_level,
    page_tracking, enabled, last_preflight_json, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING *;

-- name: GetPolicy :one
SELECT * FROM policies WHERE id = ?;

-- name: ListPolicies :many
SELECT * FROM policies WHERE (CAST(sqlc.arg(before) AS TEXT) = '' OR id < CAST(sqlc.arg(before) AS TEXT))
ORDER BY id DESC LIMIT sqlc.arg(row_limit);

-- name: UpdatePolicy :one
-- PUT replaces every mutable field, so the replica repository can be cleared.
UPDATE policies SET
    name = sqlc.arg(name),
    source_id = sqlc.arg(source_id),
    local_repository_id = sqlc.arg(local_repository_id),
    replica_repository_id = sqlc.narg(replica_repository_id),
    full_cron = sqlc.arg(full_cron),
    incremental_cron = sqlc.arg(incremental_cron),
    timezone = sqlc.arg(timezone),
    max_chain_length = sqlc.arg(max_chain_length),
    parallel = sqlc.arg(parallel),
    throttle = sqlc.arg(throttle),
    zstd_level = sqlc.arg(zstd_level),
    page_tracking = sqlc.arg(page_tracking),
    enabled = sqlc.arg(enabled),
    last_preflight_json = sqlc.narg(last_preflight_json),
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id) RETURNING *;

-- name: SetPolicyEnabled :one
-- Enable persists the preflight that authorised it in the same statement.
UPDATE policies SET
    enabled = sqlc.arg(enabled),
    last_preflight_json = COALESCE(sqlc.narg(last_preflight_json), last_preflight_json),
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id) RETURNING *;

-- name: DeletePolicy :exec
DELETE FROM policies WHERE id = ?;

-- name: GetPolicySchedule :one
SELECT next_full_at, next_incremental_at FROM policies WHERE id = ?;

-- name: SetPolicySchedule :one
UPDATE policies SET
    next_full_at = sqlc.narg(next_full_at),
    next_incremental_at = sqlc.narg(next_incremental_at),
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id) RETURNING next_full_at, next_incremental_at;

-- name: DuePolicies :many
-- The scheduler looks at one column at a time; a NULL value means the schedule
-- was never initialized (for example a row created before migration 00005).
SELECT * FROM policies
WHERE enabled = 1 AND (next_full_at IS NOT NULL AND next_full_at <= sqlc.arg(due)
    OR next_incremental_at IS NOT NULL AND next_incremental_at <= sqlc.arg(due));

-- name: InsertRun :one
INSERT INTO runs (
    id, policy_id, source_id, kind, "trigger", idempotency_key, request_hash,
    status, scheduled_at, created_at, actor_id
) VALUES (?, ?, ?, ?, ?, ?, ?, 'queued', ?, ?, ?) RETURNING *;

-- name: GetRun :one
SELECT * FROM runs WHERE id = ?;

-- name: GetRunByIdempotencyKey :one
SELECT * FROM runs WHERE idempotency_key = ?;

-- name: ListRuns :many
SELECT * FROM runs
WHERE (CAST(sqlc.arg(before) AS TEXT) = '' OR id < CAST(sqlc.arg(before) AS TEXT))
    AND (CAST(sqlc.arg(policy_id) AS TEXT) = '' OR policy_id = CAST(sqlc.arg(policy_id) AS TEXT))
    AND (CAST(sqlc.arg(source_id) AS TEXT) = '' OR source_id = CAST(sqlc.arg(source_id) AS TEXT))
    AND (CAST(sqlc.arg(status) AS TEXT) = '' OR status = CAST(sqlc.arg(status) AS TEXT))
ORDER BY id DESC LIMIT sqlc.arg(row_limit);

-- name: ClaimNextRun :one
-- One claimant at a time holds the database write lock, so the UPDATE ... WHERE
-- status = 'queued' clause is the concurrency control: the loser's statement
-- runs after the winner's commit and no longer matches. No result means no
-- claimable run.
UPDATE runs SET status = 'running', started_at = sqlc.arg(started_at)
WHERE id = (
    SELECT id FROM runs WHERE status = 'queued' AND cancel_requested = 0
    ORDER BY kind = 'full' DESC, scheduled_at, id LIMIT 1
) AND status = 'queued'
RETURNING *;

-- name: CancelQueuedRun :execrows
UPDATE runs SET status = 'canceled', finished_at = sqlc.arg(finished_at)
WHERE id = sqlc.arg(id) AND status = 'queued' AND cancel_requested = 0;

-- name: RequestCancel :execrows
UPDATE runs SET cancel_requested = 1
WHERE id = sqlc.arg(id) AND status IN ('queued', 'running');

-- name: CompleteRun :execrows
UPDATE runs SET
    status = 'completed', finished_at = sqlc.arg(finished_at), backup_id = sqlc.arg(backup_id),
    chain_id = sqlc.arg(chain_id), payload_size = sqlc.arg(payload_size)
WHERE id = sqlc.arg(id) AND status = 'running';

-- name: FailRun :execrows
UPDATE runs SET
    status = 'failed', finished_at = sqlc.arg(finished_at),
    error_class = sqlc.arg(error_class), error_code = sqlc.narg(error_code)
WHERE id = sqlc.arg(id) AND status = 'running';

-- name: CancelRunningRun :execrows
UPDATE runs SET status = 'canceled', finished_at = sqlc.arg(finished_at)
WHERE id = sqlc.arg(id) AND status = 'running';

-- name: InterruptedRuns :many
SELECT * FROM runs WHERE status = 'running';

-- name: InsertRunLog :exec
INSERT INTO run_logs (run_id, seq, ts, level, event, message) VALUES (?, ?, ?, ?, ?, ?);

-- name: CountRunLogs :one
SELECT COUNT(*) FROM run_logs WHERE run_id = ?;

-- name: GetRunLog :one
SELECT * FROM run_logs WHERE run_id = ? AND seq = ?;

-- name: ReplaceRunLog :execrows
UPDATE run_logs SET ts = ?, level = ?, event = ?, message = ?
WHERE run_id = ? AND seq = ?;

-- name: RunLogsAfter :many
SELECT * FROM run_logs WHERE run_id = ? AND seq > ? ORDER BY seq LIMIT ?;

-- name: EnabledPolicies :many
SELECT * FROM policies WHERE enabled = 1 ORDER BY id;

-- name: MaxRunLogSeq :one
SELECT CAST(COALESCE(MAX(seq), 0) AS INTEGER) FROM run_logs WHERE run_id = ?;
