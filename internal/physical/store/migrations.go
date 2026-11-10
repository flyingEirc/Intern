package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"strconv"
	"strings"

	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Version is read-only, including when the version table does not yet exist.
// A nil database returns the embedded latest version without opening a database.
func Version(ctx context.Context, db *sql.DB) (current, latest int64, err error) {
	entries, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return 0, 0, err
	}
	for _, entry := range entries {
		prefix, _, _ := strings.Cut(strings.TrimPrefix(entry, "migrations/"), "_")
		version, parseErr := strconv.ParseInt(prefix, 10, 64)
		if parseErr != nil {
			return 0, 0, parseErr
		}
		latest = max(latest, version)
	}
	if db == nil {
		return 0, latest, nil
	}
	var exists bool
	if err = db.QueryRowContext(ctx, "SELECT to_regclass('physical_goose_db_version') IS NOT NULL").Scan(&exists); err != nil || !exists {
		return 0, latest, err
	}
	err = db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version_id), 0) FROM physical_goose_db_version v
WHERE is_applied AND id = (SELECT MAX(id) FROM physical_goose_db_version WHERE version_id = v.version_id)`).Scan(&current)
	return current, latest, err
}

// Migrate runs only physical migrations on the caller's control-plane database.
// The caller owns the database and its process-wide migration lock.
func Migrate(ctx context.Context, db *sql.DB) error {
	current, latest, err := Version(ctx, db)
	if err != nil {
		return err
	}
	if current > latest {
		return fmt.Errorf("physical database version %d is newer than embedded latest version %d", current, latest)
	}
	fsys, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, fsys,
		goose.WithDisableGlobalRegistry(true), goose.WithTableName("physical_goose_db_version"))
	if err != nil {
		return err
	}
	_, err = provider.Up(ctx)
	return err
}
