package restore

import (
	"context"
	"errors"
	"path/filepath"
)

// Recover removes exactly the two durable targets of each given inactive restore
// id: <workRoot>/restore-<id> and <exportRoot>/.pending-<id>. It never scans a
// prefix and never deletes a published export. It is idempotent.
func Recover(ctx context.Context, workRoot, exportRoot string, restoreIDs []string) error {
	if err := validatePrivateWorkRoot(workRoot); err != nil {
		return err
	}
	if err := CheckExportRoot(exportRoot); err != nil {
		return err
	}
	for _, id := range restoreIDs {
		if !restoreIDPattern.MatchString(id) {
			return errors.New("restore: recovery requires 32 lowercase hex restore ids")
		}
	}
	var result error
	for _, id := range restoreIDs {
		if err := ctx.Err(); err != nil {
			return errors.Join(result, err)
		}
		result = errors.Join(result,
			removeExactDirectory(filepath.Join(workRoot, "restore-"+id), workRoot),
			removeExactDirectory(filepath.Join(exportRoot, ".pending-"+id), exportRoot),
		)
	}
	return result
}

// RestoreTargets reports the two paths Recover owns for one restore id. It is
// read-only and exists so W5 can report side effects without guessing names.
func RestoreTargets(workRoot, exportRoot, restoreID string) (workDir, pendingExport string, ok bool) {
	if !restoreIDPattern.MatchString(restoreID) || !filepath.IsAbs(workRoot) || !filepath.IsAbs(exportRoot) {
		return "", "", false
	}
	return filepath.Join(workRoot, "restore-"+restoreID), filepath.Join(exportRoot, ".pending-"+restoreID), true
}
