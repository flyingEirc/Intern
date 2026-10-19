package local

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestAbortAllStates(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, *Staging)
	}{
		{"empty staging", func(*testing.T, *Staging) {}},
		{"unwritten file", func(t *testing.T, s *Staging) { create(t, s, "payload") }},
		{"partial file", func(t *testing.T, s *Staging) {
			sink := create(t, s, "payload")
			if _, err := io.WriteString(sink, "partial"); err != nil {
				t.Fatal(err)
			}
		}},
		{"committed file", func(t *testing.T, s *Staging) {
			commitBytes(t, s, "payload", "committed")
		}},
		{"canceled commit", canceledCommit},
		{"failed file sync", failedSyncCommit},
		{"aborted sink", func(t *testing.T, s *Staging) {
			sink := commitBytes(t, s, "payload", "committed")
			for range 2 {
				if err := sink.Abort(); err != nil {
					t.Fatal(err)
				}
			}
		}},
		{"canceled publish", func(t *testing.T, s *Staging) {
			commitBytes(t, s, "payload", "committed")
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := s.Publish(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled publish error = %v", err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertAbortState(t, tt.setup)
		})
	}
}

func assertAbortState(t *testing.T, setup func(*testing.T, *Staging)) {
	t.Helper()
	repo, root := newRepository(t)
	staging := begin(t, repo, "backup")
	setup(t, staging)
	for range 2 {
		if err := staging.Abort(); err != nil {
			t.Fatal(err)
		}
	}
	assertNoPending(t, root)
	assertInvisible(t, repo, "backup/payload")
	assertAbortedHandles(t, staging)
}

func canceledCommit(t *testing.T, staging *Staging) {
	t.Helper()
	sink := create(t, staging, "payload")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sink.Commit(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("commit error = %v", err)
	}
}

func failedSyncCommit(t *testing.T, staging *Staging) {
	t.Helper()
	sink := create(t, staging, "payload")
	file, ok := sink.(*fileSink)
	if !ok {
		t.Fatal("unexpected local sink type")
	}
	if err := file.file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := sink.Commit(context.Background()); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("commit closed file error = %v", err)
	}
}

func assertAbortedHandles(t *testing.T, staging *Staging) {
	t.Helper()
	if _, err := staging.Create("later"); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("create after abort error = %v", err)
	}
	if err := staging.Publish(context.Background()); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("publish after abort error = %v", err)
	}
	for _, sink := range staging.sinks {
		if err := sink.Abort(); err != nil {
			t.Fatal(err)
		}
		if _, err := sink.Write([]byte("closed")); !errors.Is(err, fs.ErrClosed) {
			t.Fatalf("write after abort error = %v", err)
		}
	}
}

