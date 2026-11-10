package pgtest

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// OpenDSN opens an existing fixture database rather than allocating a new one.
// The caller retains the DSN across process boundaries and never logs it.
func OpenDSN(t testing.TB, dsn string) *sql.DB {
	t.Helper()
	if strings.TrimSpace(dsn) == "" {
		t.Fatal("fixture PostgreSQL database URL is required")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if err = db.PingContext(t.Context()); err != nil {
		t.Fatal(errors.Join(err, db.Close()))
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close fixture PostgreSQL connection: %v", err)
		}
	})
	return db
}

// Digest hashes every application table and row in one consistent snapshot.
// It replaces file checksums in read-only operation assertions; PostgreSQL
// data files are private to the server and are not a client-side test API.
func Digest(t testing.TB, db *sql.DB) string {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(t, tx)
	hash := sha256.New()
	for _, table := range tableNames(t, tx) {
		if _, err = fmt.Fprintf(hash, "%q\n", table); err != nil {
			t.Fatal(err)
		}
		hashTable(t, tx, table, hash)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func tableNames(t testing.TB, tx *sql.Tx) []string {
	t.Helper()
	rows, err := tx.QueryContext(t.Context(), "SELECT tablename FROM pg_tables WHERE schemaname = current_schema() ORDER BY tablename")
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var table string
		if err = rows.Scan(&table); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, table)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	return tables
}

func hashTable(t testing.TB, tx *sql.Tx, table string, hash io.Writer) {
	t.Helper()
	rows, err := tx.QueryContext(t.Context(), digestSelectSQL(table))
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var record string
		if err = rows.Scan(&record); err != nil {
			t.Fatal(err)
		}
		if _, err = fmt.Fprintf(hash, "%q\n", record); err != nil {
			t.Fatal(err)
		}
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
}

// CopyContents copies fixed, dependency-ordered tables into an already migrated
// empty PostgreSQL database. Both sides use real transactions and constraints.
func CopyContents(t testing.TB, source, destination *sql.DB, tables []string) {
	t.Helper()
	read, err := source.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(t, read)
	write, err := destination.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(t, write)
	for _, table := range tables {
		copyTable(t, read, write, table)
	}
	if err = write.Commit(); err != nil {
		t.Fatal(err)
	}
}

func copyTable(t testing.TB, source, destination *sql.Tx, table string) {
	t.Helper()
	rows, err := source.QueryContext(t.Context(), copySelectSQL(table))
	if err != nil {
		t.Fatal(err)
	}
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	values, scan := make([]any, len(columns)), make([]any, len(columns))
	for i := range values {
		scan[i] = &values[i]
	}
	statement := insertSQL(table, columns)
	for rows.Next() {
		if err = rows.Scan(scan...); err != nil {
			t.Fatal(err)
		}
		if _, err = destination.ExecContext(t.Context(), statement, values...); err != nil {
			t.Fatal(err)
		}
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
}

func rollback(t testing.TB, tx *sql.Tx) {
	t.Helper()
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		t.Errorf("rollback fixture transaction: %v", err)
	}
}

func digestSelectSQL(table string) string {
	return "SELECT row_to_json(record)::text FROM " + quoteIdentifier(table) + " record ORDER BY 1"
}

func copySelectSQL(table string) string {
	order := ""
	if table == "backups" {
		order = " ORDER BY chain_id, seq"
	}
	return "SELECT * FROM " + quoteIdentifier(table) + order
}

func insertSQL(table string, columns []string) string {
	quoted, parameters := make([]string, len(columns)), make([]string, len(columns))
	for i, column := range columns {
		quoted[i], parameters[i] = quoteIdentifier(column), fmt.Sprintf("$%d", i+1)
	}
	return "INSERT INTO " + quoteIdentifier(table) + " (" + strings.Join(quoted, ",") + ") VALUES (" + strings.Join(parameters, ",") + ")"
}

func quoteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}
