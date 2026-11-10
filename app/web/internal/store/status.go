package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/flyingEirc/Intern/app/web/internal/store/internal/storesql"
	physicalstore "github.com/flyingEirc/Intern/internal/physical/store"
	"github.com/flyingEirc/Intern/internal/replication"
)

type Status struct {
	CurrentVersion            int64  `json:"current_version"`
	LatestVersion             int64  `json:"latest_version"`
	PendingMigrations         int    `json:"pending_migrations"`
	InstanceID                string `json:"instance_id,omitempty"`
	PhysicalCurrentVersion    int64  `json:"physical_current_version"`
	PhysicalLatestVersion     int64  `json:"physical_latest_version"`
	PhysicalPendingMigrations int    `json:"physical_pending_migrations"`
	ReplicationCurrentVersion int64  `json:"replication_current_version"`
	ReplicationLatestVersion  int64  `json:"replication_latest_version"`
	ReplicationPending        int    `json:"replication_pending_migrations"`
}

// ReadStatus never runs migrations or takes the ownership lock. Its connection
// is read-only and independent of the serve owner.
func ReadStatus(ctx context.Context, dsn string) (_ Status, retErr error) {
	versions, err := embeddedVersions()
	if err != nil {
		return Status{}, err
	}
	status := Status{LatestVersion: latestVersion(versions), PendingMigrations: len(versions)}
	if err = readReplicaVersionPairs(ctx, nil, &status); err != nil {
		return Status{}, err
	}
	db, err := openReadOnly(ctx, dsn)
	if err != nil {
		return Status{}, err
	}
	defer func() { retErr = errors.Join(retErr, db.Close()) }()
	if err = readAppliedVersions(ctx, db, versions, &status); err != nil {
		return Status{}, err
	}
	return status, nil
}

// readAppliedVersions reads every migration set from an existing database.
func readAppliedVersions(ctx context.Context, db *sql.DB, versions []int64, status *Status) error {
	if err := readVersions(ctx, db, versions, status); err != nil {
		return err
	}
	if err := readReplicaVersionPairs(ctx, db, status); err != nil {
		return err
	}
	instance, err := readInstanceID(ctx, db)
	if err != nil {
		return err
	}
	status.InstanceID = instance
	return nil
}

// readReplicaVersionPairs reads the physical and replication sets, which report
// their versions without a DB (nil) or with the caller's database.
func readReplicaVersionPairs(ctx context.Context, db *sql.DB, status *Status) error {
	for _, read := range []func(context.Context, *sql.DB, *Status) error{readPhysicalVersions, readReplicationVersions} {
		if err := read(ctx, db, status); err != nil {
			return err
		}
	}
	return nil
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

// rowQueryer is the read surface shared by a pool and a transaction.
type rowQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// openReadOnly is a single read-only session. Startup rejects writes, and a
// failed dial is returned as a fixed reason without the connection identity.
func openReadOnly(ctx context.Context, dsn string) (*sql.DB, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("database_url is required")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("invalid PostgreSQL database configuration")
	}
	if config.RuntimeParams == nil {
		config.RuntimeParams = map[string]string{}
	}
	config.RuntimeParams["default_transaction_read_only"] = "on"
	db := sql.OpenDB(stdlib.GetConnector(*config))
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err = db.PingContext(ctx); err != nil {
		return nil, errors.Join(redactConnectError(err), db.Close())
	}
	return db, nil
}

func tableExists(ctx context.Context, q rowQueryer, name string) (bool, error) {
	var exists bool
	err := q.QueryRowContext(ctx, "SELECT to_regclass($1) IS NOT NULL", name).Scan(&exists)
	return exists, err
}

func readVersions(ctx context.Context, db *sql.DB, embedded []int64, status *Status) (retErr error) {
	exists, err := tableExists(ctx, db, versionTable)
	if err != nil || !exists {
		return err
	}
	rows, err := db.QueryContext(ctx, "SELECT version_id FROM web_goose_db_version WHERE is_applied")
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
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

// readReplicationVersions mirrors the physical pattern: the embedded latest is
// always known, and the applied version comes from the caller's database.
func readReplicationVersions(ctx context.Context, db *sql.DB, status *Status) error {
	current, latest, err := replication.Version(ctx, db)
	if err != nil {
		return err
	}
	status.ReplicationCurrentVersion = current
	status.ReplicationLatestVersion = latest
	// Replication migration numbers are contiguous, starting at one.
	status.ReplicationPending = int(max(0, latest-current))
	return nil
}
