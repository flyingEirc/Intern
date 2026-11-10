package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/gofrs/flock"
	_ "modernc.org/sqlite"
)

// ImportResult is the non-secret evidence returned by the controlled upgrade.
// Counts refer to rows copied in the committed PostgreSQL transaction.
type ImportResult struct {
	InstanceID string           `json:"instance_id"`
	Rows       map[string]int64 `json:"rows"`
}

// legacyTables is also the dependency order. It is deliberately fixed: an
// unknown source table is never turned into executable SQL or imported by name.
var legacyTables = []string{
	"instance", "users", "sessions", "session_tombstones", "audit_logs",
	"repositories", "sources", "policies", "runs", "run_logs", "restores", "restore_logs",
	"chains", "backups", "cleanup_ledger", "replicas", "replica_members", "replica_parts",
}

var (
	importHookMu sync.Mutex
	// importAfterRow, when set, runs after each copied row inside the import
	// transaction. Tests hold that transaction open; production leaves it nil.
	importAfterRow            func(context.Context, *sql.Tx, string, int64) error
	errDestinationHasData     = errors.New("PostgreSQL destination already contains business data; refusing to overwrite")
	errDestinationInitialized = errors.New("PostgreSQL destination was already initialized; refusing to overwrite")
)

func signalImportRow(ctx context.Context, tx *sql.Tx, table string, count int64) error {
	importHookMu.Lock()
	hook := importAfterRow
	importHookMu.Unlock()
	if hook == nil {
		return nil
	}
	return hook(ctx, tx, table, count)
}

// ImportSQLite performs a one-time, offline upgrade into an empty PostgreSQL
// control plane. It leaves the SQLite source untouched and holds the legacy
// process lock until the single copy transaction commits or rolls back.
// An occupied or already initialized target is refused on a read-only session
// before Open migrates or inserts an instance row.
func ImportSQLite(ctx context.Context, dsn, sourcePath string) (_ ImportResult, retErr error) {
	source, err := openLegacySource(ctx, sourcePath)
	if err != nil {
		return ImportResult{}, err
	}
	defer func() { retErr = errors.Join(retErr, source.Close()) }()
	if err = validateLegacySQLite(ctx, source.tx); err != nil {
		return ImportResult{}, err
	}
	if err = rejectOccupiedDestination(ctx, dsn); err != nil {
		return ImportResult{}, err
	}
	destination, err := Open(ctx, dsn)
	if err != nil {
		return ImportResult{}, err
	}
	defer func() { retErr = errors.Join(retErr, destination.Close()) }()
	result := ImportResult{Rows: make(map[string]int64, len(legacyTables))}
	if err = destination.importTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		return copyLegacyPlane(ctx, source.tx, tx, &result)
	}); err != nil {
		return ImportResult{}, err
	}
	return result, nil
}

// legacySource holds the read-only legacy database, its snapshot transaction
// and the process lock that excludes the retired SQLite owner.
type legacySource struct {
	lock *flock.Flock
	db   *sql.DB
	tx   *sql.Tx
}

func openLegacySource(ctx context.Context, path string) (_ *legacySource, retErr error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("sqlite_file must be an absolute path")
	}
	if err := checkFile(path); err != nil {
		return nil, err
	}
	lock, err := lockDirectory(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	db, err := openLegacySQLite(ctx, path)
	if err != nil {
		return nil, errors.Join(err, lock.Close())
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, errors.Join(err, db.Close(), lock.Close())
	}
	return &legacySource{lock: lock, db: db, tx: tx}, nil
}

func (s *legacySource) Close() error {
	rollbackErr := s.tx.Rollback()
	if errors.Is(rollbackErr, sql.ErrTxDone) {
		rollbackErr = nil
	}
	return errors.Join(rollbackErr, s.db.Close(), s.lock.Close())
}

func openLegacySQLite(ctx context.Context, path string) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("mode", "ro")
	q.Add("_pragma", "query_only(1)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "busy_timeout(5000)")
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

func validateLegacySQLite(ctx context.Context, tx *sql.Tx) (retErr error) {
	for _, expected := range []struct {
		table   string
		version int64
	}{{"web_goose_db_version", 9}, {"physical_goose_db_version", 1}, {"replication_goose_db_version", 2}} {
		var version int64
		query := "SELECT COALESCE(MAX(version_id), 0) FROM " + expected.table + " WHERE is_applied = 1"
		if err := tx.QueryRowContext(ctx, query).Scan(&version); err != nil {
			return fmt.Errorf("read legacy SQLite schema version: %w", err)
		}
		if version != expected.version {
			return fmt.Errorf("unsupported legacy SQLite %s version %d; expected %d", expected.table, version, expected.version)
		}
	}
	var integrity string
	if err := tx.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&integrity); err != nil {
		return err
	}
	if integrity != "ok" {
		return errors.New("legacy SQLite integrity check failed")
	}
	rows, err := tx.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	if rows.Next() {
		return errors.New("legacy SQLite has foreign-key violations")
	}
	return rows.Err()
}

