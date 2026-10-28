package physical

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ErrRehearsalRoot refuses a rehearsal started by root. A recovered datadir is
// not a disposable object for a privileged process, and MySQL itself refuses to
// run as root without an explicit override.
var ErrRehearsalRoot = errors.New("physical: rehearsal refuses to run as root")

// ErrMysqldUnavailable reports a platform whose lifecycle cannot be supervised.
var ErrMysqldUnavailable = errors.New("physical: mysqld rehearsal requires Linux process supervision")

type MysqldOptions struct {
	Binary   string
	DataDir  string
	Socket   string
	PIDFile  string
	LogError string
}

// MysqldArgs returns the fixed rehearsal argv: no option files, unix socket
// only, no grants, no replica start and no binary log. It never includes a
// credential.
func MysqldArgs(opts MysqldOptions) ([]string, error) {
	if opts.Binary == "" || strings.ContainsAny(opts.Binary, "\x00\r\n") {
		return nil, errors.New("physical: mysqld binary is required")
	}
	paths := []struct {
		name, value string
	}{
		{"datadir", opts.DataDir}, {"socket", opts.Socket}, {"pid-file", opts.PIDFile}, {"log-error", opts.LogError},
	}
	for _, path := range paths {
		if !filepath.IsAbs(path.value) || strings.ContainsAny(path.value, "\x00\r\n") {
			return nil, errors.New("physical: mysqld paths must be absolute and contain no control separators")
		}
	}
	return []string{
		"--no-defaults",
		"--datadir=" + opts.DataDir,
		"--socket=" + opts.Socket,
		"--pid-file=" + opts.PIDFile,
		"--log-error=" + opts.LogError,
		"--skip-networking",
		"--skip-grant-tables",
		"--skip-replica-start",
		"--skip-log-bin",
	}, nil
}

// MysqldProcess is one supervised rehearsal server. The caller owns the
// shutdown sequence: it issues SHUTDOWN over the unix socket, then waits with a
// bound and calls Kill only when that bound expires.
type MysqldProcess struct {
	process *supervisedProcess
	binary  string
}

// StartMysqld starts the rehearsal server in its own supervised process group.
// The current effective uid must not be root. The configured binary is validated
// before anything is launched: the supervisor always starts /proc/self/exe, so a
// missing or non-executable server would otherwise look like a successful start
// and only surface later as an opaque exit. The server keeps running until the
// caller shuts it down, the context ends, or Kill is called.
func StartMysqld(ctx context.Context, opts MysqldOptions) (*MysqldProcess, error) {
	if os.Geteuid() == 0 {
		return nil, ErrRehearsalRoot
	}
	args, err := MysqldArgs(opts)
	if err != nil {
		return nil, err
	}
	if err = validateMysqldBinary(opts.Binary); err != nil {
		return nil, err
	}
	info, err := os.Lstat(opts.DataDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("physical: rehearsal datadir must be an existing non-symlink directory")
	}
	process, err := startSupervisedProcess(ctx, processSpec{binary: opts.Binary, args: args, dir: opts.DataDir})
	if err != nil {
		return nil, err
	}
	return &MysqldProcess{process: process, binary: opts.Binary}, nil
}

// validateMysqldBinary requires the configured server to be an absolute path to
// an existing regular file the current effective uid may execute. Every check is
// synchronous, so an unusable server is refused before a supervisor exists and
// the refusal is the same on every platform.
func validateMysqldBinary(binary string) error {
	if !filepath.IsAbs(binary) || strings.ContainsAny(binary, "\x00\r\n") {
		return errors.New("physical: mysqld binary must be an absolute path")
	}
	info, err := os.Stat(binary)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("physical: mysqld binary must be an existing regular file")
	}
	if !executableForCurrentUser(binary, info) {
		return errors.New("physical: mysqld binary is not executable by this user")
	}
	return nil
}

// PID is the supervisor process id, not the tool's.
func (p *MysqldProcess) PID() int {
	if p == nil || p.process == nil {
		return 0
	}
	return p.process.pid()
}

// Exited is closed once the supervised server exits, whichever reason ended it.
func (p *MysqldProcess) Exited() <-chan struct{} {
	if p == nil || p.process == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return p.process.exited()
}

// WaitExit waits for the supervised server to exit, bounded by ctx. It returns
// nil only for a clean exit. A teardown this process requested through Kill is
// clean whichever side ended it first: the SIGKILL, or the supervisor's own
// teardown after it observed the lifetime-pipe close.
func (p *MysqldProcess) WaitExit(ctx context.Context) error {
	if p == nil || p.process == nil {
		return errors.New("physical: no rehearsal process")
	}
	return p.process.waitExit(ctx)
}

// Kill terminates the supervised group. It is the last resort after a bounded
// shutdown wait; the supervisor kills the tool group when the parent lifetime
// ends, and a second signal reaches the supervisor itself.
func (p *MysqldProcess) Kill() error {
	if p == nil || p.process == nil {
		return nil
	}
	return p.process.kill()
}
