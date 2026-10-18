package local

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/flyingEirc/Intern/internal/pipeline"
)

func newRepository(t *testing.T) (*Repository, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "repository")
	repo, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Error(err)
		}
	})
	return repo, root
}

func begin(t *testing.T, repo *Repository, dir string) *Staging {
	t.Helper()
	staging, err := repo.Begin(dir)
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

func create(t *testing.T, staging *Staging, name string) pipeline.Sink {
	t.Helper()
	sink, err := staging.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	return sink
}

func commitBytes(t *testing.T, staging *Staging, name, contents string) pipeline.Sink {
	t.Helper()
	sink := create(t, staging, name)
	if _, err := io.WriteString(sink, contents); err != nil {
		t.Fatal(err)
	}
	receipt, err := sink.Commit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := pipeline.Receipt{Path: path.Join(staging.dir, name), Size: int64(len(contents))}
	if receipt != want {
		t.Fatalf("receipt = %+v, want %+v", receipt, want)
	}
	return sink
}

func assertMode(t *testing.T, name string, mode fs.FileMode) {
	t.Helper()
	info, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != mode {
		t.Fatalf("%s mode = %o, want %o", name, info.Mode().Perm(), mode)
	}
}

func assertNoPending(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if isPendingName(entry.Name()) {
			t.Errorf("pending directory remains: %s", entry.Name())
		}
	}
}

func assertObject(t *testing.T, repo *Repository, name, contents string) {
	t.Helper()
	file, err := repo.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(file)
	if err = errors.Join(readErr, file.Close()); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte(contents)) {
		t.Fatalf("%s contents = %q, want %q", name, got, contents)
	}
	size, err := repo.Size(name)
	if err != nil || size != int64(len(contents)) {
		t.Fatalf("size = %d, error = %v", size, err)
	}
}

func TestOpenRootPermissions(t *testing.T) {
	for _, mode := range []fs.FileMode{0700, 0750, 0710, 0701, 0777, 0, os.ModeSticky | 0700} {
		t.Run(mode.String(), func(t *testing.T) {
			root := t.TempDir()
			if err := os.Chmod(root, mode); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(root, 0700) })
			repo, err := Open(root)
			if repo != nil {
				repo.Close()
			}
			if mode == 0700 && err != nil {
				t.Fatal(err)
			}
			if mode != 0700 && err == nil {
				t.Fatalf("accepted root mode %v", mode)
			}
		})
	}
}

func TestOpenRejectsSymlinkToPrivateDirectory(t *testing.T) {
	target := t.TempDir()
	if err := os.Chmod(target, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(target, 0700) })
	link := filepath.Join(t.TempDir(), "root")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	repo, err := Open(link)
	if repo != nil {
		repo.Close()
	}
	if err == nil {
		t.Fatal("opened a symlink root pointing at a 0700 directory")
	}
}

func TestPublishRejectsExistingWorldReadableParent(t *testing.T) {
	repo, root := newRepository(t)
	parent := filepath.Join(root, "parent")
	if err := os.Mkdir(parent, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0755); err != nil {
		t.Fatal(err)
	}
	staging := begin(t, repo, "parent/backup")
	commitBytes(t, staging, "payload", "private")
	if err := staging.Publish(context.Background()); err == nil {
		t.Fatal("published under a 0755 parent")
	}
	if err := staging.Abort(); err != nil {
		t.Fatal(err)
	}
	assertNoPending(t, root)
	if _, err := os.Stat(filepath.Join(parent, "backup")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("final directory was created under 0755 parent")
	}
}

func TestOpenRejectsRelativeSymlinkAndFile(t *testing.T) {
	parent := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(parent, "link")
	file := filepath.Join(parent, "file")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{"", "relative", link, file} {
		repo, err := Open(root)
		if repo != nil {
			repo.Close()
		}
		if err == nil {
			t.Errorf("accepted root %q", root)
		}
	}
}

