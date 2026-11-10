-- name: GetInstance :one
SELECT id, created_at FROM instance WHERE singleton = 1;

-- name: InsertInstance :exec
INSERT INTO instance (singleton, id, created_at) VALUES (1, sqlc.arg(id), sqlc.arg(created_at));

-- name: HasUsers :one
SELECT EXISTS (SELECT 1 FROM users);

-- name: GetSetupCompleted :one
SELECT completed FROM setup_state WHERE singleton = 1;

-- name: LockSetupState :one
SELECT completed FROM setup_state WHERE singleton = 1 FOR UPDATE;

-- name: CompleteSetup :exec
UPDATE setup_state SET completed = TRUE WHERE singleton = 1;

-- name: CreateUser :one
INSERT INTO users (id, username, password_hash, role, created_at, updated_at)
VALUES (sqlc.arg(id), sqlc.arg(username), sqlc.arg(password_hash), sqlc.arg(role), sqlc.arg(created_at), sqlc.arg(updated_at)) RETURNING *;

-- name: GetUser :one
SELECT * FROM users WHERE id = sqlc.arg(id);

-- name: GetUserByUsername :one
SELECT * FROM users WHERE translate(username, 'ABCDEFGHIJKLMNOPQRSTUVWXYZ', 'abcdefghijklmnopqrstuvwxyz') = translate(sqlc.arg(username), 'ABCDEFGHIJKLMNOPQRSTUVWXYZ', 'abcdefghijklmnopqrstuvwxyz');

-- name: ListUsers :many
SELECT * FROM users ORDER BY translate(username, 'ABCDEFGHIJKLMNOPQRSTUVWXYZ', 'abcdefghijklmnopqrstuvwxyz'), id;

-- name: UpdateUser :one
UPDATE users SET
    role = COALESCE(sqlc.narg(role), role),
    disabled = COALESCE(sqlc.narg(disabled), disabled),
    password_hash = COALESCE(sqlc.narg(password_hash), password_hash),
    session_epoch = session_epoch + 1,
    updated_at = sqlc.arg(updated_at)
WHERE users.id = sqlc.arg(user_id)
AND NOT (
    role = 'admin' AND disabled = 0
    AND (COALESCE(sqlc.narg(role), role) <> 'admin' OR COALESCE(sqlc.narg(disabled), disabled) = 1)
    AND (SELECT COUNT(*) FROM users WHERE role = 'admin' AND disabled = 0) = 1
)
RETURNING *;

-- name: ChangePassword :one
UPDATE users SET password_hash = sqlc.arg(password_hash), session_epoch = session_epoch + 1, updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id) AND session_epoch = sqlc.arg(session_epoch) AND disabled = 0 RETURNING *;

-- name: RevokeUserSessions :one
UPDATE users SET session_epoch = session_epoch + 1, updated_at = sqlc.arg(updated_at) WHERE id = sqlc.arg(id) RETURNING *;

-- name: FindSession :one
SELECT data FROM sessions WHERE token = sqlc.arg(token) AND expiry > sqlc.arg(expiry);

-- name: GetSessionExpiry :one
SELECT expiry FROM sessions WHERE token = sqlc.arg(token);

-- name: TombstoneActive :one
SELECT expiry FROM session_tombstones WHERE token = sqlc.arg(token) AND expiry > sqlc.arg(expiry);

-- name: CommitSession :exec
INSERT INTO sessions (token, data, expiry) VALUES (sqlc.arg(token), sqlc.arg(data), sqlc.arg(expiry))
ON CONFLICT (token) DO UPDATE SET data = excluded.data, expiry = excluded.expiry;

-- name: DeleteSession :exec
DELETE FROM sessions WHERE token = sqlc.arg(token);

-- name: UpsertSessionTombstone :exec
INSERT INTO session_tombstones (token, expiry) VALUES (sqlc.arg(token), sqlc.arg(expiry))
ON CONFLICT (token) DO UPDATE SET expiry = GREATEST(session_tombstones.expiry, excluded.expiry);

-- name: DeleteExpiredSessions :exec
DELETE FROM sessions WHERE expiry <= sqlc.arg(expiry);

-- name: DeleteExpiredTombstones :exec
DELETE FROM session_tombstones WHERE expiry <= sqlc.arg(expiry);

