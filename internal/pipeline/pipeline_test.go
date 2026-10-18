package pipeline

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"
)

type sourceFunc func(context.Context, io.Writer) error

func (f sourceFunc) Run(ctx context.Context, w io.Writer) error { return f(ctx, w) }

type stageSpec struct {
	name         string
	constructErr error
	writeErr     error
	closeErr     error
	short        bool
	flush        bool
	closes       int
	events       *[]string
}

func (s *stageSpec) stage(next io.Writer) (io.WriteCloser, error) {
	*s.events = append(*s.events, "construct:"+s.name)
	if s.constructErr != nil {
		return nil, s.constructErr
	}
	return &testStage{spec: s, next: next}, nil
}

type testStage struct {
	spec *stageSpec
	next io.Writer
}

func (s *testStage) Write(p []byte) (int, error) {
	if s.spec.writeErr != nil {
		return 0, s.spec.writeErr
	}
	if s.spec.short {
		return len(p) - 1, nil
	}
	return s.next.Write(p)
}

func (s *testStage) Close() error {
	s.spec.closes++
	*s.spec.events = append(*s.spec.events, "close:"+s.spec.name)
	if s.spec.flush {
		if _, err := io.WriteString(s.next, s.spec.name); err != nil {
			return err
		}
	}
	return s.spec.closeErr
}

type testSink struct {
	bytes.Buffer
	events    *[]string
	writeErr  error
	commitErr error
	abortErr  error
	short     bool
	commits   int
	aborts    int
}

func (s *testSink) Write(p []byte) (int, error) {
	if s.writeErr != nil {
		return 0, s.writeErr
	}
	if s.short {
		return len(p) - 1, nil
	}
	return s.Buffer.Write(p)
}

func (s *testSink) Commit(ctx context.Context) (Receipt, error) {
	s.commits++
	*s.events = append(*s.events, "commit")
	if err := ctx.Err(); err != nil {
		return Receipt{}, err
	}
	return Receipt{Path: "payload", Size: int64(s.Len())}, s.commitErr
}

func (s *testSink) Abort() error {
	s.aborts++
	*s.events = append(*s.events, "abort")
	return s.abortErr
}

func newTestChain() ([]*stageSpec, []Stage, *testSink, *[]string) {
	events := make([]string, 0, 16)
	specs := []*stageSpec{
		{name: "a", events: &events},
		{name: "b", events: &events},
		{name: "c", events: &events},
	}
	stages := []Stage{specs[0].stage, specs[1].stage, specs[2].stage}
	return specs, stages, &testSink{events: &events}, &events
}

func writeSource(_ context.Context, w io.Writer) error {
	_, err := io.WriteString(w, "payload")
	return err
}

func TestRunSuccessCloseOrder(t *testing.T) {
	specs, stages, sink, events := newTestChain()
	for _, spec := range specs {
		spec.flush = true
	}
	src := sourceFunc(func(ctx context.Context, w io.Writer) error {
		*events = append(*events, "source")
		return writeSource(ctx, w)
	})
	receipt, err := Run(context.Background(), src, stages, sink)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"construct:c", "construct:b", "construct:a", "source", "close:a", "close:b", "close:c", "commit"}
	if !reflect.DeepEqual(*events, want) {
		t.Fatalf("events = %v, want %v", *events, want)
	}
	if sink.String() != "payloadabc" || receipt != (Receipt{Path: "payload", Size: 10}) {
		t.Fatalf("contents = %q, receipt = %+v", sink.String(), receipt)
	}
	assertSinkCalls(t, sink, 1, 0)
	assertStageCloses(t, specs, []int{1, 1, 1})
}

