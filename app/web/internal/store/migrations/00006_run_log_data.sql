-- +goose Up
-- Structured run log payloads for dispatcher lifecycle events (W4).
-- The message stays a fixed redacted string; data holds the typed object.
ALTER TABLE run_logs ADD COLUMN data TEXT NULL;

-- +goose Down
ALTER TABLE run_logs DROP COLUMN data;
