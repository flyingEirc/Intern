-- +goose Up
CREATE TABLE session_tombstones (
    token TEXT PRIMARY KEY NOT NULL,
    expiry DOUBLE PRECISION NOT NULL
);
CREATE INDEX session_tombstones_expiry_idx ON session_tombstones (expiry);

-- +goose Down
DROP TABLE session_tombstones;
