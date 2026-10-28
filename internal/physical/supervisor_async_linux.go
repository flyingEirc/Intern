//go:build linux

package physical

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
)

// supervisedProcess is the asynchronous form of runProcess for tools that must
// outlive one call, such as the rehearsal server. The supervisor is always the
// same private self-reexec entrypoint, so the tool runs in its own process
// group and its descendants are reaped when the group is torn down.
type supervisedProcess struct {
	cmd      *exec.Cmd
	writeEnd *os.File
	written  chan error

	done    chan struct{}
	once    sync.Once
	waitErr error

	// killRequested records a teardown this process asked for through kill.
	killRequested atomic.Bool
}

func startSupervisedProcess(ctx context.Context, spec processSpec) (*supervisedProcess, error) {
	payload, err := supervisorPayload(spec)
	if err != nil {
		return nil, err
	}
	if credentialArg(spec.args) != (len(spec.credential) > 0) {
		return nil, errors.New("physical: invalid supervisor configuration")
	}
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	extra, err := supervisorFiles(readEnd, spec.credential)
	if err != nil {
		closePipe(readEnd)
		closePipe(writeEnd)
		return nil, errors.New("physical: cannot start supervised tool")
	}
	// #nosec G204 -- fixed self-exe path and one fixed activation argument; the
	// configured tool and argv travel through the inherited private descriptor.
	cmd := exec.CommandContext(ctx, "/proc/self/exe", supervisorArg)
	cmd.ExtraFiles = extra
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = spec.stdin, spec.stdout, spec.stderr
	cmd.Cancel = func() error { return writeEnd.Close() }
	cmd.WaitDelay = supervisorWaitDelay
	if err = cmd.Start(); err != nil {
		closeSupervisorFiles(extra, writeEnd)
		return nil, errors.New("physical: native tool failed to start")
	}
	closeSupervisorFiles(extra, nil)
	process := &supervisedProcess{cmd: cmd, writeEnd: writeEnd, written: make(chan error, 1), done: make(chan struct{})}
	go func() { process.written <- writeSupervisorConfig(writeEnd, payload) }()
	go process.collect(cmd)
	return process, nil
}

func (p *supervisedProcess) exited() <-chan struct{} { return p.done }

func (p *supervisedProcess) pid() int {
	if p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

func closeSupervisorFiles(files []*os.File, extra *os.File) {
	for _, file := range files {
		closePipe(file)
	}
	if extra != nil {
		closePipe(extra)
	}
}

func (p *supervisedProcess) collect(cmd *exec.Cmd) {
	err := cmd.Wait()
	closePipe(p.writeEnd)
	writeErr := <-p.written
	p.once.Do(func() { p.waitErr = errors.Join(err, writeErr); close(p.done) })
}

func (p *supervisedProcess) waitExit(ctx context.Context) error {
	select {
	case <-p.done:
		return p.teardownResult()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// teardownResult reports the wait result. When this process asked for the
// teardown, the supervisor's own teardown exit is as clean as the SIGKILL:
// closing the lifetime pipe can let the supervisor reap the tool group and exit
// with its teardown code before the signal reaches it. Any other exit or error
// stays a failure.
func (p *supervisedProcess) teardownResult() error {
	if p.waitErr == nil || !p.killRequested.Load() {
		return p.waitErr
	}
	if !requestedKillExit(p.waitErr) {
		return p.waitErr
	}
	return nil
}

// requestedKillExit reports whether err carries nothing but an exit status a
// kill requested by this process produces: a SIGKILL, or the supervisor's own
// teardown exit when it observed the lifetime-pipe close first. A different
// signal, a plain error, or a second error joined next to the exit status is not
// a requested teardown.
func requestedKillExit(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		return allRequestedKillExits(joined.Unwrap())
	}
	if wrapped := errors.Unwrap(err); wrapped != nil {
		return requestedKillExit(wrapped)
	}
	return requestedKillExitStatus(err)
}

// allRequestedKillExits requires every joined part to be a requested kill exit;
// an empty join carries no exit status at all.
func allRequestedKillExits(parts []error) bool {
	if len(parts) == 0 {
		return false
	}
	for _, part := range parts {
		if !requestedKillExit(part) {
			return false
		}
	}
	return true
}

// requestedKillExitStatus reports whether err is the wait status of a process
// this process ended with SIGKILL, or of the supervisor that tore the tool group
// down after observing the lifetime-pipe close.
func requestedKillExitStatus(err error) bool {
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return false
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok {
		return false
	}
	if status.Signaled() {
		return status.Signal() == syscall.SIGKILL
	}
	return status.Exited() && status.ExitStatus() == supervisorTeardownExit
}

func (p *supervisedProcess) kill() error {
	// Record the request before either teardown step: closing the pipe can win
	// the race against the signal, and the wait result must still be recognized
	// as the teardown this process asked for.
	p.killRequested.Store(true)
	// Closing the lifetime pipe makes the supervisor tear down the tool group.
	closePipe(p.writeEnd)
	if p.cmd.Process == nil {
		return nil
	}
	// The supervisor itself may already be gone; that is the desired state.
	err := p.cmd.Process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}
