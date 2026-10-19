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

func TestRejectsUnsafeRepositoryPaths(t *testing.T) {
	repo, _ := newRepository(t)
	invalid := []string{
		"", ".", "..", "../outside", "a/../b", "/absolute", "a/./b",
		"a//b", "a/", ".pending-root", ".PENDING-x", ".Pending-x",
		"a/.pending-hidden/payload", "a/.PENDING-x/payload", "a\\b", "a\x00b",
	}
	for _, name := range invalid {
		t.Run(strings.ReplaceAll(name, "\x00", "NUL"), func(t *testing.T) {
			assertRejectedRepositoryPath(t, repo, name)
		})
	}
}

func assertRejectedRepositoryPath(t *testing.T, repo *Repository, name string) {
	t.Helper()
	if staging, err := repo.Begin(name); err == nil {
		staging.Abort()
		t.Errorf("Begin accepted %q", name)
	}
	if file, err := repo.Open(name); err == nil {
		file.Close()
		t.Errorf("Open accepted %q", name)
	}
	if _, err := repo.Size(name); err == nil {
		t.Errorf("Size accepted %q", name)
	}
	if name != "" {
		if _, err := repo.List(name); err == nil {
			t.Errorf("List accepted %q", name)
		}
	}
	if err := repo.Delete(name); err == nil {
		t.Errorf("Delete accepted %q", name)
	}
}

func TestRejectsUnsafeObjectNames(t *testing.T) {
	repo, _ := newRepository(t)
	staging := begin(t, repo, "backup")
	for _, name := range []string{"", ".", "..", "a/b", "/absolute", "a\\b", ".pending-file", ".PENDING-x", ".Pending-x", "nul\x00"} {
		if _, err := staging.Create(name); !errors.Is(err, fs.ErrInvalid) {
			t.Errorf("Create(%q) error = %v", name, err)
		}
	}
}

func TestSymlinksCannotEscapeOrExposePending(t *testing.T) {
	repo, root := newRepository(t)
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	assertRejectedRepositoryPath(t, repo, "escape")
	assertRejectedRepositoryPath(t, repo, "escape/secret")
	if staging, err := repo.Begin("escape/new"); err == nil {
		staging.Abort()
		t.Fatal("Begin accepted outside parent symlink")
	}
	assertOutsideUnchanged(t, outside, secret)
	assertPendingSymlinkHidden(t, repo, root)
	assertList(t, repo, "", []string{})
}

func assertOutsideUnchanged(t *testing.T, outside, secret string) {
	t.Helper()
	contents, err := os.ReadFile(secret)
	if err != nil || string(contents) != "unchanged" {
		t.Fatalf("outside data changed: %q, %v", contents, err)
	}
	if _, err = os.Stat(filepath.Join(outside, "new")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("outside directory was created: %v", err)
	}
}

func assertPendingSymlinkHidden(t *testing.T, repo *Repository, root string) {
	t.Helper()
	staging := begin(t, repo, "backup")
	commitBytes(t, staging, "payload", "private")
	if err := os.Symlink(staging.pending, filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	assertRejectedRepositoryPath(t, repo, "alias/payload")
	assertRejectedRepositoryPath(t, repo, staging.pending+"/payload")
	if err := repo.Delete(staging.pending); !errors.Is(err, fs.ErrInvalid) {
		t.Fatalf("pending Delete error = %v", err)
	}
}

func TestPublishRechecksSymlinkParents(t *testing.T) {
	repo, root := newRepository(t)
	outside := t.TempDir()
	staging := begin(t, repo, "parent/backup")
	commitBytes(t, staging, "payload", "private")
	if err := os.Symlink(outside, filepath.Join(root, "parent")); err != nil {
		t.Fatal(err)
	}
	if err := staging.Publish(context.Background()); err == nil {
		t.Fatal("Publish followed a newly introduced parent symlink")
	}
	if err := staging.Abort(); err != nil {
		t.Fatal(err)
	}
	assertNoPending(t, root)
	if _, err := os.Stat(filepath.Join(outside, "backup")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("outside publication appeared: %v", err)
	}
}

func TestListOmitsNestedPendingAndSymlinks(t *testing.T) {
	repo, root := newRepository(t)
	staging := begin(t, repo, "backup")
	commitBytes(t, staging, "payload", "published")
	if err := staging.Publish(context.Background()); err != nil {
		t.Fatal(err)
	}
	pending := filepath.Join(root, "backup", ".pending-hidden")
	if err := os.Mkdir(pending, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pending, "hidden"), []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("payload", filepath.Join(root, "backup", "alias")); err != nil {
		t.Fatal(err)
	}
	assertList(t, repo, "backup", []string{"backup/payload"})
}

func TestListSkipsPendingCaseVariants(t *testing.T) {
	repo, root := newRepository(t)
	staging := begin(t, repo, "backup")
	commitBytes(t, staging, "payload", "published")
	if err := staging.Publish(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".PENDING-x", ".Pending-x"} {
		dir := filepath.Join(root, "backup", name)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "hidden"), []byte("private"), 0o600); err != nil {
			t.Fatal(err)
		}
		assertList(t, repo, "backup", []string{"backup/payload"})
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
	}
}