func copyLegacyPlane(ctx context.Context, source, destination *sql.Tx, result *ImportResult) error {
	if err := requireEmptyTarget(ctx, destination); err != nil {
		return err
	}
	// Open creates one fresh instance identity. Replace only that empty
	// deployment identity, inside the copy transaction, with the legacy one.
	if _, err := destination.ExecContext(ctx, "DELETE FROM instance"); err != nil {
		return err
	}
	for _, table := range legacyTables {
		count, err := copyLegacyTable(ctx, source, destination, table)
		if err != nil {
			return fmt.Errorf("import SQLite table %s: %w", table, err)
		}
		result.Rows[table] = count
	}
	if result.Rows["instance"] != 1 {
		return errors.New("legacy SQLite must contain exactly one instance identity")
	}
	if err := destination.QueryRowContext(ctx, "SELECT id FROM instance WHERE singleton = 1").Scan(&result.InstanceID); err != nil {
		return err
	}
	if _, err := destination.ExecContext(ctx, setupCompletedSQL); err != nil {
		return err
	}
	return restartImportedSequences(ctx, destination)
}

const setupCompletedSQL = `UPDATE setup_state SET completed =
EXISTS (SELECT 1 FROM users) OR EXISTS (
    SELECT 1 FROM audit_logs
    WHERE action IN ('auth.setup', 'users.create', 'users.create_admin') AND result = 'success'
) WHERE singleton = 1`

func restartImportedSequences(ctx context.Context, tx *sql.Tx) error {
	for _, table := range []string{"audit_logs", "cleanup_ledger", "replica_parts"} {
		if err := restartImportedSequence(ctx, tx, table); err != nil {
			return err
		}
	}
	return nil
}

// rejectOccupiedDestination reads the target before any migration. A fresh
// database has no business tables and is not a refusal.
func rejectOccupiedDestination(ctx context.Context, dsn string) (retErr error) {
	db, err := openReadOnly(ctx, dsn)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, db.Close()) }()
	return destinationOccupation(ctx, db)
}

func destinationOccupation(ctx context.Context, q rowQueryer) error {
	for _, table := range legacyTables {
		if table == "instance" {
			continue
		}
		occupied, err := legacyTableOccupied(ctx, q, table)
		if err != nil {
			return err
		}
		if occupied {
			return errDestinationHasData
		}
	}
	completed, err := setupAlreadyCompleted(ctx, q)
	if err != nil {
		return err
	}
	if completed {
		return errDestinationInitialized
	}
	return nil
}

func legacyTableOccupied(ctx context.Context, q rowQueryer, table string) (bool, error) {
	exists, err := tableExists(ctx, q, table)
	if err != nil || !exists {
		return false, err
	}
	var present bool
	err = q.QueryRowContext(ctx, legacyExistsSQL(table)).Scan(&present)
	return present, err
}

func setupAlreadyCompleted(ctx context.Context, q rowQueryer) (bool, error) {
	exists, err := tableExists(ctx, q, "setup_state")
	if err != nil || !exists {
		return false, err
	}
	var completed bool
	err = q.QueryRowContext(ctx, "SELECT completed FROM setup_state WHERE singleton = 1").Scan(&completed)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return completed, err
}

func requireEmptyTarget(ctx context.Context, tx *sql.Tx) error {
	for _, table := range legacyTables {
		if table == "instance" {
			continue
		}
		var present bool
		if err := tx.QueryRowContext(ctx, legacyExistsSQL(table)).Scan(&present); err != nil {
			return err
		}
		if present {
			return errDestinationHasData
		}
	}
	var completed bool
	if err := tx.QueryRowContext(ctx, "SELECT completed FROM setup_state WHERE singleton = 1 FOR UPDATE").Scan(&completed); err != nil {
		return err
	}
	if completed {
		return errDestinationInitialized
	}
	return nil
}

