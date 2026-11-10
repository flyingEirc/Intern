-- +goose Up
-- A replica row now names the local repository its backup is stored in, so one
-- FTP repository shared by two local repositories gets two queues that never
-- read each other's rows. The physical schema keeps no local repository on a
-- backup, so the queue row is the only durable place for this binding: the
-- caller that enqueues knows the policy's local repository.
--
-- A row written before this migration has no local repository, so it is left
-- with an empty value: it matches no replicator and is therefore inert. The
-- insert is an upsert that binds such a row instead of adding a second one, so
-- the dispatcher's catch-up claims it, and its uploaded state, attempt history
-- and temporary-key cleanup obligations are all kept.
ALTER TABLE replicas ADD COLUMN local_repository_id TEXT NOT NULL DEFAULT '';
CREATE INDEX replicas_local_repository
    ON replicas (local_repository_id, repository_id, status, next_attempt_at);

-- +goose Down
DROP INDEX replicas_local_repository;
ALTER TABLE replicas DROP COLUMN local_repository_id;
