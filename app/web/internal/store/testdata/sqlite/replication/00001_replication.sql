-- +goose Up
-- Replica state for one remote repository. The queue row is per
-- (backup, repository); idem_key makes Enqueue repeatable. repository_id stays
-- opaque text until W3 adds the repositories table, so it has no foreign key.
-- created_at/uploaded_at/verified_at are fixed-width UTC text like the physical
-- schema, so lexical order is chronological order.
CREATE TABLE replicas (
    backup_id TEXT NOT NULL REFERENCES backups(id),
    repository_id TEXT NOT NULL,
    idem_key TEXT NOT NULL UNIQUE,
    status TEXT NOT NULL CHECK (status IN ('pending', 'uploading', 'uploaded', 'verified', 'failed', 'exhausted')),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    -- A non-retryable failure (local corruption, remote conflict) also stops
    -- automatic attempts; status and error code stay observable.
    permanent INTEGER NOT NULL DEFAULT 0 CHECK (permanent IN (0, 1)),
    next_attempt_at TEXT NOT NULL,
    uploaded_at TEXT,
    verified_at TEXT,
    failed_since TEXT,
    alert_since TEXT,
    last_error_code TEXT,
    last_reply_code INTEGER,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (backup_id, repository_id)
);
CREATE INDEX replicas_repository_status ON replicas(repository_id, status, next_attempt_at);

-- One row per member. A member is never uploaded again once committed, and an
-- uncertain commit is accepted only by the verification readback.
CREATE TABLE replica_members (
    backup_id TEXT NOT NULL,
    repository_id TEXT NOT NULL,
    member TEXT NOT NULL CHECK (member IN ('payload.bin', 'envelope.json', 'metadata.bin', 'metadata.envelope.json')),
    status TEXT NOT NULL CHECK (status IN ('pending', 'uploaded', 'uncertain', 'verified')),
    digest TEXT NOT NULL,
    size INTEGER NOT NULL CHECK (size >= 0),
    updated_at TEXT NOT NULL,
    PRIMARY KEY (backup_id, repository_id, member),
    FOREIGN KEY (backup_id, repository_id) REFERENCES replicas(backup_id, repository_id) ON DELETE CASCADE
);

-- Every remote temporary key is recorded here before the first payload byte is
-- written, so an interrupted upload leaves a durable cleanup obligation. The
-- row is deleted only after a successful Commit or a successful CleanupPart.
CREATE TABLE replica_parts (
    id INTEGER PRIMARY KEY,
    backup_id TEXT NOT NULL,
    repository_id TEXT NOT NULL,
    member TEXT NOT NULL,
    part TEXT NOT NULL UNIQUE,
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error_code TEXT,
    created_at TEXT NOT NULL,
    FOREIGN KEY (backup_id, repository_id) REFERENCES replicas(backup_id, repository_id) ON DELETE CASCADE
);
CREATE INDEX replica_parts_repository ON replica_parts(repository_id, id);

-- +goose Down
DROP TABLE replica_parts;
DROP TABLE replica_members;
DROP TABLE replicas;
