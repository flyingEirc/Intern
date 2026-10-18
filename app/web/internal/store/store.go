// Package store owns the single-process Web control-plane database.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"

	"github.com/flyingEirc/Intern/app/web/internal/store/internal/storesql"
	"github.com/flyingEirc/Intern/internal/fsutil"
)

// BusyTimeout is the SQLite busy_timeout on every control-plane connection.
// modernc.org/sqlite's default busy callback does not observe sqlite3_interrupt,
// so a statement already waiting on a lock keeps running until this elapses
// even after its context is canceled. Shutdown waits at least this long, in
// addition to the detached-audit budget, before it may close the database.
const BusyTimeout = 5 * time.Second

type Store struct {
	db              *sql.DB
	provider        *goose.Provider
	lock            *flock.Flock
	instanceID      string
	latest          int64
	sessionLifetime time.Duration
}

// Open acquires exclusive ownership, migrates, and preserves the instance identity.
func Open(ctx context.Context, dir string) (*Store, error) {
	lock, err := lockDirectory(dir)
	if err != nil {
		return nil, err
	}
	s, err := openStore(ctx, dir)
	if err != nil {
		return nil, errors.Join(err, lock.Close())
	}
	s.lock = lock
	return s, nil
}

func openStore(ctx context.Context, dir string) (_ *Store, retErr error) {
	path := filepath.Join(dir, "intern.db")
	if err := prepareDatabaseFile(path); err != nil {
		return nil, err
	}
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
	latest := provider.ListSources()
	s := &Store{db: db, provider: provider, latest: latest[len(latest)-1].Version}
	if err = s.migrate(ctx); err != nil {
		return nil, err
	}
	s.instanceID, err = ensureInstance(ctx, db)
	return s, err
}

func openDatabase(ctx context.Context, path string, readOnly bool) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", BusyTimeout.Milliseconds()))
	q.Add("_pragma", "synchronous(FULL)")
	q.Add("_pragma", "foreign_keys(1)")
	if readOnly {
		q.Set("mode", "ro")
		q.Add("_pragma", "query_only(1)")
	} else {
		q.Add("_pragma", "journal_mode(WAL)")
		// modernc.org/sqlite applies _txlock only when the transaction is not
		// read-only (newTx uses BEGIN IMMEDIATE for !opts.ReadOnly). The write
		// lock is reserved before any SELECT, so busy_timeout waits. That wait
		// does not return early when the context is canceled. A deferred
		// transaction that has already read does not invoke the busy handler and
		// returns SQLITE_BUSY or SQLITE_BUSY_SNAPSHOT instead.
		q.Set("_txlock", "immediate")
	}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err = db.PingContext(ctx); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return db, nil
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

// ensureInstance reads the singleton row and inserts it when absent. The writable
// pool starts this transaction with BEGIN IMMEDIATE, so that read cannot upgrade
// into SQLITE_BUSY_SNAPSHOT.
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

// ConnectionsInUse reports pooled connections currently inside a driver call.
func (s *Store) ConnectionsInUse() int { return s.db.Stats().InUse }

// Ready checks both connectivity and the exact migration version.
func (s *Store) Ready(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return err
	}
	current, err := s.provider.GetDBVersion(ctx)
	if err != nil {
		return err
	}
	if current != s.latest {
		return fmt.Errorf("metadata database version %d does not match embedded latest version %d", current, s.latest)
	}
	return nil
}

// withTx runs fn inside one BEGIN IMMEDIATE transaction. A non-nil error rolls
// the mutation and any success audit inserted by fn back together.
func (s *Store) withTx(ctx context.Context, fn func(*storesql.Queries) error) (retErr error) {
	tx, err := s.db.BeginTx(ctx, nil)
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
	if err = fn(storesql.New(tx)); err != nil {
		return err
	}
	return tx.Commit()
}

// Close closes the database before releasing exclusive ownership.
func (s *Store) Close() error {
	if s.lock == nil {
		return s.db.Close()
	}
	return errors.Join(s.db.Close(), s.lock.Close())
}
