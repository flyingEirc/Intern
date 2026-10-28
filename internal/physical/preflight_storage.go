package physical

import (
	"context"
	"errors"
	"math"
	"os"
)

func checkPreflightStorage(ctx context.Context, opts PreflightOptions) error {
	roots, err := preflightStorageRoots(opts)
	if err != nil {
		return err
	}
	for _, root := range roots {
		if err = probeStorageWrite(ctx, root); err != nil {
			return err
		}
	}
	work, err := inspectVolumeSpace(roots[0])
	if err != nil {
		return preflightFailure("capacity", "capacity_query_failed", "Cannot inspect available work-volume capacity.")
	}
	backup, err := inspectVolumeSpace(roots[1])
	if err != nil {
		return preflightFailure("capacity", "capacity_query_failed", "Cannot inspect available backup-volume capacity.")
	}
	return admitVolumeSpace(work, backup, opts.MinWorkBytes, opts.MinBackupBytes)
}

func preflightStorageRoots(opts PreflightOptions) ([2]string, error) {
	var roots [2]string
	data, err := canonicalDirectory(opts.Connection.DataDir)
	if err != nil {
		return roots, preflightFailure("capacity", "datadir_unreadable", "Cannot resolve the mounted data directory.")
	}
	for index, path := range []string{opts.WorkRoot, opts.BackupRoot} {
		root, err := canonicalDirectory(path)
		if err != nil {
			return roots, preflightFailure("capacity", "storage_root_invalid", "Work and backup roots must be existing accessible directories.")
		}
		if pathsOverlap(data, root) {
			return roots, preflightFailure("capacity", "storage_overlap", "Work and backup roots must not contain, or be contained by, datadir.")
		}
		roots[index] = root
	}
	if pathsOverlap(roots[0], roots[1]) {
		return roots, preflightFailure("capacity", "storage_overlap", "Work and backup roots must be separate non-overlapping directories.")
	}
	return roots, nil
}

func probeStorageWrite(ctx context.Context, root string) (retErr error) {
	if ctx.Err() != nil {
		return preflightExternalFailure(ctx, "capacity", "canceled", "Preflight was canceled.")
	}
	file, err := os.CreateTemp(root, ".intern-probe-*")
	if err != nil {
		return preflightFailure("capacity", "storage_unwritable", "Cannot create a private probe in a storage root; check runtime UID/GID and mount writability.")
	}
	defer func() {
		if err := os.Remove(file.Name()); err != nil {
			retErr = errors.Join(retErr, preflightFailure("capacity", "storage_probe_cleanup_failed", "Cannot remove a storage probe; correct storage permissions before retrying."))
		}
	}()
	_, writeErr := file.WriteString("intern-preflight\n")
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return preflightFailure("capacity", "storage_unwritable", "Cannot write and sync a private storage probe; check permissions and available capacity.")
	}
	return nil
}

// volumeSpace is one capacity observation. Available is the bytes an
// unprivileged write may still use, Device identifies the filesystem, and Block
// is the block size the filesystem charges every write in, so a caller can tell
// "less than one block left" from "plenty of space".
type volumeSpace struct {
	Available uint64
	Device    uint64
	Block     uint64
}

func admitVolumeSpace(work, backup volumeSpace, minWork, minBackup uint64) error {
	if minWork == 0 || minBackup == 0 {
		return preflightFailure("capacity", "capacity_policy_required", "Both storage capacity thresholds must be positive.")
	}
	if work.Available < minWork || backup.Available < minBackup {
		return preflightFailure("capacity", "space_insufficient", "Available work or backup volume space is below its configured minimum; free space or increase volume capacity.")
	}
	if work.Device == backup.Device {
		if minWork > math.MaxUint64-minBackup {
			return preflightFailure("capacity", "space_insufficient", "The combined work and backup capacity requirement exceeds the representable capacity.")
		}
		if min(work.Available, backup.Available) < minWork+minBackup {
			return preflightFailure("capacity", "space_insufficient", "Work and backup share a filesystem; its available space must cover both minimum reservations together.")
		}
	}
	return nil
}
