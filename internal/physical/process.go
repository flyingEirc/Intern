package physical

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"time"
)

const diagnosticLimit = 32 * 1024

// defaultsFileArgument selects the anonymous option-file descriptor, not a secret.
// Linux supervision inherits that descriptor as fd 4.
const defaultsFileArgument = "--defaults-file=/proc/self/fd/4"

// errSupervisorEncoding is the explicit limit for non-UTF-8 paths and arguments.
// Other supervisor failures stay opaque so tool paths and credentials are not echoed.
var errSupervisorEncoding = errors.New("physical: supervisor configuration requires UTF-8 paths and arguments")

// boundedOutput never exposes tool diagnostics in returned errors. Native output
// can contain credentials, SQL or DSNs; only the completion predicate is public.
type boundedOutput struct {
	mu        sync.Mutex
	data      []byte
	truncated bool
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if n >= diagnosticLimit {
		b.data = append(b.data[:0], p[n-diagnosticLimit:]...)
		b.truncated = true
		return n, nil
	}
	if len(b.data)+n > diagnosticLimit {
		b.data = append(b.data[:0], b.data[len(b.data)+n-diagnosticLimit:]...)
		b.truncated = true
	}
	b.data = append(b.data, p...)
	return n, nil
}

func (b *boundedOutput) completed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.HasSuffix(strings.TrimSpace(string(b.data)), "completed OK!")
}

type cancelWriter struct {
	next   io.Writer
	cancel context.CancelCauseFunc
}

func (w cancelWriter) Write(p []byte) (int, error) {
	n, err := w.next.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.cancel(err)
	}
	return n, err
}

type processSpec struct {
	binary  string
	args    []string
	dir     string
	cleanup string
	// credential, when set, is the private defaults-file body. Linux passes it
	// through an anonymous 0600 descriptor and does not create a directory entry.
	credential []byte
	stdin      io.Reader
	stdout     io.Writer
	stderr     io.Writer
}

func runProcess(ctx context.Context, spec processSpec) error {
	childCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	if spec.stdout != nil {
		spec.stdout = cancelWriter{next: spec.stdout, cancel: cancel}
	}
	err := executeProcess(childCtx, spec)
	if cause := context.Cause(childCtx); cause != nil {
		return processCancellation{cause: errors.Join(cause, ctx.Err())}
	}
	if err != nil {
		if errors.Is(err, errSupervisorEncoding) {
			return errSupervisorEncoding
		}
		// Do not wrap exec/path errors: their text may contain secrets.
		return errors.New("physical: native tool failed to start, finish, or drain its pipes")
	}
	return nil
}

// processCancellation preserves errors.Is without exposing caller-controlled
// cause strings that might include credentials or a full DSN.
type processCancellation struct{ cause error }

func (e processCancellation) Error() string {
	return "physical: native tool canceled or downstream I/O failed"
}

func (e processCancellation) Unwrap() error { return e.cause }

func toolContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout == 0 {
		timeout = 24 * time.Hour
	}
	return context.WithTimeout(ctx, timeout)
}
