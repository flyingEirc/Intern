//go:build linux

package restore

import (
	"os"

	"golang.org/x/sys/unix"
)

// renameNoReplace publishes a directory only when the final name is still free.
func renameNoReplace(from, to string) error {
	if err := unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_NOREPLACE); err != nil {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
	}
	return nil
}
