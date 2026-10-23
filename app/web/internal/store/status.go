package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"

	"github.com/flyingEirc/Intern/app/web/internal/store/internal/storesql"
	"github.com/flyingEirc/Intern/internal/fsutil"
	physicalstore "github.com/flyingEirc/Intern/internal/physical/store"
)

type Status struct {
	CurrentVersion            int64  `json:"current_version"`
	LatestVersion             int64  `json:"latest_version"`
	PendingMigrations         int    `json:"pending_migrations"`
	InstanceID                string `json:"instance_id,omitempty"`
	PhysicalCurrentVersion    int64  `json:"physical_current_version"`
	PhysicalLatestVersion     int64  `json:"physical_latest_version"`
	PhysicalPendingMigrations int    `json:"physical_pending_migrations"`
}

// ReadStatus never creates a directory, locks ownership, or runs migrations.
func ReadStatus(ctx context.Context, dir string) (_ Status, retErr error) {
	versions, err := embeddedVersions()
	if err != nil {
		return Status{}, err
	}
	status := Status{LatestVersion: latestVersion(versions), PendingMigrations: len(versions)}
	if err = readPhysicalVersions(ctx, nil, &status); err != nil {
		return Status{}, err
	}
	if err = checkDirectory(dir); errors.Is(err, os.ErrNotExist) {
		return status, nil
	}
	if err != nil {
		return Status{}, err
	}
	path := filepath.Join(dir, "intern.db")
	if err = checkFile(path); errors.Is(err, os.ErrNotExist) {
		return status, nil
	}
	if err != nil {
		return Status{}, err
	}
	db, err := openDatabase(ctx, path, true)
	if err != nil {
		return Status{}, err
	}
	defer fsutil.Close(&retErr, db)
	if err = readVersions(ctx, db, versions, &status); err != nil {
		return Status{}, err
	}
	if err = readPhysicalVersions(ctx, db, &status); err != nil {
		return Status{}, err
	}
	status.InstanceID, err = readInstanceID(ctx, db)
	return status, err
}

func readInstanceID(ctx context.Context, db *sql.DB) (string, error) {
	exists, err := tableExists(ctx, db, "instance")
	if err != nil || !exists {
		return "", err
	}
	instance, err := storesql.New(db).GetInstance(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return instance.ID, err
}

func tableExists(ctx context.Context, db *sql.DB, name string) (bool, error) {
	var exists bool
	err := db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM sqlite_schema WHERE type = 'table' AND name = ?)", name).Scan(&exists)
	return exists, err
}

func readVersions(ctx context.Context, db *sql.DB, embedded []int64, status *Status) (retErr error) {
	exists, err := tableExists(ctx, db, versionTable)
	if err != nil || !exists {
		return err
	}
	rows, err := db.QueryContext(ctx, "SELECT version_id FROM web_goose_db_version WHERE is_applied = 1")
	if err != nil {
		return err
	}
	defer fsutil.Close(&retErr, rows)
	applied := make(map[int64]bool)
	for rows.Next() {
		var version int64
		if err = rows.Scan(&version); err != nil {
			return err
		}
		status.CurrentVersion = max(status.CurrentVersion, version)
		applied[version] = true
	}
	status.PendingMigrations = 0
	for _, version := range embedded {
		if !applied[version] {
			status.PendingMigrations++
		}
	}
	return rows.Err()
}

func readPhysicalVersions(ctx context.Context, db *sql.DB, status *Status) error {
	current, latest, err := physicalstore.Version(ctx, db)
	if err != nil {
		return err
	}
	status.PhysicalCurrentVersion = current
	status.PhysicalLatestVersion = latest
	// Physical migration numbers are contiguous, starting at one.
	status.PhysicalPendingMigrations = int(max(0, latest-current))
	return nil
}
