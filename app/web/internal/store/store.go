// Package store owns the Web control-plane PostgreSQL database.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pressly/goose/v3"

	"github.com/flyingEirc/Intern/app/web/internal/store/internal/storesql"
	physicalstore "github.com/flyingEirc/Intern/internal/physical/store"
	"github.com/flyingEirc/Intern/internal/replication"
)

// StatementTimeout bounds one statement wait on the owned connection. It also
// keeps the shutdown drain bound in server.Run finite when the database is slow.
const StatementTimeout = 5 * time.Second

type Store struct {
	db              *sql.DB
	provider        *goose.Provider
	instanceID      string
	latest          int64
	sessionLifetime time.Duration
	owner           *ownerGuard
}

// Open acquires database-level ownership, migrates, and preserves the instance
// identity. The database URL is never logged or returned in an error.
func Open(ctx context.Context, dsn string) (*Store, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("database_url is required")
	}
	return openStore(ctx, dsn)
}

func openStore(ctx context.Context, dsn string) (_ *Store, retErr error) {
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("invalid PostgreSQL database configuration")
	}
	configureSession(config)
	owner := newOwnerGuard()
	db := sql.OpenDB(owner.connector(*config))
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)
	if err = db.PingContext(ctx); err != nil {
		return nil, connectFailure(owner, db, err)
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, db.Close())
		}
	}()
	return bootstrapStore(ctx, db, owner)
}

func configureSession(config *pgx.ConnConfig) {
	config.ConnectTimeout = 10 * time.Second
	if config.RuntimeParams == nil {
		config.RuntimeParams = map[string]string{}
	}
	config.RuntimeParams["statement_timeout"] = strconv.FormatInt(StatementTimeout.Milliseconds(), 10)
	config.RuntimeParams["lock_timeout"] = strconv.FormatInt(StatementTimeout.Milliseconds(), 10)
}

func connectFailure(owner *ownerGuard, db *sql.DB, err error) error {
	closeErr := db.Close()
	if ownerErr := owner.errValue(); ownerErr != nil {
		return errors.Join(ownerErr, closeErr)
	}
	return errors.Join(fmt.Errorf("connect to PostgreSQL: %w", err), closeErr)
}

func bootstrapStore(ctx context.Context, db *sql.DB, owner *ownerGuard) (*Store, error) {
	fsys, err := migrationFS()
	if err != nil {
		return nil, err
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, fsys, providerOptions()...)
	if err != nil {
		return nil, err
	}
	latest := provider.ListSources()
	s := &Store{db: db, provider: provider, owner: owner, latest: latest[len(latest)-1].Version}
	if err = s.migrate(ctx); err != nil {
		return nil, err
	}
	if err = migrateSharedSchemas(ctx, db); err != nil {
		return nil, err
	}
	s.instanceID, err = ensureInstance(ctx, db)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func migrateSharedSchemas(ctx context.Context, db *sql.DB) error {
	if err := physicalstore.Migrate(ctx, db); err != nil {
		return err
	}
	// Replication owns its own goose table and refers to the physical backups
	// table, so it migrates after the physical schema.
	return replication.Migrate(ctx, db)
}

func (s *Store) migrate(ctx context.Context) error {
	current, err := s.provider.GetDBVersion(ctx)
	if err != nil {
		return fmt.Errorf("read migration version: %w", err)
	}
	if err = checkVersion(current, s.latest); err != nil {
		return err
	}
	if _, err = s.provider.Up(ctx); err != nil {
		return fmt.Errorf("migrate metadata database: %w", err)
	}
	return nil
}

// ensureInstance reads the singleton row and inserts it when absent. The single
// owned connection serializes the read and insert, and the owner lock excludes
// a second process.
func ensureInstance(ctx context.Context, db *sql.DB) (_ string, retErr error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() {
		if retErr != nil {
			rollbackErr := tx.Rollback()
			if !errors.Is(rollbackErr, sql.ErrTxDone) {
				retErr = errors.Join(retErr, rollbackErr)
			}
		}
	}()
	q := storesql.New(tx)
	instance, err := q.GetInstance(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		instance, err = insertInstance(ctx, q)
	}
	if err != nil {
		return "", err
	}
	return instance.ID, tx.Commit()
}

func insertInstance(ctx context.Context, q *storesql.Queries) (storesql.GetInstanceRow, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return storesql.GetInstanceRow{}, err
	}
	instance := storesql.GetInstanceRow{ID: id.String(), CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	err = q.InsertInstance(ctx, storesql.InsertInstanceParams(instance))
	return instance, err
}

func (s *Store) InstanceID() string { return s.instanceID }

// Lost is closed when the owned database session ends or the store closes.
func (s *Store) Lost() <-chan struct{} { return s.owner.lost }

// Err reports the permanent ownership error, if any.
func (s *Store) Err() error { return s.owner.errValue() }

// Watch detects a broken owned connection even when no request is in flight.
// A lost session permanently retires this process because the connector refuses
// a replacement. Query cancellation, including statement_timeout, leaves the
// owner in place.
func (s *Store) Watch(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.Lost():
			return
		case <-ticker.C:
		}
		pingCtx, cancel := context.WithTimeout(ctx, StatementTimeout)
		err := s.db.PingContext(pingCtx)
		cancel()
		if err == nil || !ownerSessionLost(err) {
			continue
		}
		s.owner.lose()
		return
	}
}

