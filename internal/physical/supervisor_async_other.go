//go:build !linux

package physical

import "context"

type supervisedProcess struct{}

func startSupervisedProcess(context.Context, processSpec) (*supervisedProcess, error) {
	return nil, ErrMysqldUnavailable
}

func (p *supervisedProcess) waitExit(context.Context) error { return ErrMysqldUnavailable }

func (p *supervisedProcess) kill() error { return ErrMysqldUnavailable }

func (p *supervisedProcess) pid() int { return 0 }

func (p *supervisedProcess) exited() <-chan struct{} {
	closed := make(chan struct{})
	close(closed)
	return closed
}
