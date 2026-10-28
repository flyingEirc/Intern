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
// The current effective uid must not be root. The server keeps running until the
// caller shuts it down, the context ends, or Kill is called.
func StartMysqld(ctx context.Context, opts MysqldOptions) (*MysqldProcess, error) {
	if os.Geteuid() == 0 {
		return nil, ErrRehearsalRoot
	}
	args, err := MysqldArgs(opts)
	if err != nil {
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
// nil only for a clean exit.
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
