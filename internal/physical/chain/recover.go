package chain

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/flyingEirc/Intern/internal/physical/store"
	"github.com/flyingEirc/Intern/internal/storage/local"
)

// Recover runs under the caller's exclusive scheduler ownership. activeRuns is
// the complete live-run set for this instance. It never scans directory names.
func Recover(ctx context.Context, s *store.Store, repo *local.Repository, workRoot, instanceID string, activeRuns []string) error {
	if s == nil || repo == nil || !filepath.IsAbs(workRoot) || !safeComponent(instanceID) {
		return errors.New("physical chain: invalid recovery ownership")
	}
	rows, err := s.PendingCleanup(ctx, instanceID, activeRuns)
	if err != nil {
		return err
	}
	return reclaim(ctx, s, repo, workRoot, instanceID, rows)
}

func cleanupBackup(ctx context.Context, s *store.Store, repo *local.Repository, workRoot, instanceID, backupID string) error {
	rows, err := s.PendingCleanup(ctx, instanceID, nil)
	if err != nil {
		return err
	}
	owned := rows[:0]
	for _, row := range rows {
		if row.BackupID == backupID {
			owned = append(owned, row)
		}
	}
	return reclaim(ctx, s, repo, workRoot, instanceID, owned)
}

func reclaim(ctx context.Context, s *store.Store, repo *local.Repository, workRoot, instanceID string, rows []store.Cleanup) error {
	var result error
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return errors.Join(result, err)
		}
		result = errors.Join(result, reclaimOne(ctx, s, repo, workRoot, instanceID, row))
	}
	return result
}

func reclaimOne(ctx context.Context, s *store.Store, repo *local.Repository, workRoot, instanceID string, row store.Cleanup) error {
	backup, err := s.Backup(ctx, row.BackupID)
	if err != nil {
		return err
	}
	if backup.InstanceID != instanceID || backup.RunID != row.RunID || backup.Status == "completed" {
		return errors.New("physical chain: cleanup ownership mismatch")
	}
	if backup.Status == "running" {
		if err = s.Fail(ctx, backup.ID, "interrupted"); err != nil {
			return err
		}
	}
	if err = reclaimTarget(repo, workRoot, instanceID, backup, row); err != nil {
		return errors.Join(errors.New("physical chain: cleanup target failed"), s.CleanupFailed(ctx, row.ID, "cleanup_failed"))
	}
	return s.CleanupDone(ctx, row.ID)
}

func reclaimTarget(repo *local.Repository, workRoot, instanceID string, backup store.Backup, row store.Cleanup) error {
	switch row.Kind {
	case "staging":
		if row.Target != row.RunID {
			return errors.New("physical chain: staging ownership mismatch")
		}
		return repo.ReclaimPending(row.Target)
	case "workdir":
		return removeRun(workRoot, row.Target, row.RunID)
	case "object":
		if row.Target != backup.ObjectPath || !strings.HasPrefix(row.Target, instanceID+"/") {
			return errors.New("physical chain: object ownership mismatch")
		}
		return repo.ReclaimPublished(row.Target)
	default:
		return errors.New("physical chain: unknown cleanup target")
	}
}

func removeRun(workRoot, target, runID string) (result error) {
	if !safeComponent(runID) || !filepath.IsAbs(workRoot) || target != filepath.Join(workRoot, "run-"+runID) {
		return errors.New("physical chain: work directory ownership mismatch")
	}
	root, err := openWorkRoot(workRoot)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, root.Close()) }()
	name := "run-" + runID
	child, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return syncWorkRoot(root)
	}
	if err != nil {
		return err
	}
	if !child.IsDir() || child.Mode()&os.ModeSymlink != 0 {
		return errors.New("physical chain: refusing non-directory run target")
	}
	if err = root.RemoveAll(name); err != nil {
		return err
	}
	return syncWorkRoot(root)
}

func openWorkRoot(workRoot string) (*os.Root, error) {
	info, err := os.Lstat(workRoot)
	if err != nil {
		return nil, err
	}
	if info.Mode() != os.ModeDir|0o700 {
		return nil, errors.New("physical chain: work root must be a private non-symlink directory")
	}
	root, err := os.OpenRoot(workRoot)
	if err != nil {
		return nil, err
	}
	pinned, err := root.Stat(".")
	if err != nil || !os.SameFile(info, pinned) {
		return nil, errors.Join(errors.New("physical chain: work root changed"), root.Close())
	}
	return root, nil
}

func syncWorkRoot(root *os.Root) (result error) {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, dir.Close()) }()
	return dir.Sync()
}
