package local

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func beginOwned(t *testing.T, repo *Repository, dir, token string) *Staging {
	t.Helper()
	staging, err := repo.BeginOwned(dir, token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := staging.Abort(); err != nil {
			t.Error(err)
		}
	})
	return staging
}

func TestBeginOwnedTokenBoundaries(t *testing.T) {
	for _, token := range []string{"a", "0", "-", "abcdefghijklmnopqrstuvwxyz-0123456789", strings.Repeat("a", 64)} {
		t.Run(token, func(t *testing.T) {
			repo, root := newRepository(t)
			staging := beginOwned(t, repo, "instance/source/backup", token)
			assertMode(t, filepath.Join(root, ".pending-"+token), 0o700)
			commitBytes(t, staging, "payload", "owned bytes")
			assertInvisible(t, repo, "instance/source/backup/payload")
			if err := staging.Publish(context.Background()); err != nil {
				t.Fatal(err)
			}
			assertNoPending(t, root)
			assertObject(t, repo, "instance/source/backup/payload", "owned bytes")
			if err := repo.ReclaimPending(token); err != nil {
				t.Fatal(err)
			}
			assertObject(t, repo, "instance/source/backup/payload", "owned bytes")
		})
	}
}

func TestOwnedPendingRejectsInvalidTokensBeforeRootAccess(t *testing.T) {
	// An uninitialized root makes any filesystem access a test failure, rather
	// than letting a filesystem error masquerade as token validation.
	repo := &Repository{}
	for _, token := range []string{
		"", strings.Repeat("a", 65), "A", "abcD", ".", "..", "../published",
		"a/../../published", "/absolute", `a\b`, "a_b", "a.b", "a b", "a\n", "a\x00", "é",
	} {
		t.Run(token, func(t *testing.T) {
			if staging, err := repo.BeginOwned("backup", token); !errors.Is(err, fs.ErrInvalid) || staging != nil {
				t.Fatalf("BeginOwned returned %v, %v; want nil, ErrInvalid", staging, err)
			}
			if err := repo.ReclaimPending(token); !errors.Is(err, fs.ErrInvalid) {
				t.Fatalf("ReclaimPending error = %v; want ErrInvalid", err)
			}
		})
	}
}

func TestBeginOwnedRefusesExistingPending(t *testing.T) {
	for _, kind := range []string{"directory", "file", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			assertExistingPendingRefused(t, kind)
		})
	}
}

func assertExistingPendingRefused(t *testing.T, kind string) {
	t.Helper()
	repo, root := newRepository(t)
	pending := filepath.Join(root, ".pending-owned")
	makeExistingPending(t, repo, pending, kind)
	before, err := os.Lstat(pending)
	if err != nil {
		t.Fatal(err)
	}
	if staging, beginErr := repo.BeginOwned("second", "owned"); !errors.Is(beginErr, fs.ErrExist) || staging != nil {
		t.Fatalf("duplicate token returned %v, %v; want nil, ErrExist", staging, beginErr)
	}
	after, err := os.Lstat(pending)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("existing pending entry replaced: %v", err)
	}
	if kind == "directory" {
		assertFileBytes(t, filepath.Join(pending, "payload"), "first")
	}
}

