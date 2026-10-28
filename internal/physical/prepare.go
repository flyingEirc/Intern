package physical

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// PrepareOptions describes one isolated `xtrabackup --prepare` invocation.
// Every invocation starts with --no-defaults so no host option file can change
// tool behaviour. ApplyLogOnly marks an intermediate step of a chain, and
// IncrementalDir is the extracted incremental being applied to TargetDir.
type PrepareOptions struct {
	Binary         string
	TargetDir      string
	ApplyLogOnly   bool
	IncrementalDir string
	UseMemory      int64
	Timeout        time.Duration
}

// PrepareArgs returns the exact argv for one prepare step. The first element is
// always --no-defaults, so callers cannot point the tool at a login path.
func PrepareArgs(opts PrepareOptions) ([]string, error) {
	if err := validatePrepareOptions(opts); err != nil {
		return nil, err
	}
	args := []string{"--no-defaults", "--prepare", "--target-dir=" + opts.TargetDir}
	if opts.ApplyLogOnly {
		args = append(args, "--apply-log-only")
	}
	if opts.IncrementalDir != "" {
		args = append(args, "--incremental-dir="+opts.IncrementalDir)
	}
	if opts.UseMemory > 0 {
		args = append(args, "--use-memory="+strconv.FormatInt(opts.UseMemory, 10))
	}
	return args, nil
}

func validatePrepareOptions(opts PrepareOptions) error {
	if opts.UseMemory < 0 {
		return errors.New("physical: prepare memory must not be negative")
	}
	if strings.ContainsAny(opts.Binary, "\x00\r\n") || strings.ContainsAny(opts.TargetDir, "\x00\r\n") || strings.ContainsAny(opts.IncrementalDir, "\x00\r\n") {
		return errors.New("physical: invalid prepare options")
	}
	if !filepath.IsAbs(opts.Binary) || !filepath.IsAbs(opts.TargetDir) || opts.IncrementalDir != "" && !filepath.IsAbs(opts.IncrementalDir) {
		return errors.New("physical: prepare binary and directories must be absolute")
	}
	return validateXtrabackupBinary(opts.Binary)
}

// Prepare applies one prepare step to an already extracted, authenticated chain
// directory. It does not verify that the chain is coherent; the caller reads the
// resulting xtrabackup_checkpoints after the final step. Tool diagnostics are
// never returned in the error.
func Prepare(ctx context.Context, opts PrepareOptions) error {
	args, err := PrepareArgs(opts)
	if err != nil {
		return err
	}
	ctx, cancel := toolContext(ctx, opts.Timeout)
	defer cancel()
	var diagnostic boundedOutput
	return runProcess(ctx, processSpec{binary: opts.Binary, args: args, stdout: &diagnostic, stderr: &diagnostic})
}
