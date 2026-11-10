-- +goose Up
-- Browser bootstrap is a one-way state transition. A newly migrated PostgreSQL
-- database starts uninitialised; the controlled SQLite import sets this flag
-- from existing users or historical administrator audit rows in its copy
-- transaction, so an upgraded deployment never asks for another first admin.
CREATE TABLE setup_state (
    singleton BIGINT PRIMARY KEY CHECK (singleton = 1),
    completed BOOLEAN NOT NULL
);
INSERT INTO setup_state (singleton, completed)
SELECT 1, EXISTS (SELECT 1 FROM users)
    OR EXISTS (SELECT 1 FROM audit_logs
               WHERE action IN ('auth.setup', 'users.create', 'users.create_admin')
                 AND result = 'success');

-- +goose Down
DROP TABLE setup_state;
