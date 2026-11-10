package pgtest

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

// Open returns a single connection to an isolated PostgreSQL database and
// closes it during test cleanup. The caller may run its own migrations.
func Open(t testing.TB) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", New(t))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err = db.PingContext(ctx); err != nil {
		t.Fatal(errors.Join(err, db.Close()))
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close PostgreSQL test connection: %v", err)
		}
	})
	return db
}
