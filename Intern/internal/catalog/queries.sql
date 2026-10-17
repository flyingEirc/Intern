-- name: GetJob :one
SELECT * FROM jobs WHERE id = ?;

-- name: GetCompleted :one
SELECT * FROM jobs WHERE id = ? AND status = 'completed';

-- name: LatestCompleted :one
SELECT * FROM jobs WHERE source = ? AND status = 'completed' AND kind IN ('logical-full', 'binlog') ORDER BY sequence DESC LIMIT 1;

-- name: ListCompleted :many
SELECT * FROM jobs WHERE status = 'completed' ORDER BY sequence DESC;

-- name: ListPending :many
SELECT * FROM jobs WHERE status <> 'completed' ORDER BY sequence;

-- name: InsertJob :exec
INSERT INTO jobs (id,source,kind,parent,path,created_at,status) VALUES (?,?,?,?,?,?,'running');

-- name: RetryJob :execrows
UPDATE jobs SET status = 'running', parent = ?, created_at = ? WHERE id = ? AND status = 'failed';

-- name: CompleteJob :execrows
UPDATE jobs SET status = 'completed', manifest = ? WHERE id = ? AND status = 'running';

-- name: FailJob :execrows
UPDATE jobs SET status = 'failed' WHERE id = ? AND status = 'running';
