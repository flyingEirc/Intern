-- +goose Up
CREATE TABLE jobs (
 sequence INTEGER PRIMARY KEY AUTOINCREMENT,
 id TEXT NOT NULL UNIQUE,
 source TEXT NOT NULL,
 kind TEXT NOT NULL,
 parent TEXT NOT NULL DEFAULT '',
 path TEXT NOT NULL,
 manifest BLOB NOT NULL DEFAULT X'',
 created_at TEXT NOT NULL,
 status TEXT NOT NULL CHECK(status IN ('running', 'completed', 'failed'))
);
CREATE INDEX jobs_source_completed ON jobs(source, status, sequence);

-- +goose Down
DROP TABLE jobs;
