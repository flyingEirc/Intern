-- +goose Up
-- Backend repositories, MySQL sources and backup policies.
--
-- A repository is either a private local directory (tls_mode none, no secret)
-- or a plain FTP target (tls_mode plain, sealed password). The CHECK keeps those
-- two shapes inseparable in storage; FTPS values are rejected by the schema, not
-- silently downgraded. Sealed secrets never appear in config_json.
CREATE TABLE repositories (
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

-- A source always carries a sealed password; there is no secret-less source.
-- server_uuid and server_version are the identity observed by the last preflight.
CREATE TABLE sources (
    id TEXT PRIMARY KEY NOT NULL,
    name TEXT NOT NULL UNIQUE COLLATE NOCASE,
    socket TEXT NOT NULL,
    datadir TEXT NOT NULL,
    username TEXT NOT NULL,
    secret_ciphertext BLOB NOT NULL CHECK (length(secret_ciphertext) > 0),
    server_uuid TEXT,
    server_version TEXT,
    last_preflight_json TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

-- The physical chains.source_id and replication replicas.repository_id columns
-- stay opaque text: those schemas belong to other modules and are not rebuilt
-- here. Referential integrity to them is enforced when a source or repository is
-- deleted, so no dangling reference can be created through the API.
CREATE TABLE policies (
    id TEXT PRIMARY KEY NOT NULL,
    name TEXT NOT NULL UNIQUE COLLATE NOCASE,
    source_id TEXT NOT NULL REFERENCES sources(id),
    local_repository_id TEXT NOT NULL REFERENCES repositories(id),
    replica_repository_id TEXT REFERENCES repositories(id),
    full_cron TEXT NOT NULL,
    incremental_cron TEXT NOT NULL,
    timezone TEXT NOT NULL,
    max_chain_length INTEGER NOT NULL CHECK (max_chain_length >= 1),
    parallel INTEGER NOT NULL CHECK (parallel >= 1),
    throttle INTEGER NOT NULL CHECK (throttle >= 0),
    zstd_level INTEGER NOT NULL CHECK (zstd_level BETWEEN 1 AND 22),
    page_tracking INTEGER NOT NULL DEFAULT 0 CHECK (page_tracking IN (0, 1)),
    enabled INTEGER NOT NULL DEFAULT 0 CHECK (enabled IN (0, 1)),
    last_preflight_json TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX policies_source ON policies (source_id);
CREATE INDEX policies_local_repository ON policies (local_repository_id);
CREATE INDEX policies_replica_repository ON policies (replica_repository_id);

-- +goose Down
DROP TABLE policies;
DROP TABLE sources;
DROP TABLE repositories;