-- name: InsertAuditLog :exec
INSERT INTO audit_logs (
    ts, actor_type, actor_id, actor_name, action, resource_type, resource_id,
    ip, result, status, request_id, detail_json
) VALUES (sqlc.arg(ts), sqlc.arg(actor_type), sqlc.arg(actor_id), sqlc.arg(actor_name), sqlc.arg(action), sqlc.arg(resource_type), sqlc.arg(resource_id), sqlc.arg(ip), sqlc.arg(result), sqlc.arg(status), sqlc.arg(request_id), sqlc.arg(detail_json));

-- name: ListAuditLogs :many
SELECT * FROM audit_logs WHERE id < sqlc.arg(id) ORDER BY id DESC LIMIT sqlc.arg(row_limit)::bigint;

-- name: CreateRepository :one
INSERT INTO repositories (id, name, kind, config_json, secret_ciphertext, tls_mode, created_at, updated_at)
VALUES (sqlc.arg(id), sqlc.arg(name), sqlc.arg(kind), sqlc.arg(config_json), sqlc.arg(secret_ciphertext), sqlc.arg(tls_mode), sqlc.arg(created_at), sqlc.arg(updated_at)) RETURNING *;

-- name: GetRepository :one
SELECT * FROM repositories WHERE id = sqlc.arg(id);

-- name: ListRepositories :many
SELECT * FROM repositories WHERE (CAST(sqlc.arg(before) AS TEXT) = '' OR id < CAST(sqlc.arg(before) AS TEXT))
ORDER BY id DESC LIMIT sqlc.arg(row_limit)::bigint;

-- name: UpdateRepository :one
-- kind and tls_mode are immutable; the secret is replaced only when supplied.
UPDATE repositories SET
    name = sqlc.arg(name),
    config_json = sqlc.arg(config_json),
    secret_ciphertext = COALESCE(sqlc.narg(secret_ciphertext), secret_ciphertext),
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id) RETURNING *;

-- name: DeleteRepository :exec
DELETE FROM repositories WHERE id = sqlc.arg(id);

-- name: CountPoliciesByRepository :one
SELECT COUNT(*) FROM policies WHERE local_repository_id = sqlc.arg(localRepositoryID) OR replica_repository_id = sqlc.arg(localRepositoryID);

-- name: CreateSource :one
INSERT INTO sources (id, name, socket, datadir, username, secret_ciphertext, created_at, updated_at)
VALUES (sqlc.arg(id), sqlc.arg(name), sqlc.arg(socket), sqlc.arg(datadir), sqlc.arg(username), sqlc.arg(secret_ciphertext), sqlc.arg(created_at), sqlc.arg(updated_at)) RETURNING *;

-- name: GetSource :one
SELECT * FROM sources WHERE id = sqlc.arg(id);

-- name: ListSources :many
SELECT * FROM sources WHERE (CAST(sqlc.arg(before) AS TEXT) = '' OR id < CAST(sqlc.arg(before) AS TEXT))
ORDER BY id DESC LIMIT sqlc.arg(row_limit)::bigint;

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
DELETE FROM sources WHERE id = sqlc.arg(id);

-- name: CountPoliciesBySource :one
SELECT COUNT(*) FROM policies WHERE source_id = sqlc.arg(sourceID);

-- name: CreatePolicy :one
INSERT INTO policies (
    id, name, source_id, local_repository_id, replica_repository_id, full_cron,
    incremental_cron, timezone, max_chain_length, parallel, throttle, zstd_level,
    page_tracking, enabled, last_preflight_json, created_at, updated_at
) VALUES (sqlc.arg(id), sqlc.arg(name), sqlc.arg(source_id), sqlc.arg(local_repository_id), sqlc.arg(replica_repository_id), sqlc.arg(full_cron), sqlc.arg(incremental_cron), sqlc.arg(timezone), sqlc.arg(max_chain_length), sqlc.arg(parallel), sqlc.arg(throttle), sqlc.arg(zstd_level), sqlc.arg(page_tracking), sqlc.arg(enabled), sqlc.arg(last_preflight_json), sqlc.arg(created_at), sqlc.arg(updated_at)) RETURNING *;

-- name: GetPolicy :one
SELECT * FROM policies WHERE id = sqlc.arg(id);

