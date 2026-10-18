// Package pipeline streams a Source through ordered writer Stages into one Sink.
//
// Stages are listed upstream first. Run builds them in reverse, closes them in
// forward order, and commits only after every write and close has succeeded.
// A Source must wait for its writers and external processes before returning;
// a write failure cancels its context so that blocked processes can terminate.
// The first write or close failure is recorded with context.WithCancelCause.
// A stage may report that failure from a worker goroutine. Each writer's Write
// is serial: workers coordinate downstream writes and join before Close.
// Cancel records the chain failure and does not serialize Write. Overlapping
// Write is undefined for a checked writer, CountTap, and SHA256Tap, and it
// corrupts hash.Hash. checkedWriter adds no mutex for that misuse. zstd
// encoders used with Run flush one downstream Write at a time, including when
// encoder concurrency is greater than one, and Close waits for that flush.
// Checked writers return the caller context's Err when that context has ended,
// joined with its cause when the cause is a different error. An internal abort
// returns that cause and does not match context.Canceled.
//
// CountTap and SHA256Tap are single-use, zero-value-ready stages. Pass their
// Stage methods to Run. CountTap.Bytes is safe to read while the single writer
// runs; SHA256Tap.Sum is read after Run has closed the stages.
//
// FTP and S3 sinks are not implemented. A nil Commit error is the object
// completion point, not a completed backup.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// Source pushes bytes into w and waits for any external tool to exit.
// Tool failures and cancellation must be returned as errors.
type Source interface {
	Run(ctx context.Context, w io.Writer) error
}

// Stage wraps next without taking ownership of it. Close flushes only this
// stage and returns after its workers have finished writing to next. Write on
// the stage and on next is serial.
type Stage func(next io.Writer) (io.WriteCloser, error)

// Sink receives the finished byte stream. A nil error from Commit is the
// object completion point; Abort is for an object that has not committed and
// is idempotent. Local bytes stay private until Publish. Remote sinks are not
// implemented.
type Sink interface {
	io.Writer
	Commit(ctx context.Context) (Receipt, error)
	Abort() error
}

// Receipt identifies a committed object by repository-relative path and byte size.
type Receipt struct {
	Path string
	Size int64
}

// Run executes a synchronous writer chain, closing upstream stages first.
// A source error, short write, stage Close error, or Commit error aborts the
// sink and preserves that cause alongside cleanup errors. Cancellation before
// Commit aborts and returns an empty receipt. After Commit returns nil, Run
// returns the receipt and does not call Abort, including when the caller
// context has already ended.
func Run(ctx context.Context, src Source, stages []Stage, sink Sink) (Receipt, error) {
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	chain := writerChain{
		parent: ctx,
		ctx:    runCtx,
		cancel: cancel,
		stages: make([]io.WriteCloser, len(stages)),
	}
	head, err := chain.build(stages, sink)
	if err == nil {
		err = combineSourceError(ctx, runCtx, src.Run(runCtx, head))
	}
	if err != nil {
		cancel(err)
	}
	err = joinDistinct(err, chain.close(), context.Cause(runCtx), ctx.Err())
	if err != nil {
		return Receipt{}, errors.Join(err, sink.Abort())
	}

	receipt, err := sink.Commit(runCtx)
	if err != nil {
		err = joinDistinct(err, context.Cause(runCtx), ctx.Err())
		return Receipt{}, errors.Join(err, sink.Abort())
	}
	return receipt, nil
}

type writerChain struct {
	parent context.Context
	ctx    context.Context
	cancel context.CancelCauseFunc
	stages []io.WriteCloser
}

func (c *writerChain) build(stages []Stage, sink Sink) (io.Writer, error) {
	var head io.Writer = checkedWriter{next: sink, chain: c}
	for i := len(stages) - 1; i >= 0; i-- {
		stage, err := stages[i](head)
		if err != nil {
			return nil, fmt.Errorf("construct stage %d: %w", i, err)
		}
		c.stages[i] = stage
		head = checkedWriter{next: stage, chain: c}
	}
	return head, nil
}

func (c *writerChain) close() error {
	var result error
	for i, stage := range c.stages {
		if stage == nil {
			continue
		}
		c.stages[i] = nil
		if err := stage.Close(); err != nil {
			c.cancel(err)
			if errors.Is(result, err) {
				continue
			}
			result = joinDistinct(result, fmt.Errorf("close stage %d: %w", i, err))
		}
	}
	return result
}

func (c *writerChain) failed(err error) {
	c.cancel(err)
}

type checkedWriter struct {
	next  io.Writer
	chain *writerChain
}

func (w checkedWriter) Write(p []byte) (int, error) {
	if err := w.blocked(); err != nil {
		w.chain.failed(err)
		return 0, err
	}
	n, err := w.next.Write(p)
	if n < len(p) && err == nil {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.chain.failed(err)
	}
	return n, err
}

// blocked reports why a write must not continue. Caller cancellation is the
// parent context's Err so errors.Is(err, context.Canceled) stays true even when
// the caller recorded a different cause. Only an internal abort returns that cause.
func (w checkedWriter) blocked() error {
	if w.chain.ctx.Err() == nil {
		return nil
	}
	if err := w.chain.parent.Err(); err != nil {
		return parentCancelError(w.chain.parent)
	}
	if cause := context.Cause(w.chain.ctx); cause != nil {
		return cause
	}
	return w.chain.ctx.Err()
}

func parentCancelError(parent context.Context) error {
	err := parent.Err()
	cause := context.Cause(parent)
	if cause == nil || errors.Is(err, cause) {
		return err
	}
	return errors.Join(err, cause)
}

func combineSourceError(parent, run context.Context, sourceErr error) error {
	cause := context.Cause(run)
	// Strip context.Canceled only after this chain has recorded an internal
	// failure. A source that fails because its own context was canceled, while
	// the caller context is still active and no chain failure exists, must
	// still fail the run.
	if parent.Err() == nil && cause != nil {
		sourceErr = stripCanceled(sourceErr)
	}
	return joinDistinct(cause, sourceErr, parent.Err())
}

// stripCanceled drops context.Canceled leaves from a source error after an
// internal abort. Other diagnostics are kept, and a wrapped leaf remains as
// plain text so the joined result does not match context.Canceled.
func stripCanceled(err error) error {
	if err == nil || !errors.Is(err, context.Canceled) {
		return err
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		parts := joined.Unwrap()
		kept := make([]error, 0, len(parts))
		for _, part := range parts {
			if part = stripCanceled(part); part != nil {
				kept = append(kept, part)
			}
		}
		return errors.Join(kept...)
	}
	if errors.Unwrap(err) == nil {
		return nil
	}
	return errors.New(err.Error())
}

func joinDistinct(errs ...error) error {
	var result error
	for _, err := range errs {
		result = mergeError(result, err)
	}
	return result
}

func mergeError(result, err error) error {
	switch {
	case err == nil || errors.Is(result, err):
		return result
	case result == nil:
		return err
	case errors.Is(err, result):
		return result
	default:
		return errors.Join(result, err)
	}
}
