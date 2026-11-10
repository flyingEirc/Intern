package store

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/flyingEirc/Intern/internal/pgtest"
)

func TestConnectFailureHidesIdentity(t *testing.T) {
	user := "userInternSecret"
	password := "pwInternSecret99"
	database := "dbInternSecret"
	host := "127.0.0.1"
	dsn := refusedPostgresDSN(t, user, password, host, database)
	secrets := []string{"user=", "database=", "password=", user, password, database, host, dsn}

	t.Run("Open unreachable", func(t *testing.T) {
		_, err := Open(t.Context(), dsn)
		requireHiddenIdentity(t, err, secrets)
		if !errors.Is(err, ErrPostgresUnreachable) || !errors.Is(err, syscall.ECONNREFUSED) {
			t.Fatalf("unreachable dial class: %v", err)
		}
	})
	t.Run("ReadStatus unreachable", func(t *testing.T) {
		_, err := ReadStatus(t.Context(), dsn)
		requireHiddenIdentity(t, err, secrets)
		if !errors.Is(err, ErrPostgresUnreachable) || !errors.Is(err, syscall.ECONNREFUSED) {
			t.Fatalf("unreachable dial class: %v", err)
		}
	})

	admin := pgtest.New(t)
	wrong := withPassword(t, admin, "wrong-"+password)
	authSecrets := identitySecrets(t, admin)
	authSecrets = append(authSecrets, identitySecrets(t, wrong)...)
	authSecrets = append(authSecrets, "user=", "database=", "password=")
	t.Run("Open authentication", func(t *testing.T) {
		_, err := Open(t.Context(), wrong)
		requireHiddenIdentity(t, err, authSecrets)
		requireAuthenticationClass(t, err)
	})
	t.Run("ReadStatus authentication", func(t *testing.T) {
		_, err := ReadStatus(t.Context(), wrong)
		requireHiddenIdentity(t, err, authSecrets)
		requireAuthenticationClass(t, err)
	})
}

func requireAuthenticationClass(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrPostgresAuthentication) {
		t.Fatalf("authentication class: %v", err)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || !strings.HasPrefix(pgErr.Code, "28") {
		t.Fatalf("authentication chain: %v", err)
	}
}

func requireHiddenIdentity(t *testing.T, err error, secrets []string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected connection failure")
	}
	text := err.Error()
	for _, secret := range secrets {
		if secret != "" && strings.Contains(text, secret) {
			t.Fatalf("connection identity %q leaked: %s", secret, text)
		}
	}
}

func refusedPostgresDSN(t *testing.T, user, password, host, database string) string {
	t.Helper()
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable&connect_timeout=2",
		user, password, host, refusedTCPPort(t), database)
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

func withPassword(t *testing.T, dsn, password string) string {
	t.Helper()
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	user := ""
	if parsed.User != nil {
		user = parsed.User.Username()
	}
	parsed.User = url.UserPassword(user, password)
	return parsed.String()
}

func identitySecrets(t *testing.T, dsn string) []string {
	t.Helper()
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	password, _ := parsed.User.Password()
	secrets := []string{dsn, parsed.Hostname(), strings.TrimPrefix(parsed.Path, "/"), password}
	if parsed.User != nil {
		secrets = append(secrets, parsed.User.Username())
	}
	return secrets
}