-- name: ListPolicies :many
SELECT * FROM policies WHERE (CAST(sqlc.arg(before) AS TEXT) = '' OR id < CAST(sqlc.arg(before) AS TEXT))
ORDER BY id DESC LIMIT sqlc.arg(row_limit)::bigint;

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
DELETE FROM policies WHERE id = sqlc.arg(id);

-- name: GetPolicySchedule :one
SELECT next_full_at, next_incremental_at FROM policies WHERE id = sqlc.arg(id);

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
) VALUES (sqlc.arg(id), sqlc.arg(policy_id), sqlc.arg(source_id), sqlc.arg(kind), sqlc.arg(trigger), sqlc.arg(idempotency_key), sqlc.arg(request_hash), 'queued', sqlc.arg(scheduled_at), sqlc.arg(created_at), sqlc.arg(actor_id)) RETURNING *;

-- name: GetRun :one
SELECT * FROM runs WHERE id = sqlc.arg(id);

-- name: GetRunByIdempotencyKey :one
SELECT * FROM runs WHERE idempotency_key = sqlc.arg(idempotencyKey);

-- name: ListRuns :many
SELECT * FROM runs
WHERE (CAST(sqlc.arg(before) AS TEXT) = '' OR id < CAST(sqlc.arg(before) AS TEXT))
    AND (CAST(sqlc.arg(policy_id) AS TEXT) = '' OR policy_id = CAST(sqlc.arg(policy_id) AS TEXT))
    AND (CAST(sqlc.arg(source_id) AS TEXT) = '' OR source_id = CAST(sqlc.arg(source_id) AS TEXT))
    AND (CAST(sqlc.arg(status) AS TEXT) = '' OR status = CAST(sqlc.arg(status) AS TEXT))
ORDER BY id DESC LIMIT sqlc.arg(row_limit)::bigint;

-- name: ClaimNextRun :one
-- Global backup concurrency is 1: a queued row is claimed only when no run is
-- already running. That is also per-source exclusion. One claimant at a time
-- holds the database write lock, so the loser's statement runs after the
-- winner's commit and no longer matches. No result means no claimable run.
--
-- The restore queue shares the same global slot, so a queued run is also
-- refused while any restore is running. Both claims read both tables inside
-- one transaction on the single owned PostgreSQL connection: the two queues can never run
-- a backup chain and a restore at the same time.
--
-- The claim also snapshots the policy's (local, replica) pair onto the row, in
-- the same statement that reads the policy, so the run and its replication
-- binding can never disagree: a later PUT moves the next run's pair only.
UPDATE runs SET
    status = 'running',
    started_at = sqlc.arg(started_at),
    local_repository_id = (SELECT p.local_repository_id FROM policies p WHERE p.id = runs.policy_id),
    replica_repository_id = (SELECT p.replica_repository_id FROM policies p WHERE p.id = runs.policy_id)
WHERE id = (
    SELECT id FROM runs WHERE status = 'queued' AND cancel_requested = 0
    ORDER BY kind = 'full' DESC, scheduled_at, id LIMIT 1
) AND status = 'queued'
AND NOT EXISTS (SELECT 1 FROM runs WHERE status = 'running')
AND NOT EXISTS (SELECT 1 FROM restores WHERE status = 'running')
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
INSERT INTO run_logs (run_id, seq, ts, level, event, message, data) VALUES (sqlc.arg(run_id), sqlc.arg(seq), sqlc.arg(ts), sqlc.arg(level), sqlc.arg(event), sqlc.arg(message), sqlc.arg(data));

-- name: CountRunLogs :one
SELECT COUNT(*) FROM run_logs WHERE run_id = sqlc.arg(runID);

-- name: GetRunLog :one
SELECT * FROM run_logs WHERE run_id = sqlc.arg(run_id) AND seq = sqlc.arg(seq);

-- name: ReplaceRunLog :execrows
UPDATE run_logs SET ts = sqlc.arg(ts), level = sqlc.arg(level), event = sqlc.arg(event), message = sqlc.arg(message), data = sqlc.arg(data)
WHERE run_id = sqlc.arg(run_id) AND seq = sqlc.arg(seq);

-- name: RunLogsAfter :many
SELECT * FROM run_logs WHERE run_id = sqlc.arg(run_id) AND seq > sqlc.arg(seq) ORDER BY seq LIMIT sqlc.arg(row_limit)::bigint;