func TestRunFailures(t *testing.T) {
	cause := errors.New("injected failure")
	tests := []struct {
		name    string
		setup   func([]*stageSpec, *testSink) Source
		wantErr error
		commits int
		closes  []int
	}{
		{"source", func(_ []*stageSpec, _ *testSink) Source {
			return sourceFunc(func(context.Context, io.Writer) error { return cause })
		}, cause, 0, []int{1, 1, 1}},
		{"construction", func(specs []*stageSpec, _ *testSink) Source {
			specs[1].constructErr = cause
			return sourceFunc(writeSource)
		}, cause, 0, []int{0, 0, 1}},
		{"stage write", func(specs []*stageSpec, _ *testSink) Source {
			specs[1].writeErr = cause
			return sourceFunc(writeSource)
		}, cause, 0, []int{1, 1, 1}},
		{"stage short write", func(specs []*stageSpec, _ *testSink) Source {
			specs[1].short = true
			return sourceFunc(writeSource)
		}, io.ErrShortWrite, 0, []int{1, 1, 1}},
		{"close", func(specs []*stageSpec, _ *testSink) Source {
			specs[0].closeErr = cause
			return sourceFunc(writeSource)
		}, cause, 0, []int{1, 1, 1}},
		{"sink write", func(_ []*stageSpec, sink *testSink) Source {
			sink.writeErr = cause
			return sourceFunc(writeSource)
		}, cause, 0, []int{1, 1, 1}},
		{"sink short write", func(_ []*stageSpec, sink *testSink) Source {
			sink.short = true
			return sourceFunc(writeSource)
		}, io.ErrShortWrite, 0, []int{1, 1, 1}},
		{"sink commit", func(_ []*stageSpec, sink *testSink) Source {
			sink.commitErr = cause
			return sourceFunc(writeSource)
		}, cause, 1, []int{1, 1, 1}},
		{"source swallows write error", func(_ []*stageSpec, sink *testSink) Source {
			sink.writeErr = cause
			return sourceFunc(func(_ context.Context, w io.Writer) error {
				w.Write([]byte("ignored"))
				return nil
			})
		}, cause, 0, []int{1, 1, 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			specs, stages, sink, _ := newTestChain()
			src := tt.setup(specs, sink)
			receipt, err := Run(context.Background(), src, stages, sink)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want cause %v", err, tt.wantErr)
			}
			if receipt != (Receipt{}) {
				t.Fatalf("failed run returned receipt %+v", receipt)
			}
			assertSinkCalls(t, sink, tt.commits, 1)
			assertStageCloses(t, specs, tt.closes)
		})
	}
}

func TestRunCancellationRejectsHeadWrites(t *testing.T) {
	for _, sourceErr := range []error{nil, io.EOF} {
		t.Run(sourceErrorName(sourceErr), func(t *testing.T) {
			assertCanceledSource(t, sourceErr)
		})
	}
}

func assertCanceledSource(t *testing.T, sourceErr error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	specs, stages, sink, _ := newTestChain()
	src := sourceFunc(func(_ context.Context, w io.Writer) error {
		cancel()
		n, err := w.Write([]byte("must not reach sink"))
		if n != 0 || !errors.Is(err, context.Canceled) {
			t.Fatalf("write after cancellation = %d, %v", n, err)
		}
		return sourceErr
	})
	_, err := Run(ctx, src, stages, sink)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("run error = %v", err)
	}
	if sourceErr != nil && !errors.Is(err, sourceErr) {
		t.Fatalf("source cause lost: %v", err)
	}
	assertSinkCalls(t, sink, 0, 1)
	assertStageCloses(t, specs, []int{1, 1, 1})
	if sink.Len() != 0 {
		t.Fatal("canceled head wrote into sink")
	}
}

func sourceErrorName(err error) string {
	if err == nil {
		return "nil"
	}
	return err.Error()
}

func TestRunJoinsCleanupErrors(t *testing.T) {
	cause := errors.New("source failure")
	closeA := errors.New("close a")
	closeB := errors.New("close b")
	abort := errors.New("abort failure")
	specs, stages, sink, _ := newTestChain()
	specs[0].closeErr, specs[1].closeErr, sink.abortErr = closeA, closeB, abort
	src := sourceFunc(func(context.Context, io.Writer) error { return cause })
	_, err := Run(context.Background(), src, stages, sink)
	for _, want := range []error{cause, closeA, closeB, abort} {
		if !errors.Is(err, want) {
			t.Errorf("error %v lost cause %v", err, want)
		}
	}
	if !strings.HasPrefix(err.Error(), cause.Error()) {
		t.Fatalf("original cause is not primary: %v", err)
	}
	assertStageCloses(t, specs, []int{1, 1, 1})
	assertSinkCalls(t, sink, 0, 1)
}

func TestRunWriteErrorCancelsSourceContext(t *testing.T) {
	cause := errors.New("downstream blocked")
	_, stages, sink, _ := newTestChain()
	sink.writeErr = cause
	src := sourceFunc(func(ctx context.Context, w io.Writer) error {
		_, err := w.Write([]byte("payload"))
		if !errors.Is(err, cause) || !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("write error = %v, source context = %v", err, ctx.Err())
		}
		return ctx.Err()
	})
	parent := context.Background()
	_, err := Run(parent, src, stages, sink)
	if !errors.Is(err, cause) || errors.Is(err, context.Canceled) || parent.Err() != nil {
		t.Fatalf("error = %v, parent context = %v", err, parent.Err())
	}
}

