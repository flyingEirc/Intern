-- +goose Up
-- The (local, replica) repository pair a run was claimed on, snapshotted from
-- its policy by the claim itself. Replication resolves a backup's pair from
-- this snapshot, so a later PUT that moves the policy's repositories cannot
-- rebind an existing backup to a repository it was never written to: new runs
-- get the new pair, old backups keep theirs.
ALTER TABLE runs ADD COLUMN local_repository_id TEXT;
ALTER TABLE runs ADD COLUMN replica_repository_id TEXT;

-- Rows written before this migration carry no snapshot. Their producing policy
-- is the only surviving evidence of the pair they ran on, so backfill it once:
-- a completed run keeps the pair it is already replicated through today instead
-- of losing its binding at the upgrade.
UPDATE runs SET
    local_repository_id = (SELECT p.local_repository_id FROM policies p WHERE p.id = runs.policy_id),
    replica_repository_id = (SELECT p.replica_repository_id FROM policies p WHERE p.id = runs.policy_id)
WHERE backup_id IS NOT NULL;

CREATE INDEX runs_replication_pair ON runs (local_repository_id, replica_repository_id);

-- +goose Down
DROP INDEX runs_replication_pair;
ALTER TABLE runs DROP COLUMN replica_repository_id;
ALTER TABLE runs DROP COLUMN local_repository_id;