-- name: EnabledPolicies :many
SELECT * FROM policies WHERE enabled = 1 ORDER BY id;

-- name: MaxRunLogSeq :one
SELECT CAST(COALESCE(MAX(seq), 0) AS BIGINT) FROM run_logs WHERE run_id = sqlc.arg(runID);

-- name: RunByBackupID :one
SELECT * FROM runs WHERE backup_id = sqlc.arg(backupID) ORDER BY id DESC LIMIT 1;

-- name: CountActiveFullsForSource :one
SELECT COUNT(*) FROM runs
WHERE source_id = sqlc.arg(sourceID) AND kind = 'full' AND status IN ('queued', 'running');

-- name: PairBackups :many
-- Every backup published by a run claimed with exactly this
-- (local_repository_id, replica_repository_id) pair. A backup produced on
-- another local repository is never selected, so one replica repository shared
-- by two local repositories never queues the other's backup. The catch-up then
-- keeps only the backups that are still locally present: a pruned backup has
-- nothing to copy and must not warn on every scan.
SELECT DISTINCT r.backup_id AS backup_id
FROM runs r
WHERE r.local_repository_id = sqlc.arg(local_repository_id) AND r.replica_repository_id = sqlc.arg(replica_repository_id)
  AND r.backup_id IS NOT NULL;

-- name: CompletedRunReplicationPairs :many
-- The pairs completed runs were claimed with, kept while the run's snapshot
-- still names a replica repository that exists. These pairs stay installed so
-- their backups keep replicating after a policy edit.
SELECT DISTINCT r.local_repository_id AS local_repository_id, r.replica_repository_id AS replica_repository_id
FROM runs r
JOIN repositories lr ON lr.id = r.local_repository_id
JOIN repositories rr ON rr.id = r.replica_repository_id
WHERE r.status = 'completed' AND r.replica_repository_id IS NOT NULL
ORDER BY r.local_repository_id, r.replica_repository_id;

-- Restore queue (W5). The claim mirrors ClaimNextRun: a queued restore is
-- claimed only when no run and no other restore is running, so the two queues
-- share one global execution slot. The oldest queued row wins.
-- name: ClaimNextRestore :one
UPDATE restores SET status = 'running', started_at = sqlc.arg(started_at), phase = NULL
WHERE id = (
    SELECT id FROM restores WHERE status = 'queued' AND cancel_requested = 0
    ORDER BY created_at, id LIMIT 1
) AND status = 'queued'
AND NOT EXISTS (SELECT 1 FROM restores WHERE status = 'running')
AND NOT EXISTS (SELECT 1 FROM runs WHERE status = 'running')
RETURNING *;

-- name: InsertRestore :one
INSERT INTO restores (
    id, backup_id, status, plan_json, plan_digest, rehearse, verify_sql_json,
    idempotency_key, request_hash, requested_by, created_at, verify_status
) VALUES (sqlc.arg(id), sqlc.arg(backup_id), 'queued', sqlc.arg(plan_json), sqlc.arg(plan_digest), sqlc.arg(rehearse), sqlc.arg(verify_sql_json), sqlc.arg(idempotency_key), sqlc.arg(request_hash), sqlc.arg(requested_by), sqlc.arg(created_at), sqlc.arg(verify_status)) RETURNING *;

-- name: GetRestore :one
SELECT * FROM restores WHERE id = sqlc.arg(id);

-- name: GetRestoreByIdempotencyKey :one
SELECT * FROM restores WHERE idempotency_key = sqlc.arg(idempotencyKey);

-- name: ListRestores :many
SELECT * FROM restores
WHERE (CAST(sqlc.arg(before) AS TEXT) = '' OR id < CAST(sqlc.arg(before) AS TEXT))
    AND (CAST(sqlc.arg(status) AS TEXT) = '' OR status = CAST(sqlc.arg(status) AS TEXT))
    AND (CAST(sqlc.arg(backup_id) AS TEXT) = '' OR backup_id = CAST(sqlc.arg(backup_id) AS TEXT))
ORDER BY id DESC LIMIT sqlc.arg(row_limit)::bigint;

-- name: SetRestoreWorkDir :exec
UPDATE restores SET work_dir = sqlc.arg(work_dir) WHERE id = sqlc.arg(id) AND status = 'running';

