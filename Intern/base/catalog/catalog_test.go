package catalog

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func requireStarted(t *testing.T, s *Store, r Record, want bool) {
	t.Helper()
	started, err := s.Begin(context.Background(), r)
	requireNoError(t, err)
	if started != want {
		t.Fatalf("request start=%t, want %t", started, want)
	}
}
func TestPublicationSurvivesReopenAndRejectsConcurrentOwner(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "state")
	s, err := Open(ctx, dir)
	requireNoError(t, err)
	r := Record{ID: "job", Source: "source", Kind: "logical-full", Path: "/owned/artifact", CreatedAt: time.Now().UTC()}
	requireStarted(t, s, r, true)
	if _, err = s.Get(ctx, r.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unpublished backup visible: %v", err)
	}
	other, openErr := Open(ctx, dir)
	if openErr == nil {
		other.Close()
		t.Fatal("concurrent catalog owner accepted")
	}
	requireNoError(t, s.Close())
	s, err = Open(ctx, dir)
	requireNoError(t, err)
	defer s.Close()
	checkInterruptedRequest(t, s, r)
	checkPublicationIdempotency(t, s, r)
	checkRestoreIsNotParent(t, s, r)
}
func checkInterruptedRequest(t *testing.T, s *Store, r Record) {
	t.Helper()
	ctx := context.Background()
	pending, err := s.Pending(ctx)
	requireNoError(t, err)
	if len(pending) != 1 || pending[0].ID != r.ID {
		t.Fatalf("crash cleanup responsibility lost: %v", pending)
	}
	if _, err = s.Begin(ctx, r); err == nil {
		t.Fatal("running request silently restarted")
	}
	requireNoError(t, s.Fail(ctx, r.ID))
	requireStarted(t, s, r, true)
}
func checkPublicationIdempotency(t *testing.T, s *Store, r Record) {
	t.Helper()
	ctx := context.Background()
	requireNoError(t, s.Complete(ctx, r.ID, []byte("authenticated manifest")))
	requireStarted(t, s, r, false)
	r.Path = "/another/request"
	if _, err := s.Begin(ctx, r); err == nil {
		t.Fatal("idempotency key reused for another artifact")
	}
	got, err := s.Get(ctx, r.ID)
	requireNoError(t, err)
	if string(got.Manifest) != "authenticated manifest" {
		t.Fatalf("published artifact unavailable: %v", got)
	}
}
func checkRestoreIsNotParent(t *testing.T, s *Store, r Record) {
	t.Helper()
	ctx := context.Background()
	restore := Record{ID: "restore-job", Source: r.Source, Kind: "restore", Path: "/owned/restore", CreatedAt: time.Now().UTC()}
	requireStarted(t, s, restore, true)
	requireNoError(t, s.Complete(ctx, restore.ID, []byte("restore plan")))
	latest, err := s.Latest(ctx, r.Source)
	requireNoError(t, err)
	if latest.ID != r.ID {
		t.Fatalf("restore ledger row displaced backup chain parent: %v", latest)
	}
}
