package ftp

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net/textproto"
	"strings"
)

var (
	ErrOwnership       = errors.New("FTP namespace ownership refused")
	ErrCommitUncertain = errors.New("FTP publication outcome uncertain")
	ErrCleanupPending  = errors.New("FTP temporary object cleanup pending")
	// ErrTLS reports that an FTPS channel was refused: the server certificate
	// failed verification, or TLS itself was never negotiated. It never carries
	// the peer's message, which can name a host or an authority.
	ErrTLS = errors.New("FTP TLS refused")
)

// Error contains only safe operation metadata. Raw server replies, paths,
// addresses and credentials are never retained in its public error chain.
type Error struct {
	Op        string
	Kind      string
	ReplyCode int
	cause     error
}

func (e *Error) Error() string { return "FTP repository operation failed" }
func (e *Error) Unwrap() error { return e.cause }

// CleanupError hands one temporary key to the caller's durable cleanup ledger.
// It is not a queue and does not confer permission to delete published objects.
type CleanupError struct {
	Path  string
	cause error
}

func (e *CleanupError) Error() string        { return "FTP temporary object cleanup pending" }
func (e *CleanupError) Unwrap() error        { return e.cause }
func (e *CleanupError) Is(target error) bool { return target == ErrCleanupPending }

func failure(op string, err error) error {
	if err == nil {
		return nil
	}
	result := &Error{Op: op, Kind: "transport"}
	var reply *textproto.Error
	var previous *Error
	if errors.As(err, &reply) {
		result.ReplyCode = reply.Code
		result.Kind = "reply"
	} else if errors.As(err, &previous) {
		result.ReplyCode = previous.ReplyCode
		result.Kind = previous.Kind
	}
	// A refusal keeps its precise category across every layer that wraps it:
	// the certificate error behind it is deliberately not retained, so the kind
	// an earlier operation reported is the only faithful description left.
	if kind, ok := tlsKind(err); ok && !strings.HasPrefix(result.Kind, "tls_") {
		result.cause = errors.Join(result.cause, ErrTLS)
		result.Kind = kind
	}
	if result.ReplyCode == 552 {
		result.Kind = "quota"
	}
	// 550 can mean absence, permission or another server-specific refusal.
	// Keep the exact code without guessing absence or disclosing its message.
	for _, safe := range []struct {
		err  error
		kind string
	}{
		{fs.ErrNotExist, "not_exist"},
		{fs.ErrPermission, "permission"},
		{fs.ErrClosed, "closed"},
		{fs.ErrInvalid, "invalid"},
		{fs.ErrExist, "exists"},
		{io.ErrShortWrite, "short_write"},
		{context.Canceled, "canceled"},
		{context.DeadlineExceeded, "deadline"},
		{ErrOwnership, "ownership"},
		{ErrCommitUncertain, "commit_uncertain"},
	} {
		if errors.Is(err, safe.err) {
			result.cause = errors.Join(result.cause, safe.err)
			result.Kind = safe.kind
		}
	}
	var cleanup *CleanupError
	if errors.As(err, &cleanup) {
		return errors.Join(result, cleanup)
	}
	return result
}
