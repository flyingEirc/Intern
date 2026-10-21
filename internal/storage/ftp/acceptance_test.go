//go:build integration

package ftp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tink-crypto/tink-go/v2/keyset"
	"github.com/tink-crypto/tink-go/v2/streamingaead"

	"github.com/flyingEirc/Intern/internal/pipeline"
)

// This worker runs as uid 1000 on the server's private container network. The
// shared volume is an independent filesystem oracle, not an FTP implementation.
func TestS1RepositoryWorker(t *testing.T) {
	if os.Getenv("INTERN_S1_ADDRESS") == "" || os.Getuid() == 0 {
		t.Fatal("real FTP fixture and non-root worker are required; never skip")
	}
	t.Run("publish", s1Publish)
	t.Run("interrupted_upload", func(t *testing.T) { s1TransportFailure(t, "interrupt") })
	t.Run("quota_552", s1Quota)
	t.Run("permission_550", s1Permission)
	t.Run("source_error", s1SourceError)
	t.Run("short_write", func(t *testing.T) { s1TransportFailure(t, "short") })
	t.Run("data_close_error", func(t *testing.T) { s1TransportFailure(t, "close") })
	t.Run("stage_close_error", s1StageClose)
	t.Run("cancel_upload", s1Cancel)
	t.Run("cleanup_retry", s1CleanupRetry)
	t.Run("instance_isolation", s1Isolation)
	t.Run("marker_conflict", s1Marker)
	t.Run("path_tls_rejection", s1Paths)
	t.Run("ciphertext_resume", s1Resume)
	t.Run("close_drain", s1CloseDrain)
	t.Run("rolling_idle", s1RollingIdle)
	t.Run("pipeline_completion", s1Completion)
	t.Run("rename_ack_lost", s1RenameAckLost)
}

func s1Require(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func s1Config(instance, user string) Config {
	password := os.Getenv("INTERN_S1_PASSWORD")
	if user == "quota" {
		password = os.Getenv("INTERN_S1_QUOTA_PASSWORD")
	}
	if password == "" {
		panic("real per-account FTP password is required")
	}
	return Config{Address: os.Getenv("INTERN_S1_ADDRESS"), Username: user, Password: password, BasePath: "/", InstanceID: instance, TLSMode: "plain", DialTimeout: 3 * time.Second, IdleTimeout: 3 * time.Second}
}

func s1Repository(t *testing.T, instance, user string) (*Repository, *s1Transport) {
	t.Helper()
	baseline := runtime.NumGoroutine()
	r, err := Open(t.Context(), s1Config(instance, user))
	s1Require(t, err)
	transport := &s1Transport{dial: r.dialContext}
	r.dialContext = transport.connect
	t.Cleanup(func() {
		err := r.Close()
		if err != nil {
			var pending *CleanupError
			if transport.expectedClosePath == "" || !errors.As(err, &pending) || pending.Path != transport.expectedClosePath || !errors.Is(err, ErrCleanupPending) {
				t.Fatal(err)
			}
		}
		s1Eventually(t, func() bool { return transport.active.Load() == 0 }, "all control and data sockets closed")
		s1Eventually(t, func() bool { return runtime.NumGoroutine() <= baseline }, "all operation goroutines drained")
	})
	return r, transport
}

func s1Disk(user, key string) string { return filepath.Join("/fixture", user, filepath.FromSlash(key)) }

func s1Payload() []byte { return bytes.Repeat([]byte("real-ftp-immutable-payload\x00"), 16384) }

func s1Absent(t *testing.T, user, key string) {
	t.Helper()
	_, err := os.Lstat(s1Disk(user, key))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expected absent remote path %s (stat error=%v)", key, err)
	}
}

func s1Transferred(t *testing.T, user, key string) {
	t.Helper()
	s1Eventually(t, func() bool {
		info, err := os.Stat(s1Disk(user, key))
		return err == nil && info.Size() > 0
	}, "real partial bytes visible on server filesystem")
}

