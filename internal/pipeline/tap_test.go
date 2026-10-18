package pipeline

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"testing"
)

func TestTaps(t *testing.T) {
	for _, payload := range []string{"", "deterministic pipeline bytes"} {
		t.Run(sourceErrorNameForPayload(payload), func(t *testing.T) {
			var count CountTap
			var digest SHA256Tap
			_, _, sink, _ := newTestChain()
			src := sourceFunc(func(_ context.Context, w io.Writer) error {
				_, err := io.WriteString(w, payload)
				return err
			})
			_, err := Run(context.Background(), src, []Stage{count.Stage, digest.Stage}, sink)
			if err != nil {
				t.Fatal(err)
			}
			if count.Bytes() != int64(len(payload)) || digest.Sum() != sha256.Sum256([]byte(payload)) {
				t.Fatalf("count = %d, digest = %x", count.Bytes(), digest.Sum())
			}
			if sink.String() != payload {
				t.Fatalf("tap altered bytes: %q", sink.String())
			}
			assertSinkCalls(t, sink, 1, 0)
		})
	}
}

func sourceErrorNameForPayload(payload string) string {
	if payload == "" {
		return "empty"
	}
	return "nonempty"
}

type partialWriter struct{}

func (partialWriter) Write([]byte) (int, error) { return 2, io.ErrUnexpectedEOF }

func TestTapsOnlyObserveAcceptedBytes(t *testing.T) {
	var count CountTap
	var digest SHA256Tap
	digestWriter, err := digest.Stage(partialWriter{})
	if err != nil {
		t.Fatal(err)
	}
	writer, err := count.Stage(digestWriter)
	if err != nil {
		t.Fatal(err)
	}
	n, err := writer.Write([]byte("abcdef"))
	if n != 2 || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("write = %d, %v", n, err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	if digest.closed {
		t.Fatal("count tap closed the downstream tap")
	}
	if err = digestWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if count.Bytes() != 2 || digest.Sum() != sha256.Sum256([]byte("ab")) {
		t.Fatalf("count = %d, digest = %x", count.Bytes(), digest.Sum())
	}
}

func TestTapLifecycle(t *testing.T) {
	var count CountTap
	var digest SHA256Tap
	tests := []struct {
		name  string
		stage Stage
		tap   io.WriteCloser
	}{
		{"count", count.Stage, &count},
		{"SHA256", digest.Stage, &digest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertTapLifecycle(t, tt.stage, tt.tap)
		})
	}
}

func assertTapLifecycle(t *testing.T, stage Stage, tap io.WriteCloser) {
	t.Helper()
	if _, err := tap.Write([]byte("not attached")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("unattached write error = %v", err)
	}
	if _, err := stage(io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := stage(io.Discard); err == nil {
		t.Fatal("tap attached twice")
	}
	for range 2 {
		if err := tap.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tap.Write([]byte("closed")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("closed write error = %v", err)
	}
}

func TestCountTapBytesConcurrent(t *testing.T) {
	var count CountTap
	writer, err := count.Stage(io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	const writes = 10000
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range writes {
			_ = count.Bytes()
		}
	}()
	for range writes {
		if _, err := writer.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	<-done
	if count.Bytes() != writes {
		t.Fatalf("count = %d, want %d", count.Bytes(), writes)
	}
}
