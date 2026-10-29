-- +goose Up
-- +goose NO TRANSACTION
-- S2: an FTP repository may now be FTPS. tls_mode gains the exact values
-- "explicit" and "implicit"; the trust bundle lives inside config_json as the
-- public ca_pem field, because a CA certificate is not a secret and must be
-- readable by every component that opens the repository.
--
-- SQLite cannot relax a CHECK in place, so the table is rebuilt with the
-- documented procedure: foreign keys off, new shape, copy every row, drop the
-- old table, rename, foreign keys on. The pragma is a no-op inside a
-- transaction, which is why this migration declares NO TRANSACTION and sets it
-- outside an explicit BEGIN/COMMIT: the rebuild itself is still one atomic
-- transaction, so a failed copy or a crash between DROP and RENAME leaves the
-- original table in place and a retry starts from the same state. The store
-- runs on a single connection (SetMaxOpenConns(1)), so every statement here
-- shares it. With foreign keys off the rename cannot rewrite the REFERENCES
-- clauses policies already hold on this table. Column names, order and types
-- are unchanged, so every existing query keeps working.
PRAGMA foreign_keys=OFF;
BEGIN;
CREATE TABLE repositories_ftps (
    id TEXT PRIMARY KEY NOT NULL,
    name TEXT NOT NULL UNIQUE COLLATE NOCASE,
    kind TEXT NOT NULL CHECK (kind IN ('local', 'ftp')),
    config_json TEXT NOT NULL,
    secret_ciphertext BLOB,
    tls_mode TEXT NOT NULL CHECK (tls_mode IN ('none', 'plain', 'explicit', 'implicit')),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    CHECK (
        (kind = 'local' AND tls_mode = 'none' AND secret_ciphertext IS NULL)
        OR (kind = 'ftp' AND tls_mode IN ('plain', 'explicit', 'implicit')
            AND secret_ciphertext IS NOT NULL AND length(secret_ciphertext) > 0)
    )
);
INSERT INTO repositories_ftps (id, name, kind, config_json, secret_ciphertext, tls_mode, created_at, updated_at)
    SELECT id, name, kind, config_json, secret_ciphertext, tls_mode, created_at, updated_at FROM repositories;
DROP TABLE repositories;
ALTER TABLE repositories_ftps RENAME TO repositories;
COMMIT;
PRAGMA foreign_keys=ON;

-- +goose Down
-- +goose NO TRANSACTION
-- The downgrade restores the plain-only shape. An FTPS row cannot be expressed
-- in it, so the copy fails with a CHECK violation instead of silently
-- downgrading a protected repository to plaintext or dropping its data; the
-- explicit transaction rolls the half-built copy back with it.
PRAGMA foreign_keys=OFF;
BEGIN;
CREATE TABLE repositories_plain (
    id TEXT PRIMARY KEY NOT NULL,
    name TEXT NOT NULL UNIQUE COLLATE NOCASE,
    kind TEXT NOT NULL CHECK (kind IN ('local', 'ftp')),
    config_json TEXT NOT NULL,
    secret_ciphertext BLOB,
    tls_mode TEXT NOT NULL CHECK (tls_mode IN ('none', 'plain')),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    CHECK (
        (kind = 'local' AND tls_mode = 'none' AND secret_ciphertext IS NULL)
        OR (kind = 'ftp' AND tls_mode = 'plain'
            AND secret_ciphertext IS NOT NULL AND length(secret_ciphertext) > 0)
    )
);
INSERT INTO repositories_plain (id, name, kind, config_json, secret_ciphertext, tls_mode, created_at, updated_at)
    SELECT id, name, kind, config_json, secret_ciphertext, tls_mode, created_at, updated_at FROM repositories;
DROP TABLE repositories;
ALTER TABLE repositories_plain RENAME TO repositories;
COMMIT;
PRAGMA foreign_keys=ON;