func s1Eventually(t *testing.T, predicate func() bool, assertion string) {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for !predicate() {
		if time.Now().After(deadline) {
			t.Fatal(assertion)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func s1Create(t *testing.T, r *Repository, key string) *Sink {
	t.Helper()
	s, err := r.Create(t.Context(), key)
	s1Require(t, err)
	if s.TemporaryPath() == "" || s.TemporaryPath() == key {
		t.Fatal("missing stable private cleanup reference")
	}
	return s
}

func s1Write(t *testing.T, w io.Writer, data []byte) {
	t.Helper()
	n, err := w.Write(data)
	s1Require(t, err)
	if n != len(data) {
		t.Fatal("unexpected short write")
	}
}

func s1Publish(t *testing.T) {
	r, _ := s1Repository(t, "publish", "normal")
	s1Require(t, r.Probe(t.Context()))
	key := "publish/source/chain/1-full/payload"
	s := s1Create(t, r, key)
	part := s.TemporaryPath()
	s1Absent(t, "normal", part)
	s1Write(t, s, s1Payload())
	s1Transferred(t, "normal", part)
	s1Absent(t, "normal", key)
	listed, err := r.List(t.Context(), "")
	s1Require(t, err)
	if len(listed) != 0 {
		t.Fatal("uncommitted part leaked into published list")
	}
	receipt, err := s.Commit(t.Context())
	s1Require(t, err)
	if receipt.Path != key || receipt.Size != int64(len(s1Payload())) || s.TemporaryPath() != part {
		t.Fatal("incorrect receipt or unstable cleanup reference")
	}
	s1Absent(t, "normal", part)
	size, err := r.Size(t.Context(), key)
	s1Require(t, err)
	if size != receipt.Size {
		t.Fatal("SIZE differs from acknowledged receipt")
	}
	reader, err := r.Open(t.Context(), key, 0)
	s1Require(t, err)
	got, err := io.ReadAll(reader)
	s1Require(t, errors.Join(err, reader.Close()))
	if !bytes.Equal(got, s1Payload()) {
		t.Fatal("published bytes differ")
	}
	listed, err = r.List(t.Context(), "publish")
	s1Require(t, err)
	if !slices.Equal(listed, []string{key}) {
		t.Fatalf("published listing mismatch: %v", listed)
	}
	s1PreserveCommitted(t, r, s, key)
	s1Require(t, r.Delete(t.Context(), key))
	s1Absent(t, "normal", key)
	t.Log("S1-A1 publish: private part -> STOR -> SIZE -> RNTO -> exact RETR -> DELETE")
}

func s1PreserveCommitted(t *testing.T, r *Repository, sink *Sink, key string) {
	t.Helper()
	existing, err := os.ReadFile(s1Disk("normal", key))
	s1Require(t, err)
	_, err = r.Create(t.Context(), key)
	if !errors.Is(err, fs.ErrExist) {
		t.Fatal("existing final object not protected")
	}
	unchanged, err := os.ReadFile(s1Disk("normal", key))
	s1Require(t, err)
	if !bytes.Equal(existing, unchanged) || !bytes.Equal(unchanged, s1Payload()) {
		t.Fatal("refused Create changed existing final object")
	}
	s1Require(t, sink.Abort())
	got, err := os.ReadFile(s1Disk("normal", key))
	s1Require(t, err)
	if !bytes.Equal(got, s1Payload()) {
		t.Fatal("Abort removed or changed committed object")
	}
}

type s1Source func(context.Context, io.Writer) error

func (f s1Source) Run(ctx context.Context, w io.Writer) error { return f(ctx, w) }

func s1SourceError(t *testing.T) {
	r, _ := s1Repository(t, "source", "normal")
	key := "source/payload"
	s := s1Create(t, r, key)
	cause := errors.New("source deliberately failed after real transfer")
	_, err := pipeline.Run(t.Context(), s1Source(func(_ context.Context, w io.Writer) error {
		s1Write(t, w, s1Payload())
		s1Transferred(t, "normal", s.TemporaryPath())
		return cause
	}), nil, s)
	if !errors.Is(err, cause) {
		t.Fatal("pipeline lost source failure")
	}
	s1Absent(t, "normal", key)
	s1Absent(t, "normal", s.TemporaryPath())
	t.Log("S1-A2 source_error: real partial upload aborted without publication")
}

type s1CloseStage struct {
	io.Writer
	close func() error
}

func (s *s1CloseStage) Close() error { return s.close() }

func s1StageClose(t *testing.T) {
	r, _ := s1Repository(t, "stage", "normal")
	key := "stage/payload"
	s := s1Create(t, r, key)
	cause := errors.New("stage close deliberately failed")
	stage := func(w io.Writer) (io.WriteCloser, error) {
		return &s1CloseStage{Writer: w, close: func() error {
			s1Transferred(t, "normal", s.TemporaryPath())
			return cause
		}}, nil
	}
	_, err := pipeline.Run(t.Context(), s1Source(func(_ context.Context, w io.Writer) error {
		_, err := w.Write(s1Payload())
		return err
	}), []pipeline.Stage{stage}, s)
	if !errors.Is(err, cause) {
		t.Fatal("pipeline lost stage Close failure")
	}
	s1Absent(t, "normal", key)
	s1Absent(t, "normal", s.TemporaryPath())
	t.Log("S1-A2 stage_close_error: real transferred bytes never published")
}

func s1Reply(t *testing.T, err error, code int) {
	t.Helper()
	var remote *Error
	if !errors.As(err, &remote) || remote.ReplyCode != code {
		t.Fatalf("expected actual FTP reply %d, got %v", code, err)
	}
	if strings.Contains(err.Error(), os.Getenv("INTERN_S1_PASSWORD")) {
		t.Fatal("public error leaked credential")
	}
}

func s1Quota(t *testing.T) {
	r, transport := s1Repository(t, "quota", "quota")
	key := "quota/payload"
	s := s1Create(t, r, key)
	_, writeErr := s.Write(bytes.Repeat([]byte("q"), 2<<20))
	_, commitErr := s.Commit(t.Context())
	s1Reply(t, errors.Join(commitErr, writeErr), 552)
	if transport.written.Load() == 0 {
		t.Fatal("quota failure occurred before a real data transfer")
	}
	s1Absent(t, "quota", key)
	s1Require(t, s.Abort())
	s1Require(t, s.Abort())
	s1Absent(t, "quota", s.TemporaryPath())
	t.Log("S1-A2 quota_552: actual quota exceeded after data transfer; part cleaned")
}

func s1Permission(t *testing.T) {
	r, _ := s1Repository(t, "permission", "normal")
	dir := s1Disk("normal", "permission")
	s1Require(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { s1Require(t, os.Chmod(dir, 0o700)) })
	_, err := r.Create(t.Context(), "permission/denied/payload")
	s1Reply(t, err, 550)
	if errors.Is(err, fs.ErrNotExist) {
		t.Fatal("permission refusal was misclassified as benign absence")
	}
	s1Absent(t, "normal", "permission/denied")
	t.Log("S1-A2 permission_550: actual chmod denied MKD; no formal object")
}

func s1TransportFailure(t *testing.T, mode string) {
	r, transport := s1Repository(t, "fault-"+mode, "normal")
	key := "fault-" + mode + "/payload"
	s := s1Create(t, r, key)
	transport.mode = mode
	_, writeErr := s.Write(s1Payload())
	_, commitErr := s.Commit(t.Context())
	if errors.Join(writeErr, commitErr) == nil || !transport.injected.Load() {
		t.Fatal("real data fault was not observed")
	}
	if mode == "short" && !errors.Is(errors.Join(writeErr, commitErr), io.ErrShortWrite) {
		t.Fatal("short write cause was not preserved")
	}
	s1Transferred(t, "normal", s.TemporaryPath())
	s1Absent(t, "normal", key)
	s1Require(t, s.Abort())
	s1Absent(t, "normal", s.TemporaryPath())
	t.Logf("S1-A2 %s: actual data bytes transferred; failure prevented publication; part cleaned", map[string]string{"interrupt": "interrupted_upload", "short": "short_write", "close": "data_close_error"}[mode])
}

func s1Cancel(t *testing.T) {
	r, transport := s1Repository(t, "cancel", "normal")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s, err := r.Create(ctx, "cancel/payload")
	s1Require(t, err)
	s1Write(t, s, s1Payload())
	s1Transferred(t, "normal", s.TemporaryPath())
	cancel()
	_, err = s.Commit(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatal("upload cancellation cause not preserved")
	}
	s1Require(t, s.Abort())
	s1Absent(t, "normal", "cancel/payload")
	s1Absent(t, "normal", s.TemporaryPath())
	s1Eventually(t, func() bool { return transport.active.Load() == 0 }, "canceled upload sockets drained")
	s1CancelBlockedWriter(t, r, transport)
	t.Log("S1-A2 cancel_upload: cancellation after remote bytes; bounded independent Abort cleaned part")
}

func s1CancelBlockedWriter(t *testing.T, r *Repository, transport *s1Transport) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s, err := r.Create(ctx, "cancel/blocked")
	s1Require(t, err)
	transport.mode = "block"
	transport.blocked = make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, writeErr := s.Write(s1Payload())
		done <- writeErr
	}()
	select {
	case <-transport.blocked:
	case <-time.After(6 * time.Second):
		t.Fatal("data transport never reached blocked writer after real bytes")
	}
	s1Transferred(t, "normal", s.TemporaryPath())
	cancel()
	select {
	case writeErr := <-done:
		if writeErr == nil {
			t.Fatal("canceled blocked pipe Write succeeded")
		}
	case <-time.After(6 * time.Second):
		t.Fatal("cancellation did not release blocked pipe writer")
	}
	_, err = s.Commit(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatal("blocked upload lost cancellation cause")
	}
	s1Require(t, s.Abort())
	s1Absent(t, "normal", "cancel/blocked")
	s1Absent(t, "normal", s.TemporaryPath())
}

func s1CleanupRetry(t *testing.T) {
	r, _ := s1Repository(t, "cleanup", "normal")
	s := s1Create(t, r, "cleanup/payload")
	s1Write(t, s, s1Payload())
	s1Transferred(t, "normal", s.TemporaryPath())
	if err := r.CleanupPart(t.Context(), s.TemporaryPath()); err == nil {
		t.Fatal("active part was accepted for external cleanup")
	}
	dir := filepath.Dir(s1Disk("normal", s.TemporaryPath()))
	s1Require(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { s1Require(t, os.Chmod(dir, 0o700)) })
	started := time.Now()
	err := s.Abort()
	var pending *CleanupError
	if !errors.As(err, &pending) || !errors.Is(err, ErrCleanupPending) || pending.Path != s.TemporaryPath() {
		t.Fatal("failed cleanup did not retain exact retry reference")
	}
	if time.Since(started) > 7*time.Second {
		t.Fatal("Abort cleanup was not bounded")
	}
	s1Transferred(t, "normal", pending.Path)
	s1Absent(t, "normal", "cleanup/payload")
	s1Require(t, os.Chmod(dir, 0o700))
	s1Require(t, r.CleanupPart(t.Context(), pending.Path))
	s1Require(t, r.CleanupPart(t.Context(), pending.Path))
	s1Absent(t, "normal", pending.Path)
	t.Log("S1-A2 cleanup_retry: actual denied DELETE yielded CleanupError; exact reference retry succeeded")
}

func s1Put(t *testing.T, r *Repository, key string, data []byte) {
	t.Helper()
	s := s1Create(t, r, key)
	s1Write(t, s, data)
	_, err := s.Commit(t.Context())
	s1Require(t, err)
}

func s1Isolation(t *testing.T) {
	a, _ := s1Repository(t, "isolation-a", "normal")
	b, _ := s1Repository(t, "isolation-b", "normal")
	ka, kb := "isolation-a/backup", "isolation-b/backup"
	s1Put(t, a, ka, []byte("a"))
	s1Put(t, b, kb, []byte("b"))
	for _, key := range []string{kb, "isolation-b", "isolation-a/.intern-instance"} {
		if a.Delete(t.Context(), key) == nil {
			t.Fatal("cross-instance or marker delete accepted")
		}
	}
	s1Require(t, os.Symlink(s1Disk("normal", "isolation-b"), s1Disk("normal", "isolation-a/link")))
	listed, err := a.List(t.Context(), "")
	s1Require(t, err)
	if !slices.Equal(listed, []string{ka}) {
		t.Fatal("listing exposed foreign namespace or symlink")
	}
	s1Require(t, a.Delete(t.Context(), ka))
	got, err := os.ReadFile(s1Disk("normal", kb))
	s1Require(t, err)
	if string(got) != "b" {
		t.Fatal("foreign backup changed")
	}
	t.Log("S1-A3 instance_isolation: two real namespaces; foreign backup and marker preserved; symlink excluded")
}

func s1Marker(t *testing.T) {
	root := s1Disk("normal", "unmarked")
	s1Require(t, os.Mkdir(root, 0o700))
	s1Require(t, os.WriteFile(filepath.Join(root, "backup"), []byte("existing"), 0o600))
	_, err := Open(t.Context(), s1Config("unmarked", "normal"))
	if !errors.Is(err, ErrOwnership) {
		t.Fatal("unmarked populated namespace adopted")
	}
	r, _ := s1Repository(t, "marker", "normal")
	s := s1Create(t, r, "marker/payload")
	s1Write(t, s, s1Payload())
	s1Transferred(t, "normal", s.TemporaryPath())
	marker := s1Disk("normal", "marker/.intern-instance")
	s1Require(t, os.WriteFile(marker, bytes.Repeat([]byte("foreign-instance"), 1<<18), 0o600))
	_, err = s.Commit(t.Context())
	if !errors.Is(err, ErrOwnership) {
		t.Fatal("commit failed to recheck owner marker")
	}
	_, err = Open(t.Context(), s1Config("marker", "normal"))
	if !errors.Is(err, ErrOwnership) {
		t.Fatal("foreign owner marker adopted")
	}
	s1Absent(t, "normal", "marker/payload")
	if r.Delete(t.Context(), "marker/payload") == nil {
		t.Fatal("operation accepted changed owner")
	}
	s1Require(t, os.WriteFile(marker, []byte("marker"), 0o600))
	s1Require(t, s.Abort())
	s1Require(t, r.CleanupPart(t.Context(), s.TemporaryPath()))
	got, err := os.ReadFile(filepath.Join(root, "backup"))
	s1Require(t, err)
	if string(got) != "existing" {
		t.Fatal("unmarked existing backup was changed")
	}
	t.Log("S1-A3 marker_conflict: unmarked and foreign ownership refused; commit rechecked; existing backup preserved")
}

func s1Paths(t *testing.T) {
	r, _ := s1Repository(t, "paths", "normal")
	for _, key := range []string{"../escape", "/paths/absolute", "paths/../escape", "paths/back\\slash", "other/payload", "paths/cr\r\n", "paths/.intern-instance"} {
		if _, err := r.Create(t.Context(), key); err == nil {
			t.Fatalf("unsafe key accepted: %q", key)
		}
		if err := r.Delete(t.Context(), key); err == nil {
			t.Fatalf("unsafe delete accepted: %q", key)
		}
	}
	for _, mode := range []string{"", "explicit", "implicit"} {
		cfg := s1Config("paths", "normal")
		cfg.TLSMode = mode
		if _, err := Open(t.Context(), cfg); err == nil {
			t.Fatal("unsupported TLS mode silently downgraded")
		}
	}
	if _, err := r.Open(t.Context(), "paths/payload", -1); !errors.Is(err, fs.ErrInvalid) {
		t.Fatal("negative RETR offset accepted")
	}
	t.Log("S1-A3 path_tls_rejection: traversal and foreign paths refused; plain explicitly selected; FTP credentials travel in cleartext")
}

func s1Resume(t *testing.T) {
	h, err := keyset.NewHandle(streamingaead.AES128GCMHKDF1MBKeyTemplate())
	s1Require(t, err)
	primitive, err := streamingaead.New(h)
	s1Require(t, err)
	var ciphertext bytes.Buffer
	aad := []byte("s1-immutable-ciphertext")
	writer, err := primitive.NewEncryptingWriter(&ciphertext, aad)
	s1Require(t, err)
	plaintext := bytes.Repeat(s1Payload(), 8)
	s1Write(t, writer, plaintext)
	s1Require(t, writer.Close())
	r, transport := s1Repository(t, "resume", "normal")
	key := "resume/ciphertext"
	s1Put(t, r, key, ciphertext.Bytes())
	ctx, cancel := context.WithCancel(t.Context())
	reader, err := r.Open(ctx, key, 0)
	s1Require(t, err)
	prefix := make([]byte, 123457, ciphertext.Len())
	_, err = io.ReadFull(reader, prefix)
	s1Require(t, err)
	cancel()
	_, readErr := io.Copy(io.Discard, reader)
	closeErr := reader.Close()
	if errors.Join(readErr, closeErr) == nil {
		t.Fatal("interrupted real RETR did not report cancellation")
	}
	s1Eventually(t, func() bool { return transport.active.Load() == 0 }, "interrupted reader sockets released")
	reader, err = r.Open(t.Context(), key, int64(len(prefix)))
	s1Require(t, err)
	rest, err := io.ReadAll(reader)
	s1Require(t, errors.Join(err, reader.Close()))
	joined := append(prefix, rest...)
	if sha256.Sum256(joined) != sha256.Sum256(ciphertext.Bytes()) {
		t.Fatal("RetrFrom resumed bytes differ from immutable ciphertext")
	}
	decrypted, err := primitive.NewDecryptingReader(bytes.NewReader(joined), aad)
	s1Require(t, err)
	got, err := io.ReadAll(decrypted)
	s1Require(t, err)
	if !bytes.Equal(got, plaintext) {
		t.Fatal("resumed ciphertext failed full Tink authentication")
	}
	t.Logf("S1-A4 ciphertext_resume: interrupted RETR + RetrFrom SHA256=%x; Tink authenticated to EOF; SIZE is not authentication", sha256.Sum256(joined))
}

func s1CloseDrain(t *testing.T) {
	baseline := runtime.NumGoroutine()
	r, transport := s1Repository(t, "drain", "normal")
	s1Put(t, r, "drain/read", s1Payload())
	reader, err := r.Open(t.Context(), "drain/read", 0)
	s1Require(t, err)
	s := s1Create(t, r, "drain/upload")
	s1Write(t, s, s1Payload())
	s1Transferred(t, "normal", s.TemporaryPath())
	transport.expectedClosePath = s.TemporaryPath()
	done := make(chan error, 1)
	go func() { done <- r.Close() }()
	select {
	case repositoryErr := <-done:
		if repositoryErr != nil {
			var pending *CleanupError
			if !errors.As(repositoryErr, &pending) || pending.Path != s.TemporaryPath() || !errors.Is(repositoryErr, ErrCleanupPending) {
				t.Fatal("Close lost active part cleanup responsibility")
			}
		}
	case <-time.After(7 * time.Second):
		t.Fatal("Repository.Close failed to drain active io.Pipe upload")
	}
	_, err = s.Write([]byte("after close"))
	if err == nil {
		t.Fatal("closed upload still writable")
	}
	_, err = io.Copy(io.Discard, reader)
	closeErr := reader.Close()
	if errors.Join(err, closeErr) == nil {
		t.Fatal("closed repository reader silently remained usable")
	}
	s1Eventually(t, func() bool { return transport.active.Load() == 0 }, "Close drained every owned socket")
	s1Eventually(t, func() bool { return runtime.NumGoroutine() <= baseline }, "Close drained transfer goroutines")
	s1Absent(t, "normal", "drain/upload")
	cleanup, _ := s1Repository(t, "drain", "normal")
	s1Require(t, cleanup.CleanupPart(t.Context(), s.TemporaryPath()))
	s1Absent(t, "normal", s.TemporaryPath())
	t.Log("S1-A5 close_drain: active upload pipe and reader sockets drained; goroutines returned; cleanup reference retried")
}

func s1RollingIdle(t *testing.T) {
	cfg := s1Config("rolling", "normal")
	r, err := Open(t.Context(), cfg)
	s1Require(t, err)
	defer func() { s1Require(t, r.Close()) }()
	s := s1Create(t, r, "rolling/payload")
	chunk := bytes.Repeat([]byte("r"), 32768)
	for range 20 {
		s1Write(t, s, chunk)
		time.Sleep(200 * time.Millisecond)
	}
	receipt, err := s.Commit(t.Context())
	s1Require(t, err)
	if receipt.Size != int64(20*len(chunk)) {
		t.Fatal("rolling timeout truncated progressing transfer")
	}
	t.Log("S1-A5 rolling_idle: progressing transfer exceeded idle interval and committed exact size")
}

type s1CompletionSink struct {
	*Sink
	cancel  context.CancelFunc
	aborted bool
}

func (s *s1CompletionSink) Commit(ctx context.Context) (pipeline.Receipt, error) {
	receipt, err := s.Sink.Commit(ctx)
	if err == nil {
		s.cancel()
	}
	return receipt, err
}

func (s *s1CompletionSink) Abort() error {
	s.aborted = true
	return s.Sink.Abort()
}

func s1Completion(t *testing.T) {
	r, _ := s1Repository(t, "completion", "normal")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s := &s1CompletionSink{Sink: s1Create(t, r, "completion/payload"), cancel: cancel}
	receipt, err := pipeline.Run(ctx, s1Source(func(_ context.Context, w io.Writer) error {
		_, err := w.Write(s1Payload())
		return err
	}), nil, s)
	s1Require(t, err)
	if s.aborted || receipt.Path != "completion/payload" || ctx.Err() == nil {
		t.Fatal("pipeline completion boundary violated by late cancellation")
	}
	got, err := os.ReadFile(s1Disk("normal", receipt.Path))
	s1Require(t, err)
	if !bytes.Equal(got, s1Payload()) {
		t.Fatal("late cancellation changed completed object")
	}
	t.Log("S1-A5 pipeline_completion: acknowledged real RNTO survives cancellation; no Abort")
}

func s1RenameAckLost(t *testing.T) {
	r, transport := s1Repository(t, "uncertain", "normal")
	key := "uncertain/payload"
	s := s1Create(t, r, key)
	s1Write(t, s, s1Payload())
	s1Transferred(t, "normal", s.TemporaryPath())
	transport.mode = "rename"
	transport.renameDisk = s1Disk("normal", key)
	receipt, err := s.Commit(t.Context())
	if receipt != (pipeline.Receipt{}) || !errors.Is(err, ErrCommitUncertain) || transport.written.Load() == 0 || !transport.injected.Load() {
		t.Fatal("lost RNTO acknowledgement must stay uncertain after a real server rename")
	}
	s1Absent(t, "normal", s.TemporaryPath())
	got, err := os.ReadFile(s1Disk("normal", key))
	s1Require(t, err)
	if !bytes.Equal(got, s1Payload()) {
		t.Fatal("uncertain publication lost the actual renamed object")
	}
	s1Require(t, s.Abort())
	preserved, err := os.ReadFile(s1Disk("normal", key))
	s1Require(t, err)
	if !bytes.Equal(preserved, s1Payload()) {
		t.Fatal("Abort rolled back a possible final object after lost RNTO acknowledgement")
	}
	size, err := r.Size(t.Context(), key)
	s1Require(t, err)
	if size != int64(len(s1Payload())) {
		t.Fatal("uncertain object was claimed absent")
	}
	t.Log("S1-A5 rename_ack_lost: dropped actual positive RNTO reply after server rename; ErrCommitUncertain; Abort preserved final bytes")
}

// Faults wrap this repository's real sockets only. Data faults never touch
// marker RETR. The rename case drops an already-sent control reply after the
// server filesystem rename; it never synthesizes FTP status lines.
type s1Transport struct {
	dial              func(context.Context, string, string) (net.Conn, error)
	mode              string
	expectedClosePath string
	renameDisk        string
	blocked           chan struct{}
	active            atomic.Int64
	written           atomic.Int64
	injected          atomic.Bool
}

func (s *s1Transport) connect(ctx context.Context, network, address string) (net.Conn, error) {
	conn, err := s.dial(ctx, network, address)
	if err != nil {
		return nil, err
	}
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errors.Join(err, conn.Close())
	}
	s.active.Add(1)
	return &s1Connection{Conn: conn, owner: s, data: port != "2121", closed: make(chan struct{})}, nil
}

