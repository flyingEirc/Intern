-- +goose Up
CREATE TABLE instance (
 singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
 id TEXT NOT NULL UNIQUE,
 created_at TEXT NOT NULL
);

-- +goose Down
DROP TABLE instance;
