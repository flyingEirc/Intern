-- name: GetInstance :one
SELECT id, created_at FROM instance WHERE singleton = 1;

-- name: InsertInstance :exec
INSERT INTO instance (singleton, id, created_at) VALUES (1, ?, ?);