// ConnectionsInUse reports pooled connections currently inside a driver call.
func (s *Store) ConnectionsInUse() int { return s.db.Stats().InUse }

// Ready checks both connectivity and the exact migration version.
func (s *Store) Ready(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return err
	}
	if err := s.readyWeb(ctx); err != nil {
		return err
	}
	if err := readyPhysical(ctx, s.db); err != nil {
		return err
	}
	return readyReplication(ctx, s.db)
}

func (s *Store) readyWeb(ctx context.Context) error {
	current, err := s.provider.GetDBVersion(ctx)
	if err != nil {
		return err
	}
	if current != s.latest {
		return fmt.Errorf("metadata database version %d does not match embedded latest version %d", current, s.latest)
	}
	versions, err := embeddedVersions()
	if err != nil {
		return err
	}
	status := Status{}
	if err = readVersions(ctx, s.db, versions, &status); err != nil {
		return err
	}
	if status.PendingMigrations != 0 {
		return fmt.Errorf("metadata database has %d missing migrations", status.PendingMigrations)
	}
	return nil
}

func readyPhysical(ctx context.Context, db *sql.DB) error {
	current, latest, err := physicalstore.Version(ctx, db)
	if err != nil {
		return err
	}
	if current != latest {
		return fmt.Errorf("physical database version %d does not match embedded latest version %d", current, latest)
	}
	return nil
}

func readyReplication(ctx context.Context, db *sql.DB) error {
	current, latest, err := replication.Version(ctx, db)
	if err != nil {
		return err
	}
	if current != latest {
		return fmt.Errorf("replication database version %d does not match embedded latest version %d", current, latest)
	}
	return nil
}

// txWindow bounds one detached write transaction. Caller cancellation cannot
// abort a transaction that has already started, so a statement can never leave
// a half-open transaction on the single owned connection.
const txWindow = 30 * time.Second

// writeTx runs fn inside one transaction that caller cancellation cannot
// poison. A non-nil error rolls the mutation and any success audit inserted by
// fn back together.
func (s *Store) writeTx(ctx context.Context, fn func(ctx context.Context, tx *sql.Tx) error) (retErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.owner.errValue(); err != nil {
		return err
	}
	txCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), txWindow)
	defer cancel()
	tx, err := s.db.BeginTx(txCtx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
				retErr = errors.Join(retErr, rollbackErr)
			}
		}
	}()
	if err = fn(txCtx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

// importTx copies one legacy database in a single transaction. It follows ctx,
// so canceling the command rolls the copy back, and it is not bounded by the
// interactive statement or transaction windows. Rollback uses the same session,
// which keeps the owner lock until the store closes.
func (s *Store) importTx(ctx context.Context, fn func(context.Context, *sql.Tx) error) (retErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.owner.errValue(); err != nil {
		return err
	}
	txCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	tx, err := s.db.BeginTx(txCtx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			retErr = rollbackImport(tx, retErr)
		}
	}()
	if err = relaxImportLimits(txCtx, tx); err != nil {
		return err
	}
	if err = fn(withImport(ctx), tx); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return tx.Commit()
}

func relaxImportLimits(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, "SELECT set_config('statement_timeout', '0', true), set_config('lock_timeout', '0', true)")
	return err
}

func rollbackImport(tx *sql.Tx, err error) error {
	if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
		return errors.Join(err, rollbackErr)
	}
	return err
}

// withTx runs fn inside one write transaction. fn receives the transaction's
// own context and must run every statement with it.
func (s *Store) withTx(ctx context.Context, fn func(ctx context.Context, q *storesql.Queries) error) error {
	return s.writeTx(ctx, func(ctx context.Context, tx *sql.Tx) error { return fn(ctx, storesql.New(tx)) })
}

// withTxDB is withTx for a caller that must also run one statement the Web
// schema does not own, such as an existence count over a table migrated by the
// physical or replication package. Anything else belongs in sqlc.
func (s *Store) withTxDB(ctx context.Context, fn func(ctx context.Context, q *storesql.Queries, tx *sql.Tx) error) error {
	return s.writeTx(ctx, func(ctx context.Context, tx *sql.Tx) error { return fn(ctx, storesql.New(tx), tx) })
}

// Close closes the provider's database handle. Closing the single physical
// connection releases the session advisory lock; there is no earlier explicit
// unlock that could let another process take over while workers still run.
func (s *Store) Close() error {
	s.owner.stop()
	if s.provider != nil {
		return s.provider.Close()
	}
	return s.db.Close()
}