func TestRunCallerCauseCancelsHeadWrite(t *testing.T) {
	cause := errors.New("operator stop")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)
	specs, stages, sink, _ := newTestChain()
	var writeErr error
	src := sourceFunc(func(_ context.Context, w io.Writer) error {
		var n int
		n, writeErr = w.Write([]byte("must not reach sink"))
		if n != 0 {
			t.Fatalf("canceled write length = %d, error = %v", n, writeErr)
		}
		return writeErr
	})
	_, err := Run(ctx, src, stages, sink)
	if !errors.Is(writeErr, context.Canceled) || !errors.Is(writeErr, cause) {
		t.Fatalf("write error = %v, want context.Canceled and %v", writeErr, cause)
	}
	if !strings.HasPrefix(writeErr.Error(), context.Canceled.Error()) {
		t.Fatalf("write error = %v, want the parent context error first", writeErr)
	}
	if !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
		t.Fatalf("run error = %v", err)
	}
	if sink.Len() != 0 {
		t.Fatal("canceled head wrote into sink")
	}
	assertSinkCalls(t, sink, 0, 1)
	assertStageCloses(t, specs, []int{1, 1, 1})
}

func TestRunKeepsSourceDiagnosticWithoutCanceled(t *testing.T) {
	cause := errors.New("sink failed")
	diagnostic := errors.New("source saw the downstream failure")
	specs, stages, sink, _ := newTestChain()
	sink.writeErr = cause
	src := sourceFunc(func(ctx context.Context, w io.Writer) error {
		if _, err := w.Write([]byte("payload")); !errors.Is(err, cause) {
			t.Fatalf("write error = %v", err)
		}
		return errors.Join(diagnostic, ctx.Err())
	})
	parent := context.Background()
	_, err := Run(parent, src, stages, sink)
	if !errors.Is(err, cause) || !errors.Is(err, diagnostic) {
		t.Fatalf("error = %v, want %v and %v", err, cause, diagnostic)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("internal failure matched context.Canceled: %v", err)
	}
	if !strings.HasPrefix(err.Error(), cause.Error()) {
		t.Fatalf("original cause is not primary: %v", err)
	}
	if strings.Count(err.Error(), cause.Error()) != 1 || strings.Count(err.Error(), diagnostic.Error()) != 1 {
		t.Fatalf("duplicated or missing diagnostic text: %v", err)
	}
	if parent.Err() != nil {
		t.Fatalf("parent context = %v", parent.Err())
	}
	assertSinkCalls(t, sink, 0, 1)
	assertStageCloses(t, specs, []int{1, 1, 1})
}

func TestRunReleasesPartiallyConstructedStage(t *testing.T) {
	cause := errors.New("partial constructor failure")
	specs, stages, sink, _ := newTestChain()
	stages[1] = func(next io.Writer) (io.WriteCloser, error) {
		return &testStage{spec: specs[1], next: next}, cause
	}
	_, err := Run(context.Background(), sourceFunc(writeSource), stages, sink)
	if !errors.Is(err, cause) {
		t.Fatal(err)
	}
	assertStageCloses(t, specs, []int{0, 0, 1})
	assertSinkCalls(t, sink, 0, 1)
}

func TestRunIgnoresTypedNilConstructorError(t *testing.T) {
	cause := errors.New("typed nil constructor")
	specs, stages, sink, _ := newTestChain()
	stages[1] = func(io.Writer) (io.WriteCloser, error) {
		var encoder *typedNilCloser
		return encoder, cause
	}
	_, err := Run(context.Background(), sourceFunc(writeSource), stages, sink)
	if !errors.Is(err, cause) {
		t.Fatal(err)
	}
	assertStageCloses(t, specs, []int{0, 0, 1})
	assertSinkCalls(t, sink, 0, 1)
}

type typedNilCloser struct{}

func (*typedNilCloser) Write([]byte) (int, error) { panic("write on typed nil") }
func (*typedNilCloser) Close() error              { panic("close on typed nil") }

func TestRunInternalFailureIsNotCanceled(t *testing.T) {
	t.Run("source", func(t *testing.T) {
		cause := errors.New("tool exited 3")
		assertUncanceledFailure(t, cause, func(specs []*stageSpec, _ *testSink) Source {
			enableFlush(specs)
			return sourceFunc(func(ctx context.Context, w io.Writer) error {
				if err := writeSource(ctx, w); err != nil {
					return err
				}
				return cause
			})
		})
	})
	t.Run("sink write", func(t *testing.T) {
		cause := errors.New("sink full")
		assertUncanceledFailure(t, cause, func(specs []*stageSpec, sink *testSink) Source {
			enableFlush(specs)
			sink.writeErr = cause
			return sourceFunc(writeSource)
		})
	})
}

func enableFlush(specs []*stageSpec) {
	for _, spec := range specs {
		spec.flush = true
	}
}