func copyLegacyTable(ctx context.Context, source, destination *sql.Tx, table string) (_ int64, retErr error) {
	pgColumns, err := destinationColumns(ctx, destination, table)
	if err != nil {
		return 0, err
	}
	rows, err := source.QueryContext(ctx, legacySelectSQL(table))
	if err != nil {
		return 0, err
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	columns, err := rows.Columns()
	if err != nil {
		return 0, err
	}
	// Column identifiers come from SQLite metadata, but are accepted only when
	// they match the destination's fixed schema exactly and in the same order.
	if strings.Join(columns, "\x00") != strings.Join(pgColumns, "\x00") {
		return 0, errors.New("legacy SQLite columns do not match PostgreSQL schema")
	}
	count, err := insertLegacyRows(ctx, rows, destination, table, legacyInsertSQL(table, columns))
	if err != nil {
		return 0, err
	}
	return count, verifyLegacyCount(ctx, destination, table, count)
}

func destinationColumns(ctx context.Context, destination *sql.Tx, table string) ([]string, error) {
	rows, err := destination.QueryContext(ctx, legacyShapeSQL(table))
	if err != nil {
		return nil, err
	}
	columns, err := rows.Columns()
	return columns, errors.Join(err, rows.Close())
}

// insertLegacyRows streams typed values into prepared PostgreSQL parameters;
// the statement never contains a legacy value.
func insertLegacyRows(ctx context.Context, rows *sql.Rows, destination *sql.Tx, table, statement string) (_ int64, retErr error) {
	columns, err := rows.Columns()
	if err != nil {
		return 0, err
	}
	prepared, err := destination.PrepareContext(ctx, statement)
	if err != nil {
		return 0, err
	}
	defer func() { retErr = errors.Join(retErr, prepared.Close()) }()
	values, scan := make([]any, len(columns)), make([]any, len(columns))
	for i := range values {
		scan[i] = &values[i]
	}
	var count int64
	for rows.Next() {
		if err = ctx.Err(); err != nil {
			return 0, err
		}
		if err = rows.Scan(scan...); err != nil {
			return 0, err
		}
		if _, err = prepared.ExecContext(ctx, values...); err != nil {
			return 0, err
		}
		count++
		if err = signalImportRow(ctx, destination, table, count); err != nil {
			return 0, err
		}
	}
	return count, rows.Err()
}

func verifyLegacyCount(ctx context.Context, destination *sql.Tx, table string, count int64) error {
	var stored int64
	if err := destination.QueryRowContext(ctx, legacyCountSQL(table)).Scan(&stored); err != nil {
		return err
	}
	if count != stored {
		return errors.New("copied row count does not match PostgreSQL readback")
	}
	return nil
}

func restartImportedSequence(ctx context.Context, tx *sql.Tx, table string) error {
	var next int64
	if err := tx.QueryRowContext(ctx, legacyMaxIDSQL(table)).Scan(&next); err != nil {
		return err
	}
	// ALTER SEQUENCE is transactional, unlike setval(), so a failed import
	// cannot publish a sequence change without its rows.
	_, err := tx.ExecContext(ctx, legacyRestartSequenceSQL(table, next))
	return err
}

// The table names below are constants from legacyTables, never caller input.

func legacySelectSQL(table string) string {
	order := ""
	if table == "backups" {
		order = " ORDER BY chain_id, seq"
	}
	return "SELECT * FROM " + table + order
}

func legacyShapeSQL(table string) string { return "SELECT * FROM " + table + " LIMIT 0" }

func legacyCountSQL(table string) string { return "SELECT COUNT(*) FROM " + table }

func legacyMaxIDSQL(table string) string { return "SELECT COALESCE(MAX(id), 0) + 1 FROM " + table }

func legacyRestartSequenceSQL(table string, next int64) string {
	return "ALTER SEQUENCE " + table + "_id_seq RESTART WITH " + strconv.FormatInt(next, 10)
}
func legacyExistsSQL(table string) string { return "SELECT EXISTS (SELECT 1 FROM " + table + ")" }

func legacyInsertSQL(table string, columns []string) string {
	quoted, placeholders := make([]string, len(columns)), make([]string, len(columns))
	for i, column := range columns {
		quoted[i] = `"` + strings.ReplaceAll(column, `"`, `""`) + `"`
		placeholders[i] = "$" + strconv.Itoa(i+1)
	}
	return "INSERT INTO " + table + " (" + strings.Join(quoted, ",") + ") VALUES (" + strings.Join(placeholders, ",") + ")"
}
