package physical

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ExtractIntoOptions configures extraction into a caller-owned directory. The
// caller keeps ownership of Directory on both success and failure; unlike
// Extract this primitive never removes it.
type ExtractIntoOptions struct {
	Binary    string
	Directory string
	Parallel  int
	Timeout   time.Duration
}

// ExtractInto unpacks an authenticated, trusted xbstream into an existing empty
// private directory. It refuses a missing, non-private, symlinked or non-empty
// directory, so a stale partial tree can never be mistaken for this extract.
// Input must return on cancellation; when it implements io.Closer it is closed
// on cancellation to interrupt blocked reads.
func ExtractInto(ctx context.Context, opts ExtractIntoOptions, input io.Reader) error {
	if opts.Binary == "" || input == nil || opts.Parallel < 0 || opts.Timeout < 0 {
		return errors.New("physical: extraction requires a binary, input and nonnegative limits")
	}
	if err := validateEmptyPrivateDirectory(opts.Directory); err != nil {
		return err
	}
	ctx, cancel := toolContext(ctx, opts.Timeout)
	defer cancel()
	stop := closeInputOnCancel(ctx, input)
	defer stop()
	var diagnostic boundedOutput
	args := []string{"--extract", "--parallel=" + strconv.Itoa(max(opts.Parallel, 1))}
	return runProcess(ctx, processSpec{binary: opts.Binary, args: args, dir: opts.Directory, stdin: input, stdout: &diagnostic, stderr: &diagnostic})
}

func validateEmptyPrivateDirectory(dir string) error {
	if !filepath.IsAbs(dir) || strings.ContainsAny(dir, "\x00\r\n") {
		return errors.New("physical: extraction directory must be absolute")
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return errors.New("physical: extraction directory is unavailable")
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return errors.New("physical: extraction directory must be a private non-symlink directory with mode 0700")
	}
	if !ownedByCurrentUser(info) {
		return errors.New("physical: extraction directory must be owned by the current user")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return errors.New("physical: extraction directory is unreadable")
	}
	if len(entries) != 0 {
		return errors.New("physical: extraction directory must be empty")
	}
	return nil
}
