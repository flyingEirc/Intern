-- +goose Up
-- Persistent run queue for the dispatcher (W4).
--
-- A run is one full or incremental backup attempt of one policy. The schedule
-- state lives on the policy: next_full_at and next_incremental_at are the
-- persisted next trigger times, initialized from the cron expressions when a
-- policy is enabled and advanced by the scheduler after every fire. Rows
-- created before this migration have NULL columns; the scheduler initializes
-- them on its first tick instead of firing immediately.
--
-- Status path: queued -> running -> completed | failed | canceled. There is no
-- finalizing status: the success commit is an interior step of the physical
-- chain Run call and is not observable from the control plane.
--
-- trigger admits admission for the coalesced full a failed incremental queues.
ALTER TABLE policies ADD COLUMN next_full_at TEXT;
ALTER TABLE policies ADD COLUMN next_incremental_at TEXT;

CREATE TABLE runs (
    id TEXT PRIMARY KEY NOT NULL,
    policy_id TEXT NOT NULL REFERENCES policies(id),
    source_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('full', 'incremental')),
    "trigger" TEXT NOT NULL CHECK ("trigger" IN ('cron', 'manual', 'admission')),
    -- Cron keys are policy:<id>:<kind>:<scheduled_at>, admission keys are
    -- policy:<id>:full:admission:<scheduled_at>, manual keys come from the
    -- Idempotency-Key request header.
    idempotency_key TEXT NOT NULL UNIQUE,
    request_hash TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('queued', 'running', 'completed', 'failed', 'canceled')),
    scheduled_at TEXT NOT NULL,
    created_at TEXT NOT NULL,
    started_at TEXT,
    finished_at TEXT,
    backup_id TEXT,
    chain_id TEXT,
    payload_size INTEGER,
    error_class TEXT,
    error_code TEXT,
    cancel_requested INTEGER NOT NULL DEFAULT 0 CHECK (cancel_requested IN (0, 1)),
    actor_id TEXT
);
-- No status/cancel_requested cross-column CHECK: a running run that receives a
-- cancel request keeps cancel_requested = 1 through the frozen "success point
-- wins over cancel" transition to completed, and restart recovery marks such
-- rows failed with the same flag set.

CREATE INDEX runs_claim ON runs (status, kind, scheduled_at);
CREATE INDEX runs_policy ON runs (policy_id);
CREATE INDEX runs_source ON runs (source_id);
CREATE INDEX runs_backup ON runs (backup_id);

-- Bounded per-run event log. The 1000-row cap is enforced transactionally by
-- the store; the final row of a full log is the truncated marker. Messages are
-- fixed-format and redacted: no secrets, DSNs, raw FTP text or paths beyond
-- the configured ones.
CREATE TABLE run_logs (
    run_id TEXT NOT NULL REFERENCES runs(id),
    seq INTEGER NOT NULL CHECK (seq >= 1),
    ts TEXT NOT NULL,
    level TEXT NOT NULL CHECK (level IN ('debug', 'info', 'warn', 'error')),
    event TEXT NOT NULL,
    message TEXT NOT NULL,
    PRIMARY KEY (run_id, seq)
);

-- +goose Down
DROP TABLE run_logs;
DROP TABLE runs;
ALTER TABLE policies DROP COLUMN next_incremental_at;
ALTER TABLE policies DROP COLUMN next_full_at;
