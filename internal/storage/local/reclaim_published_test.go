package local

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestReclaimPublishedAbsentIsIdempotent(t *testing.T) {
	for _, dir := range []string{"absent", "missing/parent/absent", "existing/absent"} {
		t.Run(dir, func(t *testing.T) {
			repo, root := newRepository(t)
			if err := os.Mkdir(filepath.Join(root, "existing"), 0o700); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := repo.ReclaimPublished(dir); err != nil {
					t.Fatal(err)
				}
			}
			assertList(t, repo, "", []string{})
		})
	}
}

func TestReclaimPublishedRemovesOnlyNestedDirectory(t *testing.T) {
	repo, root := newRepository(t)
	for _, dir := range []string{"instance/chain/backup", "instance/chain/other"} {
		staging := begin(t, repo, dir)
		commitBytes(t, staging, "payload", dir)
		if err := staging.Publish(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	pending := beginOwned(t, repo, "future", "active")
	commitBytes(t, pending, "payload", "active bytes")
	for range 2 {
		if err := repo.ReclaimPublished("instance/chain/backup"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Lstat(filepath.Join(root, "instance/chain/backup")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("reclaimed directory remains: %v", err)
	}
	assertObject(t, repo, "instance/chain/other/payload", "instance/chain/other")
	assertFileBytes(t, filepath.Join(root, ".pending-active", "payload"), "active bytes")
}

func TestReclaimPublishedRefusesSymlinkComponents(t *testing.T) {
	for _, dir := range []string{"link", "link/child", "link/absent"} {
		t.Run(dir, func(t *testing.T) {
			assertPublishedSymlinkRefused(t, dir)
		})
	}
}

func assertPublishedSymlinkRefused(t *testing.T, dir string) {
	t.Helper()
	repo, root := newRepository(t)
	staging := begin(t, repo, "protected/child")
	commitBytes(t, staging, "payload", "protected bytes")
	if err := staging.Publish(context.Background()); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink("protected", link); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReclaimPublished(dir); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("symlink path error = %v; want ErrPermission", err)
	}
	if target, err := os.Readlink(link); err != nil || target != "protected" {
		t.Fatalf("symlink changed: %q, %v", target, err)
	}
	assertObject(t, repo, "protected/child/payload", "protected bytes")
}

func TestReclaimPublishedRefusesPendingAndInvalidPaths(t *testing.T) {
	repo, root := newRepository(t)
	pending := beginOwned(t, repo, "backup", "active")
	commitBytes(t, pending, "payload", "active bytes")
	for _, dir := range []string{
		".pending-active", ".PENDING-active", "parent/.pending-active", "parent/.PENDING-active",
		"", ".", "..", "/absolute", "a/../b", "a/", `a\b`,
	} {
		t.Run(dir, func(t *testing.T) {
			if err := repo.ReclaimPublished(dir); !errors.Is(err, fs.ErrInvalid) {
				t.Fatalf("invalid path error = %v; want ErrInvalid", err)
			}
		})
	}
	assertFileBytes(t, filepath.Join(root, ".pending-active", "payload"), "active bytes")
}

func TestReclaimPublishedRefusesRegularFile(t *testing.T) {
	repo, _ := newRepository(t)
	staging := begin(t, repo, "backup")
	commitBytes(t, staging, "payload", "published bytes")
	if err := staging.Publish(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReclaimPublished("backup/payload"); !errors.Is(err, fs.ErrInvalid) {
		t.Fatalf("file reclaim error = %v; want ErrInvalid", err)
	}
	assertObject(t, repo, "backup/payload", "published bytes")
}

func TestReclaimPublishedRejectsClosedRepository(t *testing.T) {
	repo, _ := newRepository(t)
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReclaimPublished("backup"); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("closed reclaim error = %v; want ErrClosed", err)
	}
}
