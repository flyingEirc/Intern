package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/pressly/goose/v3"

	"github.com/flyingEirc/Intern/internal/fsutil"
)

// OpenCurrent opens an existing, fully migrated database without taking the serve
// lock or running migrations. It is exclusively for the administrative command.
func OpenCurrent(ctx context.Context, dir string) (_ *Store, retErr error) {
	status, err := currentStatus(ctx, dir)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "intern.db")
	db, err := openDatabase(ctx, path, false)
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			fsutil.Close(&retErr, db)
		}
	}()
	fsys, err := migrationFS()
	if err != nil {
		return nil, err
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, fsys, providerOptions()...)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, provider: provider, latest: status.LatestVersion, instanceID: status.InstanceID}
	if err = s.Ready(ctx); err != nil {
		return nil, errors.New("metadata migration changed; run serve or migrate up first")
	}
	return s, nil
}

func currentStatus(ctx context.Context, dir string) (Status, error) {
	status, err := ReadStatus(ctx, dir)
	if err != nil {
		return Status{}, err
	}
	if status.CurrentVersion != status.LatestVersion || status.PendingMigrations != 0 {
		return Status{}, fmt.Errorf("metadata database version %d does not match embedded latest version %d; run serve or migrate up first", status.CurrentVersion, status.LatestVersion)
	}
	if err = checkDirectory(dir); err != nil {
		return Status{}, err
	}
	return status, checkFile(filepath.Join(dir, "intern.db"))
}