func assertUncanceledFailure(t *testing.T, cause error, setup func([]*stageSpec, *testSink) Source) {
	t.Helper()
	specs, stages, sink, _ := newTestChain()
	src := setup(specs, sink)
	parent := context.Background()
	_, err := Run(parent, src, stages, sink)
	if !errors.Is(err, cause) {
		t.Fatalf("error = %v, want %v", err, cause)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("internal failure matched context.Canceled: %v", err)
	}
	if parent.Err() != nil {
		t.Fatalf("parent context = %v", parent.Err())
	}
	if strings.Count(err.Error(), cause.Error()) != 1 {
		t.Fatalf("duplicated error text: %v", err)
	}
	assertSinkCalls(t, sink, 0, 1)
}

func TestRunCallerCancellationIsCanceled(t *testing.T) {
	assertCanceledSource(t, nil)
}

func TestRunSourceContextCancelFailsWhileParentLives(t *testing.T) {
	t.Run("bare context.Canceled", func(t *testing.T) {
		assertSourceCancelFails(t, func(ctx context.Context, w io.Writer) error {
			if _, err := io.WriteString(w, "partial"); err != nil {
				return err
			}
			return context.Canceled
		})
	})
	t.Run("watchdog inner.Err", func(t *testing.T) {
		assertSourceCancelFails(t, func(ctx context.Context, w io.Writer) error {
			if _, err := io.WriteString(w, "partial"); err != nil {
				return err
			}
			inner, stop := context.WithCancelCause(ctx)
			stop(errors.New("watchdog"))
			return inner.Err()
		})
	})
}

func assertSourceCancelFails(t *testing.T, run func(context.Context, io.Writer) error) {
	t.Helper()
	parent := context.Background()
	specs, stages, sink, _ := newTestChain()
	receipt, err := Run(parent, sourceFunc(run), stages, sink)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if receipt != (Receipt{}) || parent.Err() != nil {
		t.Fatalf("receipt = %+v, parent = %v", receipt, parent.Err())
	}
	assertSinkCalls(t, sink, 0, 1)
	assertStageCloses(t, specs, []int{1, 1, 1})
}

type serialSink struct {
	mu     sync.Mutex
	n      int
	aborts int
}

func (s *serialSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n += len(p)
	return len(p), nil
}

func (s *serialSink) Commit(context.Context) (Receipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Receipt{Path: "payload", Size: int64(s.n)}, nil
}

func (s *serialSink) Abort() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.aborts++
	return nil
}

func TestRunCancelWithConcurrentEncoder(t *testing.T) {
	// Incompressible input forces the concurrent encoder to write downstream
	// from its own goroutines. Repeated runs make a shared chain error race.
	payload := make([]byte, 128<<10)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	for i := range 200 {
		if err := runConcurrentEncoderOnce(payload); err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
	}
}

func runConcurrentEncoderOnce(payload []byte) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink := &serialSink{}
	started := make(chan struct{})
	var once sync.Once
	src := sourceFunc(func(_ context.Context, w io.Writer) error {
		for {
			_, err := w.Write(payload)
			once.Do(func() { close(started) })
			if err != nil {
				return err
			}
		}
	})
	go func() {
		select {
		case <-started:
			cancel()
		case <-ctx.Done():
		}
	}()
	stage := func(next io.Writer) (io.WriteCloser, error) {
		return zstd.NewWriter(next, zstd.WithEncoderConcurrency(4))
	}
	_, err := Run(ctx, src, []Stage{stage}, sink)
	if !errors.Is(err, context.Canceled) {
		return err
	}
	if sink.aborts == 0 {
		return errors.New("sink was not aborted")
	}
	return nil
}

func TestRunWithoutStages(t *testing.T) {
	_, _, sink, _ := newTestChain()
	receipt, err := Run(context.Background(), sourceFunc(writeSource), nil, sink)
	if err != nil || receipt.Size != 7 || sink.String() != "payload" {
		t.Fatalf("receipt = %+v, error = %v, contents = %q", receipt, err, sink.String())
	}
	assertSinkCalls(t, sink, 1, 0)
}

func assertSinkCalls(t *testing.T, sink *testSink, commits, aborts int) {
	t.Helper()
	if sink.commits != commits || sink.aborts != aborts {
		t.Fatalf("commit/abort calls = %d/%d, want %d/%d", sink.commits, sink.aborts, commits, aborts)
	}
}

func assertStageCloses(t *testing.T, specs []*stageSpec, want []int) {
	t.Helper()
	for i, spec := range specs {
		if spec.closes != want[i] {
			t.Errorf("stage %s closed %d times, want %d", spec.name, spec.closes, want[i])
		}
	}
}
