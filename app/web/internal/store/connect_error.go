package store

import (
	"context"
	"errors"
	"net"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5/pgconn"
)

// ErrPostgresUnreachable reports that dialing PostgreSQL failed.
var ErrPostgresUnreachable = errors.New("PostgreSQL server is unreachable")

// ErrPostgresAuthentication reports that PostgreSQL rejected the login.
var ErrPostgresAuthentication = errors.New("PostgreSQL authentication failed")

// errPostgresConnect is the fixed reason for a connect failure that is neither
// a dial failure nor an authentication failure.
var errPostgresConnect = errors.New("connect to PostgreSQL failed")

// connectFailureError replaces a driver connect error with a fixed reason.
// Unwrap keeps the driver chain so errors.Is still matches dial and auth errors.
type connectFailureError struct {
	reason error
	cause  error
}

func (e *connectFailureError) Error() string { return e.reason.Error() }

func (e *connectFailureError) Unwrap() error { return e.cause }

func (e *connectFailureError) Is(target error) bool {
	return errors.Is(e.reason, target)
}

// redactConnectError hides pgconn.ConnectError, whose text always includes the
// login user and database. Other errors pass through unchanged.
func redactConnectError(err error) error {
	if err == nil {
		return nil
	}
	var connectErr *pgconn.ConnectError
	if !errors.As(err, &connectErr) {
		return err
	}
	return &connectFailureError{reason: connectReason(err), cause: err}
}

func connectReason(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	case postgresAuthenticationFailed(err):
		return ErrPostgresAuthentication
	case postgresUnreachable(err):
		return ErrPostgresUnreachable
	default:
		return errPostgresConnect
	}
}

func postgresAuthenticationFailed(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return strings.HasPrefix(pgErr.Code, "28")
}

func postgresUnreachable(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	return dialUnreachable(err)
}

func dialUnreachable(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ETIMEDOUT)
}