-- name: UpdateRestorePhase :exec
UPDATE restores SET phase = sqlc.arg(phase) WHERE id = sqlc.arg(id) AND status = 'running';

-- name: RequestRestoreCancel :execrows
UPDATE restores SET cancel_requested = 1 WHERE id = sqlc.arg(id) AND status IN ('queued', 'running');

-- name: CancelQueuedRestore :execrows
-- Every terminal restore row carries the explicit side-effect observation: a
-- queued cancel never started the engine, so all three targets are recorded as
-- absent for this restore id.
UPDATE restores SET status = 'canceled', phase = NULL, finished_at = sqlc.arg(finished_at),
    side_effects_json = sqlc.arg(side_effects_json),
    verify_status = CASE WHEN rehearse = 1 THEN 'not_run' ELSE verify_status END
WHERE id = sqlc.arg(id) AND status = 'queued' AND cancel_requested = 0;

-- name: CompleteRestore :execrows
UPDATE restores SET
    status = 'completed', phase = NULL, finished_at = sqlc.arg(finished_at),
    export_path = sqlc.arg(export_path), export_bytes = sqlc.arg(export_bytes),
    side_effects_json = sqlc.arg(side_effects_json),
    verify_status = sqlc.arg(verify_status), verify_result_json = sqlc.narg(verify_result_json)
WHERE id = sqlc.arg(id) AND status = 'running';

-- name: FailRestore :execrows
UPDATE restores SET
    status = 'failed', phase = NULL, finished_at = sqlc.arg(finished_at),
    error_class = sqlc.arg(error_class), error_code = sqlc.narg(error_code),
    side_effects_json = sqlc.arg(side_effects_json),
    verify_status = sqlc.arg(verify_status), verify_result_json = sqlc.narg(verify_result_json)
WHERE id = sqlc.arg(id) AND status = 'running';

-- name: CancelRunningRestore :execrows
UPDATE restores SET
    status = 'canceled', phase = NULL, finished_at = sqlc.arg(finished_at),
    error_class = sqlc.arg(error_class), error_code = sqlc.narg(error_code),
    side_effects_json = sqlc.arg(side_effects_json),
    verify_status = sqlc.arg(verify_status), verify_result_json = sqlc.narg(verify_result_json)
WHERE id = sqlc.arg(id) AND status = 'running';

-- name: FailInterruptedRestore :execrows
UPDATE restores SET
    status = 'failed', phase = NULL, finished_at = sqlc.arg(finished_at),
    error_class = 'interrupted', error_code = 'interrupted',
    side_effects_json = sqlc.arg(side_effects_json),
    verify_status = CASE WHEN rehearse = 1 THEN 'not_run' ELSE verify_status END
WHERE id = sqlc.arg(id) AND status = 'running';

-- name: InterruptedRestores :many
SELECT * FROM restores WHERE status = 'running';

-- name: InsertRestoreLog :exec
INSERT INTO restore_logs (restore_id, seq, ts, level, message, data) VALUES (sqlc.arg(restore_id), sqlc.arg(seq), sqlc.arg(ts), sqlc.arg(level), sqlc.arg(message), sqlc.arg(data));

-- name: CountRestoreLogs :one
SELECT COUNT(*) FROM restore_logs WHERE restore_id = sqlc.arg(restoreID);

-- name: GetRestoreLog :one
SELECT * FROM restore_logs WHERE restore_id = sqlc.arg(restore_id) AND seq = sqlc.arg(seq);

-- name: ReplaceRestoreLog :execrows
UPDATE restore_logs SET ts = sqlc.arg(ts), level = sqlc.arg(level), message = sqlc.arg(message), data = sqlc.arg(data)
WHERE restore_id = sqlc.arg(restore_id) AND seq = sqlc.arg(seq);

-- name: RestoreLogsAfter :many
SELECT * FROM restore_logs WHERE restore_id = sqlc.arg(restore_id) AND seq > sqlc.arg(seq) ORDER BY seq LIMIT sqlc.arg(row_limit)::bigint;

-- name: MaxRestoreLogSeq :one
SELECT CAST(COALESCE(MAX(seq), 0) AS BIGINT) FROM restore_logs WHERE restore_id = sqlc.arg(restoreID);
