package store

import (
	"embed"
	"fmt"
	"io/fs"
	"strconv"
	"strings"

	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrations embed.FS

const versionTable = "web_goose_db_version"

func migrationFS() (fs.FS, error) {
	return fs.Sub(migrations, "migrations")
}

func embeddedVersions() ([]int64, error) {
	entries, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	versions := make([]int64, 0, len(entries))
	for _, entry := range entries {
		name := strings.TrimPrefix(entry, "migrations/")
		prefix, _, _ := strings.Cut(name, "_")
		version, parseErr := strconv.ParseInt(prefix, 10, 64)
		if parseErr != nil {
			return nil, fmt.Errorf("invalid embedded migration %q: %w", entry, parseErr)
		}
		versions = append(versions, version)
	}
	return versions, nil
}

func latestVersion(versions []int64) int64 {
	var latest int64
	for _, version := range versions {
		latest = max(latest, version)
	}
	return latest
}

// LatestVersion is the newest embedded web migration version. Callers that
// reason about "fully migrated" (the CLI status output, readiness checks and
// their tests) take it from here instead of pinning a number that every new
// migration would invalidate.
func LatestVersion() (int64, error) {
	versions, err := embeddedVersions()
	if err != nil {
		return 0, err
	}
	return latestVersion(versions), nil
}

func checkVersion(current, latest int64) error {
	if current > latest {
		return fmt.Errorf("metadata database version %d is newer than embedded latest version %d", current, latest)
	}
	return nil
}

func providerOptions() []goose.ProviderOption {
	return []goose.ProviderOption{
		goose.WithDisableGlobalRegistry(true),
		goose.WithTableName(versionTable),
	}
}
