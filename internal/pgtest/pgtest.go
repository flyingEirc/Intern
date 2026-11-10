// Package pgtest provides isolated, real PostgreSQL databases for tests.
//
// Every call to New returns a DSN for a fresh database. When
// INTERN_TEST_POSTGRES_DSN points at an administrative PostgreSQL database,
// New uses that server; otherwise it starts one pinned PostgreSQL container for
// the process. Testcontainers' reaper removes that container when the test
// process exits. Nothing here substitutes an in-memory database or skips when
// Docker is unavailable.
package pgtest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// Image is the pinned multi-architecture PostgreSQL 17 image. The digest was
// read from Docker Hub's official library tag on 2026-11-09.
const Image = "postgres:17.11-trixie@sha256:ff87138949ad9a46f32ff23bd6852b52ce024a13dffe9b4087be98292cdd3b4f"

var (
	containerOnce sync.Once
	containerDSN  string
	containerErr  error
)

func randomHex(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func startContainer() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	password, err := randomHex(24)
	if err != nil {
		containerErr = err
		return
	}
	container, err := postgres.Run(ctx, Image,
		postgres.WithUsername("intern"),
		postgres.WithPassword(password),
		postgres.WithDatabase("postgres"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		containerErr = fmt.Errorf("start real PostgreSQL test container: %w", err)
		return
	}
	containerDSN, containerErr = container.ConnectionString(ctx, "sslmode=disable")
}

// RequireDSN prevents a container worker from trying to start nested Docker.
// Native harnesses provide an administrative URL on their fixture network.
func RequireDSN(t testing.TB) {
	t.Helper()
	if os.Getenv("INTERN_TEST_POSTGRES_DSN") == "" {
		t.Fatal("container worker requires INTERN_TEST_POSTGRES_DSN from its PostgreSQL harness")
	}
}

// New returns a DSN for one isolated database and drops it during test cleanup.
// The returned DSN is a secret and must never be logged or written to evidence.
func New(t testing.TB) string {
	t.Helper()
	admin := adminDSN(t)
	parsed := parseAdminDSN(t, admin)
	name := "intern_test_" + randomSuffix(t)
	db := createDatabase(t, admin, name)
	t.Cleanup(func() { dropDatabase(t, db, name) })
	parsed.Path = "/" + name
	return parsed.String()
}

func adminDSN(t testing.TB) string {
	t.Helper()
	if admin := os.Getenv("INTERN_TEST_POSTGRES_DSN"); admin != "" {
		return admin
	}
	containerOnce.Do(startContainer)
	if containerErr != nil {
		t.Fatal(containerErr)
	}
	return containerDSN
}

func parseAdminDSN(t testing.TB, admin string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("INTERN_TEST_POSTGRES_DSN must be a postgres URL: %v", err)
	}
	if parsed.Scheme != "postgres" && parsed.Scheme != "postgresql" {
		t.Fatal("INTERN_TEST_POSTGRES_DSN must use postgres:// or postgresql://")
	}
	return parsed
}

func randomSuffix(t testing.TB) string {
	t.Helper()
	suffix, err := randomHex(12)
	if err != nil {
		t.Fatal(err)
	}
	return suffix
}

func createDatabase(t testing.TB, admin, name string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", admin)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err = db.ExecContext(ctx, fmt.Sprintf(`CREATE DATABASE %q`, name)); err != nil {
		t.Fatal(errors.Join(fmt.Errorf("create isolated PostgreSQL database: %w", err), db.Close()))
	}
	return db
}

func dropDatabase(t testing.TB, db *sql.DB, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %q WITH (FORCE)`, name)); err != nil {
		t.Errorf("drop isolated PostgreSQL database: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Errorf("close PostgreSQL administrative connection: %v", err)
	}
}
