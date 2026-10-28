package physical

import (
	"context"
	"errors"
	"io"
	"strconv"
	"time"
)

// ExtractOptions configures the xbstream tool primitive, not database recovery.
type ExtractOptions struct {
	Binary   string
	WorkRoot string
	Parallel int
	Timeout  time.Duration
}

// Extract unpacks an authenticated, trusted xbstream into a fresh private work
// directory. It does not authenticate the archive, prepare it or start MySQL.
// The caller owns removal of the returned directory on success. On failure the
// partial directory is removed. Input must return on cancellation; when input
// implements io.Closer it is closed on cancellation to interrupt blocked reads.
func Extract(ctx context.Context, opts ExtractOptions, input io.Reader) (directory string, err error) {
	if opts.Binary == "" || input == nil || opts.Parallel < 0 || opts.Timeout < 0 {
		return "", errors.New("physical: extraction requires a binary, input and nonnegative limits")
	}
	ctx, cancel := toolContext(ctx, opts.Timeout)
	defer cancel()
	dir, err := newWorkDir(opts.WorkRoot)
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, removeWorkDir(dir))
		}
	}()
	stop := closeInputOnCancel(ctx, input)
	defer stop()
	var diagnostic boundedOutput
	args := []string{"--extract", "--parallel=" + strconv.Itoa(max(opts.Parallel, 1))}
	err = runProcess(ctx, processSpec{binary: opts.Binary, args: args, dir: dir, cleanup: dir, stdin: input, stdout: io.Discard, stderr: &diagnostic})
	if err != nil {
		return "", err
	}
	return dir, nil
}

func closeInputOnCancel(ctx context.Context, input io.Reader) func() {
	closer, ok := input.(io.Closer)
	if !ok {
		return func() {}
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		if err := closer.Close(); err != nil {
			close(done)
			return
		}
		close(done)
	})
	return func() {
		if !stop() {
			<-done
		}
	}
}