type s1Connection struct {
	net.Conn
	owner    *s1Transport
	data     bool
	wrote    atomic.Bool
	rnto     atomic.Bool
	once     sync.Once
	closed   chan struct{}
	closeErr error
}

func (c *s1Connection) Read(p []byte) (int, error) {
	if c.data || c.owner.mode != "rename" || !c.rnto.Load() {
		return c.Conn.Read(p)
	}
	if c.owner.injected.Load() {
		return 0, io.ErrUnexpectedEOF
	}
	deadline := time.Now().Add(6 * time.Second)
	for {
		info, err := os.Stat(c.owner.renameDisk)
		if err == nil && info.Size() > 0 {
			break
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("server rename never became visible before the RNTO reply")
		}
		time.Sleep(10 * time.Millisecond)
	}
	n, err := c.Conn.Read(p)
	if n == 0 {
		return 0, err
	}
	c.owner.injected.Store(true)
	return 0, io.ErrUnexpectedEOF
}

func (c *s1Connection) Write(p []byte) (int, error) {
	if !c.data {
		return c.writeControl(p)
	}
	if c.owner.mode == "block" && c.wrote.Load() {
		close(c.owner.blocked)
		<-c.closed
		return 0, net.ErrClosed
	}
	if len(p) > 1 && (c.owner.mode == "short" || c.owner.mode == "interrupt") && c.owner.injected.CompareAndSwap(false, true) {
		n, err := c.Conn.Write(p[:len(p)/2])
		c.owner.written.Add(int64(n))
		c.wrote.Store(n > 0)
		if c.owner.mode == "interrupt" {
			return n, errors.Join(err, c.Close(), io.ErrUnexpectedEOF)
		}
		return n, err
	}
	n, err := c.Conn.Write(p)
	c.owner.written.Add(int64(n))
	if n > 0 {
		c.wrote.Store(true)
	}
	return n, err
}

func (c *s1Connection) writeControl(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if c.owner.mode == "rename" && bytes.Contains(p[:n], []byte("RNTO ")) {
		c.rnto.Store(true)
	}
	return n, err
}

func (c *s1Connection) Close() error {
	c.once.Do(func() {
		c.closeErr = c.Conn.Close()
		close(c.closed)
		c.owner.active.Add(-1)
		if c.data && c.wrote.Load() && c.owner.mode == "close" && c.owner.injected.CompareAndSwap(false, true) {
			c.closeErr = errors.Join(c.closeErr, fmt.Errorf("injected data Close failure after real write"))
		}
	})
	return c.closeErr
}
