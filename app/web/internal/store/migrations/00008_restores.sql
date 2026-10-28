-- +goose Up
-- Isolated restore executions (W5).
--
-- A restore is one explicit plan applied to an empty isolated target. The
-- plan_json snapshot is the plan the operator confirmed; plan_digest is the
-- sha256 of its canonical form, and execution re-reads the target from the
-- physical catalog and refuses unless a fresh plan has the same digest. Status
-- path: queued -> running -> completed | failed | canceled. A running row that
-- received a cancel request keeps cancel_requested = 1 through the frozen
-- "published export wins over cancel" transition.
--
-- The restore queue and the backup run queue are mutually exclusive: neither
-- claim matches while the other table has a running row, so the process can
-- never execute a restore and a backup chain at the same time.
--
-- verify_status is separate from the restore status on purpose: a rehearsal
-- that ran and failed does not make an otherwise published restore a failure.
-- side_effects_json records exactly what recovery removed or published; a
-- published export is never deleted, by recovery or by a cancel.
CREATE TABLE restores (
    id TEXT PRIMARY KEY NOT NULL,
    backup_id TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('queued', 'running', 'completed', 'failed', 'canceled')),
    phase TEXT,
    plan_json TEXT NOT NULL,
    plan_digest TEXT NOT NULL,
    rehearse INTEGER NOT NULL DEFAULT 0 CHECK (rehearse IN (0, 1)),
    verify_sql_json TEXT,
    idempotency_key TEXT UNIQUE,
    request_hash TEXT,
    requested_by TEXT NOT NULL REFERENCES users(id),
    work_dir TEXT,
    export_path TEXT,
    export_bytes INTEGER CHECK (export_bytes IS NULL OR export_bytes >= 0),
    created_at TEXT NOT NULL,
    started_at TEXT,
    finished_at TEXT,
    cancel_requested INTEGER NOT NULL DEFAULT 0 CHECK (cancel_requested IN (0, 1)),
    error_class TEXT,
    error_code TEXT,
    side_effects_json TEXT,
    verify_status TEXT NOT NULL DEFAULT 'not_requested'
        CHECK (verify_status IN ('not_requested', 'passed', 'failed')),
    verify_result_json TEXT
);

CREATE INDEX restores_claim ON restores (status, created_at);
CREATE INDEX restores_backup ON restores (backup_id);
CREATE INDEX restores_requested_by ON restores (requested_by);

-- Bounded per-restore event log. The 1000-row cap is enforced transactionally
-- by the store; the final row of a full log is the truncated marker. Messages
-- are fixed-format and redacted: no secrets, credentials, SQL text or paths
-- beyond the configured ones.
CREATE TABLE restore_logs (
    restore_id TEXT NOT NULL REFERENCES restores(id),
    seq INTEGER NOT NULL CHECK (seq >= 1),
    ts TEXT NOT NULL,
    level TEXT NOT NULL CHECK (level IN ('debug', 'info', 'warn', 'error')),
    message TEXT NOT NULL,
    data TEXT,
    PRIMARY KEY (restore_id, seq)
);

-- +goose Down
DROP TABLE restore_logs;
DROP TABLE restores;
