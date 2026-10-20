package physical

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
)

func checkSourceDatafiles(ctx context.Context, db *sql.DB, directory string) (retErr error) {
	rows, err := db.QueryContext(ctx, `SELECT PATH FROM information_schema.innodb_datafiles`)
	if err != nil {
		return preflightExternalFailure(ctx, "volume", "datafiles_query_failed", "Cannot enumerate InnoDB datafile paths; check PROCESS privilege.")
	}
	defer func() {
		if err := rows.Close(); err != nil {
			retErr = errors.Join(retErr, preflightFailure("volume", "datafiles_query_failed", "Cannot finish inspecting InnoDB datafile paths."))
		}
	}()
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return preflightFailure("volume", "datafiles_query_failed", "Cannot decode an InnoDB datafile path.")
		}
		if err := checkSourceDatafile(directory, path); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return preflightExternalFailure(ctx, "volume", "datafiles_query_failed", "Cannot finish enumerating InnoDB datafile paths.")
	}
	return nil
}

func checkSourceDatafile(directory, path string) (retErr error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(directory, path)
	}
	if !pathWithin(directory, path) {
		return preflightFailure("volume", "external_tablespace_unsupported", "InnoDB datafiles outside the mounted datadir are not admitted; provide a self-contained data volume.")
	}
	relative, err := filepath.Rel(directory, path)
	if err != nil {
		return preflightFailure("volume", "external_tablespace_unsupported", "Cannot resolve the InnoDB datafile within the mounted datadir.")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return preflightFailure("volume", "datadir_unreadable", "Cannot open the mounted data directory.")
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			retErr = errors.Join(retErr, preflightFailure("volume", "datadir_unreadable", "Cannot close the mounted data directory."))
		}
	}()
	info, err := root.Stat(relative)
	if err != nil || !info.Mode().IsRegular() {
		return preflightFailure("volume", "datadir_unreadable", "A server-reported InnoDB datafile is missing or not a readable regular file in the mounted volume.")
	}
	file, err := root.Open(relative)
	if err != nil {
		return preflightFailure("volume", "datadir_unreadable", "Cannot open a server-reported InnoDB datafile with the runtime UID/GID.")
	}
	if err := file.Close(); err != nil {
		return preflightFailure("volume", "datadir_unreadable", "Cannot finish inspecting a server-reported InnoDB datafile.")
	}
	return nil
}