func makeExistingPending(t *testing.T, repo *Repository, pending, kind string) {
	t.Helper()
	switch kind {
	case "directory":
		staging := beginOwned(t, repo, "first", "owned")
		commitBytes(t, staging, "payload", "first")
	case "file":
		if err := os.WriteFile(pending, []byte("first"), 0o600); err != nil {
			t.Fatal(err)
		}
	case "symlink":
		if err := os.Symlink("missing", pending); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBeginOwnedRefusesExistingTarget(t *testing.T) {
	repo, root := newRepository(t)
	published := beginOwned(t, repo, "backup", "published")
	commitBytes(t, published, "payload", "published")
	if err := published.Publish(context.Background()); err != nil {
		t.Fatal(err)
	}
	if staging, err := repo.BeginOwned("backup", "replacement"); !errors.Is(err, fs.ErrExist) || staging != nil {
		t.Fatalf("existing target returned %v, %v; want nil, ErrExist", staging, err)
	}
	assertNoPending(t, root)
	assertObject(t, repo, "backup/payload", "published")
}

func TestReclaimPendingAfterReopenIsScopedAndIdempotent(t *testing.T) {
	repo, root := newRepository(t)
	orphan, err := repo.BeginOwned("orphan", "owned")
	if err != nil {
		t.Fatal(err)
	}
	commitBytes(t, orphan, "payload", "orphan bytes")
	if closeErr := repo.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	reopened, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	})
	other := beginOwned(t, reopened, "other", "owned-other")
	commitBytes(t, other, "payload", "other bytes")
	published := beginOwned(t, reopened, "owned", "published")
	commitBytes(t, published, "payload", "published bytes")
	if err := published.Publish(context.Background()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := reopened.ReclaimPending("owned"); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(filepath.Join(root, ".pending-owned")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("orphan pending remains: %v", err)
		}
		assertFileBytes(t, filepath.Join(root, ".pending-owned-other", "payload"), "other bytes")
		assertObject(t, reopened, "owned/payload", "published bytes")
	}
	if err := other.Publish(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertObject(t, reopened, "other/payload", "other bytes")
}

func TestReclaimPendingRejectsSymlink(t *testing.T) {
	for _, target := range []string{"published", "outside", "missing"} {
		t.Run(target, func(t *testing.T) {
			assertPendingSymlinkRefused(t, target)
		})
	}
}

func assertPendingSymlinkRefused(t *testing.T, target string) {
	t.Helper()
	repo, root := newRepository(t)
	published := beginOwned(t, repo, "published", "published")
	commitBytes(t, published, "payload", "published bytes")
	if err := published.Publish(context.Background()); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "payload")
	if err := os.WriteFile(outsideFile, []byte("outside bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkTarget := target
	if target == "outside" {
		linkTarget = outside
	}
	pending := filepath.Join(root, ".pending-owned")
	if err := os.Symlink(linkTarget, pending); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReclaimPending("owned"); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("symlink reclaim error = %v; want ErrPermission", err)
	}
	if got, err := os.Readlink(pending); err != nil || got != linkTarget {
		t.Fatalf("pending symlink changed: %q, %v", got, err)
	}
	assertObject(t, repo, "published/payload", "published bytes")
	assertFileBytes(t, outsideFile, "outside bytes")
}

func TestReclaimPendingDoesNotFollowNestedSymlinks(t *testing.T) {
	repo, root := newRepository(t)
	beginOwned(t, repo, "backup", "owned")
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "payload")
	if err := os.WriteFile(outsideFile, []byte("outside bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, ".pending-owned", "link")); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReclaimPending("owned"); err != nil {
		t.Fatal(err)
	}
	assertNoPending(t, root)
	assertFileBytes(t, outsideFile, "outside bytes")
}

func TestOwnedPendingUsesPinnedRoot(t *testing.T) {
	repo, root := newRepository(t)
	moved := root + "-moved"
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".pending-owned"), 0o700); err != nil {
		t.Fatal(err)
	}
	replacementFile := filepath.Join(root, ".pending-owned", "payload")
	if err := os.WriteFile(replacementFile, []byte("replacement root"), 0o600); err != nil {
		t.Fatal(err)
	}
	staging := beginOwned(t, repo, "backup", "owned")
	commitBytes(t, staging, "payload", "pinned root")
	assertFileBytes(t, filepath.Join(moved, ".pending-owned", "payload"), "pinned root")
	if err := repo.ReclaimPending("owned"); err != nil {
		t.Fatal(err)
	}
	assertNoPending(t, moved)
	assertFileBytes(t, replacementFile, "replacement root")
}

func TestOwnedPendingRejectsClosedRepository(t *testing.T) {
	repo, _ := newRepository(t)
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	if staging, err := repo.BeginOwned("backup", "owned"); !errors.Is(err, fs.ErrClosed) || staging != nil {
		t.Fatalf("closed BeginOwned returned %v, %v", staging, err)
	}
	if err := repo.ReclaimPending("owned"); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("closed ReclaimPending error = %v", err)
	}
}

func assertFileBytes(t *testing.T, name, want string) {
	t.Helper()
	got, err := os.ReadFile(name)
	if err != nil || string(got) != want {
		t.Fatalf("%s contents = %q, %v; want %q", name, got, err, want)
	}
}
