// Package catalog owns the single-process SQLite publication and cleanup ledger.
package catalog

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"

	"github.com/flyingEirc/Intern/internal/catalog/internal/catalogsql"
	"github.com/flyingEirc/Intern/internal/fsutil"
)

//go:embed migrations/*.sql
var migrations embed.FS
var ErrNotFound = errors.New("published backup not found")

type Record struct {
	ID, Source, Kind, Parent, Path string
	Manifest                       []byte
	CreatedAt                      time.Time
	Status                         string
}
type Store struct {
	db      *sql.DB
	queries *catalogsql.Queries
	lock    *flock.Flock
}

func Open(ctx context.Context, dir string) (*Store, error) {
	lock, err := lockDirectory(dir)
	if err != nil {
		return nil, err
	}
	db, err := openDatabase(ctx, filepath.Join(dir, "catalog.sqlite"))
	if err != nil {
		return nil, errors.Join(err, lock.Close())
	}
	return &Store{db: db, queries: catalogsql.New(db), lock: lock}, nil
}

func lockDirectory(dir string) (*flock.Flock, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() || st.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("catalog directory must be private (chmod 700)")
	}
	lock := flock.New(filepath.Join(dir, ".intern.lock"))
	ok, err := lock.TryLock()
	if err != nil {
		return nil, errors.Join(err, lock.Close())
	}
	if !ok {
		return nil, errors.Join(errors.New("state directory already owned by another Intern process"), lock.Close())
	}
	return lock, nil
}

func prepareDatabaseFile(path string) error {
	f, err := fsutil.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0o077 != 0 {
		return errors.New("catalog must be a private regular file")
	}
	return nil
}

func openDatabase(ctx context.Context, path string) (db *sql.DB, retErr error) {
	if err := prepareDatabaseFile(path); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: path}
	query := u.Query()
	query.Add("_pragma", "journal_mode(WAL)")
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "synchronous(FULL)")
	u.RawQuery = query.Encode()
	opened, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			fsutil.Close(&retErr, opened)
		}
	}()
	opened.SetMaxOpenConns(1)
	if err = migrate(ctx, opened); err != nil {
		return nil, err
	}
	return opened, nil
}

func migrate(ctx context.Context, db *sql.DB) error {
	migrationFS, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrationFS, goose.WithDisableGlobalRegistry(true))
	if err != nil {
		return err
	}
	_, err = provider.Up(ctx)
	return err
}
func (s *Store) Close() error { return errors.Join(s.db.Close(), s.lock.Close()) }
func (s *Store) Begin(ctx context.Context, r Record) (started bool, retErr error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() {
		rollbackErr := tx.Rollback()
		if !errors.Is(rollbackErr, sql.ErrTxDone) {
			retErr = errors.Join(retErr, rollbackErr)
		}
	}()
	started, err = beginJob(ctx, s.queries.WithTx(tx), r)
	if err != nil || !started {
		return false, err
	}
	return true, tx.Commit()
}

func beginJob(ctx context.Context, q *catalogsql.Queries, r Record) (bool, error) {
	existing, err := q.GetJob(ctx, r.ID)
	if errors.Is(err, sql.ErrNoRows) {
		err = q.InsertJob(ctx, catalogsql.InsertJobParams{ID: r.ID, Source: r.Source, Kind: r.Kind, Parent: r.Parent, Path: r.Path, CreatedAt: r.CreatedAt.UTC().Format(time.RFC3339Nano)})
		return true, err
	}
	if err != nil {
		return false, err
	}
	if existing.Source != r.Source || existing.Kind != r.Kind || existing.Path != r.Path {
		return false, errors.New("idempotency key belongs to a different request")
	}
	switch existing.Status {
	case "completed":
		return false, nil
	case "failed":
		return true, retryJob(ctx, q, r)
	default:
		return false, errors.New("request is already running or requires crash recovery")
	}
}

func retryJob(ctx context.Context, q *catalogsql.Queries, r Record) error {
	n, err := q.RetryJob(ctx, catalogsql.RetryJobParams{Parent: r.Parent, CreatedAt: r.CreatedAt.UTC().Format(time.RFC3339Nano), ID: r.ID})
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("cannot retry request")
	}
	return nil
}

func (s *Store) Complete(ctx context.Context, id string, manifest []byte) error {
	n, err := s.queries.CompleteJob(ctx, catalogsql.CompleteJobParams{ID: id, Manifest: manifest})
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("request completion rejected: not running")
	}
	return nil
}

func (s *Store) Fail(ctx context.Context, id string) error {
	_, err := s.queries.FailJob(ctx, id)
	return err
}

func convert(j catalogsql.Job, err error) (Record, error) {
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, err
	}
	created, err := time.Parse(time.RFC3339Nano, j.CreatedAt)
	if err != nil {
		return Record{}, fmt.Errorf("invalid catalog timestamp: %w", err)
	}
	return Record{ID: j.ID, Source: j.Source, Kind: j.Kind, Parent: j.Parent, Path: j.Path, Manifest: j.Manifest, CreatedAt: created, Status: j.Status}, nil
}

func (s *Store) Get(ctx context.Context, id string) (Record, error) {
	j, err := s.queries.GetCompleted(ctx, id)
	return convert(j, err)
}

func (s *Store) Latest(ctx context.Context, source string) (Record, error) {
	j, err := s.queries.LatestCompleted(ctx, source)
	return convert(j, err)
}

func convertAll(jobs []catalogsql.Job, err error) ([]Record, error) {
	if err != nil {
		return nil, err
	}
	records := make([]Record, 0, len(jobs))
	for _, j := range jobs {
		r, err := convert(j, nil)
		if err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	return records, nil
}

func (s *Store) List(ctx context.Context) ([]Record, error) {
	j, err := s.queries.ListCompleted(ctx)
	return convertAll(j, err)
}

func (s *Store) Pending(ctx context.Context) ([]Record, error) {
	j, err := s.queries.ListPending(ctx)
	return convertAll(j, err)
}
