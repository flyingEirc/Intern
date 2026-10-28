package physical

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
)

func checkSourceDatafiles(ctx context.Context, db *sql.DB, directory string) (retErr error) {
	if err := checkSourceStoragePaths(ctx, db, directory); err != nil {
		return err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return preflightFailure("datafiles", "datadir_unreadable", "Cannot open the mounted data directory.")
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			retErr = errors.Join(retErr, preflightFailure("datafiles", "datadir_unreadable", "Cannot close the mounted data directory."))
		}
	}()
	rows, err := db.QueryContext(ctx, `SELECT PATH FROM information_schema.innodb_datafiles`)
	if err != nil {
		return preflightExternalFailure(ctx, "datafiles", "datafiles_query_failed", "Cannot enumerate InnoDB datafile paths; check PROCESS privilege.")
	}
	defer func() {
		if err := rows.Close(); err != nil {
			retErr = errors.Join(retErr, preflightFailure("datafiles", "datafiles_query_failed", "Cannot finish inspecting InnoDB datafile paths."))
		}
	}()
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return preflightFailure("datafiles", "datafiles_query_failed", "Cannot decode an InnoDB datafile path.")
		}
		if err := checkSourceDatafile(root, directory, path); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return preflightExternalFailure(ctx, "datafiles", "datafiles_query_failed", "Cannot finish enumerating InnoDB datafile paths.")
	}
	return nil
}

func checkSourceDatafile(root *os.Root, directory, path string) error {
	if !filepath.IsAbs(path) {
		path = filepath.Join(directory, path)
	}
	if !pathWithin(directory, path) {
		return preflightFailure("datafiles", "external_tablespace_unsupported", "InnoDB datafiles outside the mounted datadir are not admitted; provide a self-contained data volume.")
	}
	relative, err := filepath.Rel(directory, path)
	if err != nil {
		return preflightFailure("datafiles", "external_tablespace_unsupported", "Cannot resolve the InnoDB datafile within the mounted datadir.")
	}
	info, err := root.Stat(relative)
	if err != nil || !info.Mode().IsRegular() {
		return preflightFailure("datafiles", "datadir_unreadable", "A server-reported InnoDB datafile is missing or not a readable regular file in the mounted volume.")
	}
	file, err := root.Open(relative)
	if err != nil {
		return preflightFailure("datafiles", "datadir_unreadable", "Cannot open a server-reported InnoDB datafile with the runtime UID/GID.")
	}
	if err := file.Close(); err != nil {
		return preflightFailure("datafiles", "datadir_unreadable", "Cannot finish inspecting a server-reported InnoDB datafile.")
	}
	return nil
}

func checkSourceStoragePaths(ctx context.Context, db *sql.DB, directory string) error {
	var redo, doublewrite sql.NullString
	if err := db.QueryRowContext(ctx, "SELECT @@innodb_log_group_home_dir, @@innodb_doublewrite_dir").Scan(&redo, &doublewrite); err != nil {
		return preflightExternalFailure(ctx, "datafiles", "datafiles_query_failed", "Cannot inspect redo and doublewrite locations.")
	}
	// MySQL represents an unspecified doublewrite directory as NULL and uses
	// datadir internally. Both NULL and the empty value resolve to that root.
	for _, location := range []string{redo.String, doublewrite.String} {
		if !filepath.IsAbs(location) {
			location = filepath.Join(directory, location)
		}
		if !pathWithin(directory, location) {
			return preflightFailure("datafiles", "external_tablespace_unsupported", "Redo and doublewrite files must be inside the mounted data volume.")
		}
	}
	return nil
}