func TestSinkAbortRemovesFile(t *testing.T) {
	repo, root := newRepository(t)
	staging := begin(t, repo, "backup")
	sink := create(t, staging, "payload")
	if _, err := io.WriteString(sink, "partial"); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(root, staging.pending, "payload")
	if _, err := os.Stat(name); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := sink.Abort(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(name); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("aborted file remains: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, staging.pending)); err != nil {
		t.Fatal(err)
	}
}

type errWhenPendingMoved struct {
	context.Context
	root    string
	pending string
}

func (c errWhenPendingMoved) Err() error {
	if _, err := os.Lstat(filepath.Join(c.root, c.pending)); errors.Is(err, fs.ErrNotExist) {
		return context.Canceled
	}
	return c.Context.Err()
}

func TestPublishKeepsDirectoryWhenCanceledAfterRename(t *testing.T) {
	repo, root := newRepository(t)
	staging := begin(t, repo, "backup")
	commitBytes(t, staging, "payload", "durable")
	ctx := errWhenPendingMoved{
		Context: context.Background(),
		root:    root,
		pending: staging.pending,
	}
	if err := staging.Publish(ctx); err != nil {
		t.Fatalf("cancellation after rename = %v", err)
	}
	if staging.state != stagingPublished {
		t.Fatal("durable publish was not recorded")
	}
	if _, err := os.Stat(filepath.Join(root, staging.pending)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("pending directory still present: %v", err)
	}
	assertObject(t, repo, "backup/payload", "durable")
}

func TestPublishRequiresAllCommittedSinks(t *testing.T) {
	repo, root := newRepository(t)
	staging := begin(t, repo, "backup")
	commitBytes(t, staging, "first", "committed")
	sink := create(t, staging, "second")
	if err := staging.Publish(context.Background()); err == nil {
		t.Fatal("published an uncommitted sink")
	}
	if err := sink.Abort(); err != nil {
		t.Fatal(err)
	}
	if err := staging.Publish(context.Background()); err == nil {
		t.Fatal("published an aborted sink")
	}
	if err := staging.Abort(); err != nil {
		t.Fatal(err)
	}
	assertNoPending(t, root)
	assertInvisible(t, repo, "backup/first")
}

func TestPublishDoesNotReplaceExistingTargets(t *testing.T) {
	for _, target := range []string{"empty directory", "populated directory", "file"} {
		t.Run(target, func(t *testing.T) {
			repo, root := newRepository(t)
			staging := begin(t, repo, "backup")
			commitBytes(t, staging, "payload", "replacement")
			makeExistingTarget(t, root, target)
			if err := staging.Publish(context.Background()); !errors.Is(err, fs.ErrExist) {
				t.Fatalf("target created after Begin: publish error = %v", err)
			}
			if staging.state == stagingPublished {
				t.Fatal("rejected rename was recorded as published")
			}
			if err := staging.Abort(); err != nil {
				t.Fatal(err)
			}
			assertExistingTarget(t, repo, root, target)
			assertNoPending(t, root)
		})
	}
}

func makeExistingTarget(t *testing.T, root, kind string) {
	t.Helper()
	name := filepath.Join(root, "backup")
	if kind == "file" {
		if err := os.WriteFile(name, []byte("original"), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := os.Mkdir(name, 0o700); err != nil {
		t.Fatal(err)
	}
	if kind == "populated directory" {
		if err := os.WriteFile(filepath.Join(name, "payload"), []byte("original"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func assertExistingTarget(t *testing.T, repo *Repository, root, kind string) {
	t.Helper()
	switch kind {
	case "file":
		assertObject(t, repo, "backup", "original")
	case "populated directory":
		assertObject(t, repo, "backup/payload", "original")
	default:
		entries, err := os.ReadDir(filepath.Join(root, "backup"))
		if err != nil || len(entries) != 0 {
			t.Fatalf("empty target changed: %v, %v", entries, err)
		}
	}
}

func TestBeginRejectsExistingTarget(t *testing.T) {
	for _, target := range []string{"empty directory", "populated directory", "file"} {
		t.Run(target, func(t *testing.T) {
			repo, root := newRepository(t)
			makeExistingTarget(t, root, target)
			staging, err := repo.Begin("backup")
			if staging != nil {
				t.Cleanup(func() {
					if abortErr := staging.Abort(); abortErr != nil {
						t.Error(abortErr)
					}
				})
			}
			if !errors.Is(err, fs.ErrExist) {
				t.Fatalf("Begin error = %v", err)
			}
			assertNoPending(t, root)
			assertExistingTarget(t, repo, root, target)
		})
	}
}

func TestBeginRejectsNestedExistingFile(t *testing.T) {
	repo, root := newRepository(t)
	parent := filepath.Join(root, "parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "backup"), []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	staging, err := repo.Begin("parent/backup")
	if staging != nil {
		if abortErr := staging.Abort(); abortErr != nil {
			t.Fatal(abortErr)
		}
	}
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("Begin error = %v", err)
	}
	assertNoPending(t, root)
	assertFileContains(t, filepath.Join(parent, "backup"), "original")
}

func TestBeginAllowsAbsentTarget(t *testing.T) {
	repo, root := newRepository(t)
	if err := os.Mkdir(filepath.Join(root, "parent"), 0o700); err != nil {
		t.Fatal(err)
	}
	staging := begin(t, repo, "parent/leaf")
	if _, err := os.Stat(filepath.Join(root, staging.pending)); err != nil {
		t.Fatalf("pending directory was not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "parent", "leaf")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("Begin created the final path")
	}
}

func TestSyncRenamedReturnsDirectorySyncError(t *testing.T) {
	t.Run("parent", func(t *testing.T) { assertSyncRenamedParentClosed(t) })
	t.Run("root", func(t *testing.T) { assertSyncRenamedRootClosed(t) })
}

func assertSyncRenamedParentClosed(t *testing.T) {
	t.Helper()
	parent, rootDir := openTempDirPair(t)
	t.Cleanup(func() { rootDir.Close() })
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	err := syncRenamed("backup", rootDir, parent)
	if !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("sync error = %v", err)
	}
}

func assertSyncRenamedRootClosed(t *testing.T) {
	t.Helper()
	parent, rootDir := openTempDirPair(t)
	t.Cleanup(func() { parent.Close() })
	if err := rootDir.Close(); err != nil {
		t.Fatal(err)
	}
	err := syncRenamed("nested/backup", rootDir, parent)
	if !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("sync error = %v", err)
	}
}

func openTempDirPair(t *testing.T) (parent, rootDir *os.File) {
	t.Helper()
	parent, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rootDir, err = os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return parent, rootDir
}

func TestRestorePendingAfterDirectorySyncFailure(t *testing.T) {
	repo, root := newRepository(t)
	staging := begin(t, repo, "backup")
	commitBytes(t, staging, "payload", "rolled-back")
	rootDir, parent := openRootPair(t, repo)
	if err := renameExclusive(rootDir, staging.pending, parent, "backup"); err != nil {
		t.Fatal(err)
	}
	cause := closedDirectorySyncError(t)
	published, err := staging.restorePending(parent, rootDir, cause)
	if published {
		t.Fatal("directory sync failure stayed published")
	}
	if !errors.Is(err, cause) {
		t.Fatalf("restore error = %v, want %v", err, cause)
	}
	if _, statErr := os.Stat(filepath.Join(root, "backup")); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatal("final directory remained after rollback")
	}
	payload, readErr := os.ReadFile(filepath.Join(root, staging.pending, "payload"))
	if readErr != nil || string(payload) != "rolled-back" {
		t.Fatalf("restored payload = %q, %v", payload, readErr)
	}
}

func TestRestoreRenameFailureLeavesPublishedDirectory(t *testing.T) {
	repo, root := newRepository(t)
	staging := begin(t, repo, "backup")
	commitBytes(t, staging, "payload", "keep")
	rootDir, parent := openRootPair(t, repo)
	if err := renameExclusive(rootDir, staging.pending, parent, "backup"); err != nil {
		t.Fatal(err)
	}
	if err := repo.root.Mkdir(staging.pending, 0o700); err != nil {
		t.Fatal(err)
	}
	cause := closedDirectorySyncError(t)
	published, err := staging.restorePending(parent, rootDir, cause)
	if !published || !errors.Is(err, cause) || !errors.Is(err, fs.ErrExist) {
		t.Fatalf("published=%v restore error=%v", published, err)
	}
	assertFileContains(t, filepath.Join(root, "backup", "payload"), "keep")
	if err = staging.Abort(); err != nil {
		t.Fatal(err)
	}
	assertFileContains(t, filepath.Join(root, "backup", "payload"), "keep")
}

func assertFileContains(t *testing.T, name, want string) {
	t.Helper()
	got, err := os.ReadFile(name)
	if err != nil || string(got) != want {
		t.Fatalf("%s = %q, %v", name, got, err)
	}
}

func TestCloseWaitsForRootPin(t *testing.T) {
	repo, _ := newRepository(t)
	if err := repo.pin(); err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			repo.unpin()
		}
	}
	defer release()
	done := make(chan error, 1)
	go func() { done <- repo.Close() }()
	select {
	case err := <-done:
		t.Fatalf("Close finished while a root pin was held: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	release()
	callWithin(t, 2*time.Second, func() error { return <-done })
}

func openRootPair(t *testing.T, repo *Repository) (rootDir, parent *os.File) {
	t.Helper()
	var err error
	rootDir, err = repo.root.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rootDir.Close() })
	parent, err = repo.root.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { parent.Close() })
	return rootDir, parent
}

func closedDirectorySyncError(t *testing.T) error {
	t.Helper()
	dir, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = dir.Close(); err != nil {
		t.Fatal(err)
	}
	err = dir.Sync()
	if !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("closed directory sync = %v", err)
	}
	return err
}

func TestConcurrentPublishDoesNotReplace(t *testing.T) {
	repo, root := newRepository(t)
	other, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := other.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	stages := []*Staging{begin(t, repo, "backup"), begin(t, other, "backup")}
	for i, staging := range stages {
		commitBytes(t, staging, "payload", []string{"first", "second"}[i])
	}
	results := make(chan error, 2)
	start := make(chan struct{})
	for _, staging := range stages {
		go func() {
			<-start
			results <- staging.Publish(context.Background())
		}()
	}
	close(start)
	assertCompetingResults(t, <-results, <-results)
	for _, staging := range stages {
		if err = staging.Abort(); err != nil {
			t.Fatal(err)
		}
	}
	assertNoPending(t, root)
	assertConcurrentWinner(t, repo)
}

func assertCompetingResults(t *testing.T, first, second error) {
	t.Helper()
	if first == nil && errors.Is(second, fs.ErrExist) {
		return
	}
	if second == nil && errors.Is(first, fs.ErrExist) {
		return
	}
	t.Fatalf("competing publication errors = %v, %v", first, second)
}

func assertConcurrentWinner(t *testing.T, repo *Repository) {
	t.Helper()
	f, err := repo.Open("backup/payload")
	if err != nil {
		t.Fatal(err)
	}
	contents, readErr := io.ReadAll(f)
	if err = errors.Join(readErr, f.Close()); err != nil {
		t.Fatal(err)
	}
	if string(contents) != "first" && string(contents) != "second" {
		t.Fatalf("winner contents = %q", contents)
	}
}

func TestStagingLockDoesNotBlockRepositoryReaders(t *testing.T) {
	repo, _ := newRepository(t)
	published := begin(t, repo, "backup")
	commitBytes(t, published, "payload", "visible")
	if err := published.Publish(context.Background()); err != nil {
		t.Fatal(err)
	}
	staging := begin(t, repo, "incoming")
	sink := create(t, staging, "partial")
	if _, err := io.WriteString(sink, "partial"); err != nil {
		t.Fatal(err)
	}

	staging.mu.Lock()
	unlocked := false
	unlock := func() {
		if !unlocked {
			unlocked = true
			staging.mu.Unlock()
		}
	}
	defer unlock()

	abortDone := make(chan error, 1)
	go func() { abortDone <- sink.Abort() }()
	writeDone := make(chan error, 1)
	go func() {
		_, err := sink.Write([]byte("more"))
		writeDone <- err
	}()
	assertMutexFree(t, &repo.mu, 200*time.Millisecond)
	callWithin(t, 2*time.Second, func() error { return readPublished(repo, "backup/payload", "visible") })

	unlock()
	callWithin(t, 2*time.Second, func() error {
		abortErr := <-abortDone
		writeErr := <-writeDone
		if abortErr != nil {
			return abortErr
		}
		if writeErr != nil && !errors.Is(writeErr, fs.ErrClosed) {
			return writeErr
		}
		return nil
	})
}

func TestDataPathIgnoresRepositoryLock(t *testing.T) {
	repo, _ := newRepository(t)
	staging := begin(t, repo, "backup")
	sink := create(t, staging, "payload")
	repo.mu.Lock()
	defer repo.mu.Unlock()
	assertCallFinishes(t, "Write", 2*time.Second, func() error {
		_, err := sink.Write([]byte("payload"))
		return err
	})
	assertCallFinishes(t, "Commit", 2*time.Second, func() error {
		_, err := sink.Commit(context.Background())
		return err
	})
}

func TestQueuedStagingOperationsDoNotHoldRepositoryLock(t *testing.T) {
	t.Run("Abort", func(t *testing.T) {
		repo, staging := openStaging(t, "backup")
		queueAndAssertLockFree(t, repo, staging, staging.Abort)
	})
	t.Run("Create", func(t *testing.T) {
		repo, staging := openStaging(t, "backup")
		queueAndAssertLockFree(t, repo, staging, func() error {
			_, err := staging.Create("other")
			return err
		})
	})
	t.Run("Publish", func(t *testing.T) {
		repo, _ := newRepository(t)
		staging := begin(t, repo, "backup")
		commitBytes(t, staging, "payload", "ready")
		queueAndAssertLockFree(t, repo, staging, func() error {
			return staging.Publish(context.Background())
		})
	})
}

func TestConcurrentSinkAndStagingAbort(t *testing.T) {
	repo, _ := newRepository(t)
	staging := begin(t, repo, "backup")
	sink := create(t, staging, "payload")
	if _, err := io.WriteString(sink, "partial"); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errc := make(chan error, 2)
	go func() {
		<-start
		errc <- sink.Abort()
	}()
	go func() {
		<-start
		errc <- staging.Abort()
	}()
	close(start)
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for range 2 {
		select {
		case err := <-errc:
			if err != nil {
				t.Fatal(err)
			}
		case <-timer.C:
			t.Fatal("sink Abort and staging Abort deadlocked")
		}
	}
}

func openStaging(t *testing.T, dir string) (*Repository, *Staging) {
	t.Helper()
	repo, _ := newRepository(t)
	staging := begin(t, repo, dir)
	create(t, staging, "payload")
	return repo, staging
}

func queueAndAssertLockFree(t *testing.T, repo *Repository, staging *Staging, op func() error) {
	t.Helper()
	staging.mu.Lock()
	done := make(chan error, 1)
	go func() { done <- op() }()
	assertMutexFree(t, &repo.mu, 200*time.Millisecond)
	staging.mu.Unlock()
	assertCallFinishes(t, "queued staging operation", 2*time.Second, func() error { return <-done })
}

func TestConcurrentStagingDoesNotDeadlock(t *testing.T) {
	repo, _ := newRepository(t)
	seed := begin(t, repo, "seed")
	commitBytes(t, seed, "payload", "seed")
	if err := seed.Publish(context.Background()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() { readPublishedRepeated(t, repo) })
	}
	for worker := range 2 {
		wg.Go(func() { writeWaves(t, repo, worker) })
	}
	waitGroup(t, &wg)
}

