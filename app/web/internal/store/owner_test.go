package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func ownerPID(t *testing.T, s *Store) int {
	t.Helper()
	var pid int
	if err := s.db.QueryRowContext(t.Context(), "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	return pid
}

func TestCanceledReadPreservesOwnerSession(t *testing.T) {
	s, dsn := openTestStoreDSN(t)
	pid := ownerPID(t, s)
	for _, deadline := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		if deadline {
			cancel()
			ctx, cancel = context.WithTimeout(t.Context(), 100*time.Millisecond)
		}
		result := make(chan error, 1)
		go func() {
			var ignored any
			result <- s.db.QueryRowContext(ctx, "SELECT pg_sleep(0.3)").Scan(&ignored)
		}()
		waitForConnection(t, s)
		if !deadline {
			cancel()
		}
		if err := <-result; err == nil {
			t.Fatal("canceled read succeeded")
		}
		cancel()
		if got := ownerPID(t, s); got != pid {
			t.Fatalf("owner session changed: %d -> %d", pid, got)
		}
		if err := s.Ready(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(t.Context(), dsn); !errors.Is(err, ErrOwnerInUse) {
			t.Fatalf("canceled read released ownership: %v", err)
		}
	}
}

func TestCanceledPartiallyConsumedRowsPreserveOwnerSession(t *testing.T) {
	s, _ := openTestStoreDSN(t)
	pid := ownerPID(t, s)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	rows, err := s.db.QueryContext(ctx, "SELECT generate_series(1, 10000)")
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Next() {
		t.Fatalf("no first row: %v", rows.Err())
	}
	cancel()
	if err = rows.Close(); err != nil {
		t.Fatal(err)
	}
	if got := ownerPID(t, s); got != pid {
		t.Fatalf("canceled rows replaced owner: %d -> %d", pid, got)
	}
	if err = s.Ready(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestStatementTimeoutPreservesOwnerSession(t *testing.T) {
	s, _ := openTestStoreDSN(t)
	pid := ownerPID(t, s)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, "SELECT pg_sleep(10)"); err == nil {
		t.Fatal("overlong statement succeeded")
	}
	if got := ownerPID(t, s); got != pid {
		t.Fatalf("statement timeout replaced owner: %d -> %d", pid, got)
	}
}

func TestCanceledPingAndResetPreserveOwnerSession(t *testing.T) {
	s, _ := openTestStoreDSN(t)
	pid := ownerPID(t, s)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.db.PingContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled ping = %v", err)
	}
	conn, err := s.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = conn.Raw(func(raw any) error {
		owned := raw.(*ownerConnection)
		pingErr := owned.Ping(ctx)
		if !errors.Is(pingErr, context.Canceled) {
			return errors.New("driver ping ignored cancellation")
		}
		// Force stdlib's idle liveness check on this same physical session.
		time.Sleep(1100 * time.Millisecond)
		return owned.ResetSession(ctx)
	}); err != nil {
		t.Fatal(err)
	}
	if err = conn.Close(); err != nil {
		t.Fatal(err)
	}
	if got := ownerPID(t, s); got != pid {
		t.Fatalf("canceled health check replaced owner: %d -> %d", pid, got)
	}
}

func TestTerminatedOwnerFailsClosedWithoutReconnect(t *testing.T) {
	s, dsn := openTestStoreDSN(t)
	terminateOwner(t, dsn, ownerPID(t, s))
	requireOwnerLost(t, s)
	requireNoReconnect(t, s)
	replacement, err := Open(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	if err = s.Audit(t.Context(), SystemAudit("must.not.commit", AuditDetail{})); !errors.Is(err, ErrOwnerLost) {
		t.Fatalf("old owner mutation = %v", err)
	}
}

func terminateOwner(t *testing.T, dsn string, pid int) {
	t.Helper()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	var terminated bool
	if err = admin.QueryRowContext(t.Context(), "SELECT pg_terminate_backend($1)", pid).Scan(&terminated); err != nil || !terminated {
		t.Fatalf("terminate owner = %v, %v", terminated, err)
	}
}

func requireOwnerLost(t *testing.T, s *Store) {
	t.Helper()
	watchCtx, stop := context.WithCancel(t.Context())
	defer stop()
	go s.Watch(watchCtx)
	select {
	case <-s.Lost():
	case <-time.After(10 * time.Second):
		t.Fatal("terminated owner did not signal loss")
	}
	if !errors.Is(s.Err(), ErrOwnerLost) {
		t.Fatalf("owner error = %v", s.Err())
	}
}

func requireNoReconnect(t *testing.T, s *Store) {
	t.Helper()
	for range 3 {
		if err := s.Ready(t.Context()); err == nil {
			t.Fatal("lost owner reconnected")
		}
	}
}

func TestOwnerSessionLostClassification(t *testing.T) {
	canceled := &pgconn.PgError{Severity: "ERROR", SeverityUnlocalized: "ERROR", Code: "57014", Message: "canceling statement due to statement timeout"}
	if ownerSessionLost(canceled) {
		t.Fatal("57014 lost the session")
	}
	fatal := &pgconn.PgError{Severity: "FATAL", Code: "57P01", Message: "terminating connection due to administrator command"}
	if !ownerSessionLost(fatal) {
		t.Fatal("fatal error kept the session")
	}
	if ownerSessionLost(context.Canceled) || ownerSessionLost(context.DeadlineExceeded) || ownerSessionLost(nil) {
		t.Fatal("context error lost the session")
	}
	if !ownerSessionLost(driver.ErrBadConn) || !ownerSessionLost(io.EOF) || !ownerSessionLost(pgconn.ErrConnClosed) {
		t.Fatal("connection error kept the session")
	}
	if !ownerSessionLost(&net.OpError{Op: "read", Net: "tcp", Err: io.EOF}) {
		t.Fatal("network error kept the session")
	}
}

func TestPingQueryCancelDoesNotLoseOwnerButTerminateDoes(t *testing.T) {
	s, dsn := openTestStoreDSN(t)
	pid := ownerPID(t, s)
	if _, err := s.db.ExecContext(t.Context(), "SET statement_timeout = '300ms'"); err != nil {
		t.Fatal(err)
	}
	setHealthProbe(t, slowPing)
	requireCanceledPing(t, s, pid)
	watchCtx, stop := context.WithCancel(t.Context())
	defer stop()
	go s.Watch(watchCtx)
	time.Sleep(1500 * time.Millisecond)
	requireStillOwned(t, s)
	clearHealthProbe()
	if _, err := s.db.ExecContext(t.Context(), "SET statement_timeout = '5s'"); err != nil {
		t.Fatal(err)
	}
	requireSamePID(t, s, pid)
	if err := s.Ready(t.Context()); err != nil {
		t.Fatal(err)
	}
	terminateOwner(t, dsn, pid)
	requireLostQuickly(t, s)
}

func slowPing(ctx context.Context, conn *ownerConnection) error {
	_, err := conn.Conn.Conn().Exec(ctx, "SELECT pg_sleep(2)")
	return err
}

func requireCanceledPing(t *testing.T, s *Store, pid int) {
	t.Helper()
	pingErr := s.db.PingContext(t.Context())
	var pgErr *pgconn.PgError
	if !errors.As(pingErr, &pgErr) || pgErr.Code != "57014" {
		t.Fatalf("ping = %v", pingErr)
	}
	if ownerSessionLost(pingErr) {
		t.Fatal("57014 classified as session loss")
	}
	requireSamePID(t, s, pid)
}

func requireStillOwned(t *testing.T, s *Store) {
	t.Helper()
	select {
	case <-s.Lost():
		t.Fatalf("watch treated statement timeout as owner loss: %v", s.Err())
	default:
	}
}

func requireSamePID(t *testing.T, s *Store, pid int) {
	t.Helper()
	if got := ownerPID(t, s); got != pid {
		t.Fatalf("owner session changed: %d -> %d", pid, got)
	}
}

func requireLostQuickly(t *testing.T, s *Store) {
	t.Helper()
	start := time.Now()
	select {
	case <-s.Lost():
		if elapsed := time.Since(start); elapsed > time.Second+500*time.Millisecond {
			t.Fatalf("terminate detected in %s", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("terminated owner did not fail closed")
	}
	if !errors.Is(s.Err(), ErrOwnerLost) {
		t.Fatalf("owner error = %v", s.Err())
	}
}

func setHealthProbe(t *testing.T, probe func(context.Context, *ownerConnection) error) {
	t.Helper()
	healthProbeMu.Lock()
	healthProbe = probe
	healthProbeMu.Unlock()
	t.Cleanup(clearHealthProbe)
}

func clearHealthProbe() {
	healthProbeMu.Lock()
	healthProbe = nil
	healthProbeMu.Unlock()
}
