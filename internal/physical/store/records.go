package store

import (
	"context"
	"database/sql"
	"strconv"
	"time"

	"github.com/flyingEirc/Intern/internal/physical/store/internal/storesql"
)

func (s *Store) Backup(ctx context.Context, id string) (Backup, error) {
	row, err := storesql.New(s.db).GetBackup(ctx, id)
	if err != nil {
		return Backup{}, err
	}
	return backupRecord(row)
}

func (s *Store) ChainBackups(ctx context.Context, chainID string) ([]Backup, error) {
	rows, err := storesql.New(s.db).ChainBackups(ctx, chainID)
	if err != nil {
		return nil, err
	}
	backups := make([]Backup, 0, len(rows))
	for _, row := range rows {
		backup, err := backupRecord(row)
		if err != nil {
			return nil, err
		}
		backups = append(backups, backup)
	}
	return backups, nil
}

func backupRecord(row storesql.Backup) (Backup, error) {
	backup := Backup{
		ID: row.ID, ChainID: row.ChainID, Seq: int(row.Seq), ParentID: row.ParentID.String,
		Kind: row.Kind, Status: row.Status, InstanceID: row.InstanceID, RunID: row.RunID,
		CheckpointsRaw: row.Checkpoints, InfoRaw: row.XtrabackupInfo, ObjectPath: row.ObjectPath,
		PayloadSize: row.PayloadSize.Int64, PayloadSHA256: row.PayloadSha256.String,
		MetadataSize: row.MetadataSize.Int64, MetadataSHA256: row.MetadataSha256.String,
		KeyID: row.KeyID, LocalPresent: row.LocalPresent != 0, FailureCode: row.FailureCode.String,
	}
	for _, field := range []struct {
		value sql.NullString
		dest  *uint64
	}{{row.FromLsn, &backup.FromLSN}, {row.ToLsn, &backup.ToLSN}, {row.LastLsn, &backup.LastLSN}} {
		if field.value.Valid {
			value, err := strconv.ParseUint(field.value.String, 10, 64)
			if err != nil {
				return Backup{}, err
			}
			*field.dest = value
		}
	}
	var err error
	backup.CreatedAt, err = time.Parse(time.RFC3339Nano, row.CreatedAt)
	if err != nil {
		return Backup{}, err
	}
	if row.CompletedAt.Valid {
		backup.CompletedAt, err = time.Parse(time.RFC3339Nano, row.CompletedAt.String)
	}
	return backup, err
}

// PendingCleanup returns only this instance's inactive, never-completed work.
// The single process owner must keep activeRuns stable while reclaiming it.
func (s *Store) PendingCleanup(ctx context.Context, instanceID string, activeRuns []string) ([]Cleanup, error) {
	rows, err := storesql.New(s.db).PendingCleanup(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	active := make(map[string]struct{}, len(activeRuns))
	for _, run := range activeRuns {
		active[run] = struct{}{}
	}
	pending := make([]Cleanup, 0, len(rows))
	for _, row := range rows {
		if _, exists := active[row.RunID]; !exists {
			pending = append(pending, Cleanup{ID: row.ID, BackupID: row.BackupID, Kind: row.Kind, Target: row.Target, RunID: row.RunID})
		}
	}
	return pending, nil
}

func (s *Store) CleanupDone(ctx context.Context, id int64) error {
	return storesql.New(s.db).CleanupDone(ctx, id)
}

func (s *Store) CleanupFailed(ctx context.Context, id int64, code string) error {
	return storesql.New(s.db).CleanupFailed(ctx, storesql.CleanupFailedParams{ID: id, LastErrorCode: text(code)})
}
