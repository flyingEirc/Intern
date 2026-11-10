package cmd

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/flyingEirc/Intern/app/web/internal/store"
)

func TestCommandConnectFailuresHideIdentity(t *testing.T) {
	cleanConfigEnv(t)
	user := "userInternSecret"
	password := "pwInternSecret99"
	database := "dbInternSecret"
	host := "127.0.0.1"
	port := refusedTCPPort(t)
	dsn := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable&connect_timeout=2", user, password, host, port, database)
	t.Setenv("INTERN_DATABASE_URL", dsn)
	dir := t.TempDir()
	sqliteFile := legacyVersionFile(t)
	secrets := []string{"user=", "database=", "password=", user, password, database, host, dsn}
	commands := []struct {
		name string
		args []string
	}{
		{name: "migrate_status", args: []string{"migrate", "status", "--metadata-dir", dir}},
		{name: "migrate_up", args: []string{"migrate", "up", "--metadata-dir", dir}},
		{name: "serve", args: []string{"serve", "--metadata-dir", dir}},
		{name: "import_sqlite", args: []string{"migrate", "import-sqlite", "--metadata-dir", dir, "--sqlite-file", sqliteFile}},
	}
	for _, command := range commands {
		t.Run(command.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			root := New(&stdout, &stderr)
			root.SetArgs(command.args)
			err := root.ExecuteContext(t.Context())
			requireCommandHidesIdentity(t, err, stdout.String(), stderr.String(), secrets)
		})
	}
}

func requireCommandHidesIdentity(t *testing.T, err error, stdout, stderr string, secrets []string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected connection failure")
	}
	if !errors.Is(err, store.ErrPostgresUnreachable) {
		t.Fatalf("unreachable class: %v", err)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("dial class: %v", err)
	}
	text := stdout + "\n" + stderr + "\n" + err.Error()
	for _, secret := range secrets {
		if strings.Contains(text, secret) {
			t.Fatalf("connection identity %q leaked: %s", secret, text)
		}
	}
}

func refusedTCPPort(t *testing.T) int {
	t.Helper()
	for range 5 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr, ok := ln.Addr().(*net.TCPAddr)
		if !ok {
			t.Fatal(ln.Addr())
		}
		if err = ln.Close(); err != nil {
			t.Fatal(err)
		}
		conn, dialErr := net.DialTimeout("tcp", addr.String(), time.Second)
		if dialErr != nil {
			return addr.Port
		}
		if err = conn.Close(); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("no refused TCP port")
	return 0
}

func legacyVersionFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "intern.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`CREATE TABLE web_goose_db_version (version_id INTEGER, is_applied INTEGER)`,
		`INSERT INTO web_goose_db_version VALUES (9, 1)`,
		`CREATE TABLE physical_goose_db_version (version_id INTEGER, is_applied INTEGER)`,
		`INSERT INTO physical_goose_db_version VALUES (1, 1)`,
		`CREATE TABLE replication_goose_db_version (version_id INTEGER, is_applied INTEGER)`,
		`INSERT INTO replication_goose_db_version VALUES (2, 1)`,
	}
	for _, statement := range statements {
		if _, err = db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