func readPublishedRepeated(t *testing.T, repo *Repository) {
	t.Helper()
	for range 40 {
		if err := readPublished(repo, "seed/payload", "seed"); err != nil {
			t.Error(err)
			return
		}
	}
}

func writeWaves(t *testing.T, repo *Repository, worker int) {
	t.Helper()
	for i := range 8 {
		if err := writeWave(repo, worker, i); err != nil {
			t.Errorf("wave %d/%d: %v", worker, i, err)
			return
		}
	}
}

func waitGroup(t *testing.T, wg *sync.WaitGroup) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("concurrent staging and repository readers deadlocked")
	}
}

func readPublished(repo *Repository, name, contents string) error {
	files, err := repo.List("")
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return errors.New("repository list is empty")
	}
	file, err := repo.Open(name)
	if err != nil {
		return err
	}
	got, readErr := io.ReadAll(file)
	if err = errors.Join(readErr, file.Close()); err != nil {
		return err
	}
	if string(got) != contents {
		return fmt.Errorf("%s contents = %q", name, got)
	}
	size, err := repo.Size(name)
	if err != nil {
		return err
	}
	if size != int64(len(contents)) {
		return fmt.Errorf("%s size = %d", name, size)
	}
	return nil
}

func writeWave(repo *Repository, worker, i int) error {
	staging, err := repo.Begin(fmt.Sprintf("wave-%d-%d", worker, i))
	if err != nil {
		return err
	}
	sink, err := staging.Create("payload")
	if err != nil {
		return errors.Join(err, staging.Abort())
	}
	if _, err = io.WriteString(sink, "wave"); err != nil {
		return errors.Join(err, sink.Abort(), staging.Abort())
	}
	if i%2 == 0 {
		if _, err = sink.Commit(context.Background()); err != nil {
			return errors.Join(err, staging.Abort())
		}
		err = staging.Publish(context.Background())
		return errors.Join(err, staging.Abort())
	}
	return errors.Join(sink.Abort(), staging.Abort())
}

func callWithin(t *testing.T, d time.Duration, fn func() error) {
	t.Helper()
	assertCallFinishes(t, "call", d, fn)
}

func assertCallFinishes(t *testing.T, name string, d time.Duration, fn func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	case <-timer.C:
		t.Fatalf("%s did not finish within %s", name, d)
	}
}

func assertMutexFree(t *testing.T, mu *sync.Mutex, window time.Duration) {
	t.Helper()
	timer := time.NewTimer(window)
	defer timer.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	checks := 0
	for {
		if !mu.TryLock() {
			t.Fatal("repository lock held by a queued staging operation")
		}
		mu.Unlock()
		checks++
		select {
		case <-timer.C:
			if checks < 2 {
				t.Fatal("lock window ended before a second observation")
			}
			return
		case <-tick.C:
		}
	}
}
