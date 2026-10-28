//go:build linux

package physical

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync"
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
		return p.waitErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *supervisedProcess) kill() error {
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
