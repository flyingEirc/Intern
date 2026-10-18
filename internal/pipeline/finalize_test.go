package pipeline

import (
	"context"
	"errors"
	"io"
	"testing"
)

type closingWriter struct {
	io.Writer
	close func() error
}

func (w closingWriter) Close() error { return w.close() }

func TestCancellationDuringCloseAborts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	specs, stages, sink, _ := newTestChain()
	stages[0] = func(next io.Writer) (io.WriteCloser, error) {
		return closingWriter{Writer: next, close: func() error {
			specs[0].closes++
			cancel()
			return nil
		}}, nil
	}
	_, err := Run(ctx, sourceFunc(writeSource), stages, sink)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("close cancellation error = %v", err)
	}
	assertStageCloses(t, specs, []int{1, 1, 1})
	assertSinkCalls(t, sink, 0, 1)
}

func TestCloseCannotSwallowShortWrite(t *testing.T) {
	_, _, sink, _ := newTestChain()
	sink.short = true
	stage := func(next io.Writer) (io.WriteCloser, error) {
		return closingWriter{Writer: next, close: func() error {
			next.Write([]byte("trailer"))
			return nil
		}}, nil
	}
	source := sourceFunc(func(context.Context, io.Writer) error { return nil })
	_, err := Run(context.Background(), source, []Stage{stage}, sink)
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("swallowed flush error = %v", err)
	}
	assertSinkCalls(t, sink, 0, 1)
}

type cancelDuringCommitSink struct {
	*testSink
	cancel context.CancelFunc
	fail   bool
}

func (s cancelDuringCommitSink) Commit(ctx context.Context) (Receipt, error) {
	receipt, err := s.testSink.Commit(ctx)
	s.cancel()
	if err != nil {
		return Receipt{}, err
	}
	if s.fail {
		return Receipt{}, ctx.Err()
	}
	return receipt, nil
}

func TestCommitSuccessIgnoresLaterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	specs, stages, sink, _ := newTestChain()
	wrapped := cancelDuringCommitSink{testSink: sink, cancel: cancel}
	receipt, err := Run(ctx, sourceFunc(writeSource), stages, wrapped)
	if err != nil || receipt != (Receipt{Path: "payload", Size: 7}) {
		t.Fatalf("committed receipt = %+v, %v", receipt, err)
	}
	if ctx.Err() == nil {
		t.Fatal("context was not canceled after commit")
	}
	assertSinkCalls(t, sink, 1, 0)
	assertStageCloses(t, specs, []int{1, 1, 1})
}

func TestCommitErrorStillAborts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	specs, stages, sink, _ := newTestChain()
	wrapped := cancelDuringCommitSink{testSink: sink, cancel: cancel, fail: true}
	receipt, err := Run(ctx, sourceFunc(writeSource), stages, wrapped)
	if !errors.Is(err, context.Canceled) || receipt != (Receipt{}) {
		t.Fatalf("commit error = %+v, %v", receipt, err)
	}
	assertSinkCalls(t, sink, 1, 1)
	assertStageCloses(t, specs, []int{1, 1, 1})
}
