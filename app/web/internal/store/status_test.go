package store

import (
	"database/sql"
	"testing"

	"github.com/flyingEirc/Intern/internal/pgtest"
)

func TestStatusDatabaseIsReadOnly(t *testing.T) {
	s, dsn := openTestStoreDSN(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("pgx", dsn+"&default_transaction_read_only=on")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.ExecContext(t.Context(), "DELETE FROM instance"); err == nil {
		t.Fatal("read-only database accepted a write")
	}
}

func TestStatusDoesNotMigrate(t *testing.T) {
	dsn := pgtest.New(t)
	status, err := ReadStatus(t.Context(), dsn)
	if err != nil || status.CurrentVersion != 0 || status.PendingMigrations == 0 {
		t.Fatalf("unmigrated status: %+v, %v", status, err)
	}
}

func webLatest(t *testing.T) int64 {
	t.Helper()
	latest, err := LatestVersion()
	if err != nil {
		t.Fatal(err)
	}
	return latest
}