func assertInvisible(t *testing.T, repo *Repository, name string) {
	t.Helper()
	if f, err := repo.Open(name); err == nil {
		f.Close()
		t.Fatal("unpublished object opened")
	}
	if _, err := repo.Size(name); err == nil {
		t.Fatal("unpublished size visible")
	}
	files, err := repo.List("")
	if err != nil || len(files) != 0 {
		t.Fatalf("unpublished files = %v, error = %v", files, err)
	}
}

func TestStagingPublishVisibilityAndPermissions(t *testing.T) {
	repo, root := newRepository(t)
	assertMode(t, root, 0700)
	staging := begin(t, repo, "inst/src/chain/0001-backup")
	assertMode(t, filepath.Join(root, staging.pending), 0700)
	name := staging.dir + "/payload.zst"
	assertInvisible(t, repo, name)
	sink := commitBytes(t, staging, "payload.zst", "private bytes")
	assertMode(t, filepath.Join(root, staging.pending, "payload.zst"), 0600)
	assertInvisible(t, repo, name)
	if err := staging.Publish(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertNoPending(t, root)
	assertObject(t, repo, name, "private bytes")
	assertPublishedModes(t, root, staging.dir)
	assertList(t, repo, "", []string{name})
	assertList(t, repo, "inst/src", []string{name})
	assertList(t, repo, name, []string{name})
	for range 2 {
		if err := errors.Join(sink.Abort(), staging.Abort()); err != nil {
			t.Fatal(err)
		}
	}
	assertObject(t, repo, name, "private bytes")
}

func assertPublishedModes(t *testing.T, root, dir string) {
	t.Helper()
	current := root
	for part := range strings.SplitSeq(dir, "/") {
		current = filepath.Join(current, part)
		assertMode(t, current, 0700)
	}
	assertMode(t, filepath.Join(current, "payload.zst"), 0600)
}

func assertList(t *testing.T, repo *Repository, prefix string, want []string) {
	t.Helper()
	files, err := repo.List(prefix)
	if err != nil || !reflect.DeepEqual(files, want) {
		t.Fatalf("List(%q) = %v, %v; want %v", prefix, files, err, want)
	}
}

func TestCreateExclusiveAndClosedSink(t *testing.T) {
	repo, _ := newRepository(t)
	staging := begin(t, repo, "backup")
	sink := commitBytes(t, staging, "payload", "first")
	if _, err := staging.Create("payload"); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("duplicate create error = %v", err)
	}
	if _, err := sink.Write([]byte("second")); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("write after commit error = %v", err)
	}
	if _, err := sink.Commit(context.Background()); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("second commit error = %v", err)
	}
	if err := staging.Publish(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := staging.Create("later"); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("create after publish error = %v", err)
	}
	assertObject(t, repo, "backup/payload", "first")
}

func TestDeletePublishedObjects(t *testing.T) {
	repo, _ := newRepository(t)
	staging := begin(t, repo, "backup")
	commitBytes(t, staging, "a", "a")
	commitBytes(t, staging, "b", "b")
	if err := staging.Publish(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertList(t, repo, "", []string{"backup/a", "backup/b"})
	if err := repo.Delete("backup/a"); err != nil {
		t.Fatal(err)
	}
	assertList(t, repo, "", []string{"backup/b"})
	if err := repo.Delete("backup"); err != nil {
		t.Fatal(err)
	}
	assertList(t, repo, "", []string{})
	if _, err := repo.Size("backup/b"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("deleted object size error = %v", err)
	}
}

func TestRepositoryStaysPinnedAfterRename(t *testing.T) {
	repo, root := newRepository(t)
	moved := root + "-moved"
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	staging := begin(t, repo, "backup")
	commitBytes(t, staging, "payload", "pinned")
	if err := staging.Publish(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertObject(t, repo, "backup/payload", "pinned")
	if _, err := os.Stat(filepath.Join(root, "backup")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("publication used unpinned root: %v", err)
	}
	assertMode(t, filepath.Join(moved, "backup"), 0700)
}
