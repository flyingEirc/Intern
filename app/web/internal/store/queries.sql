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
