//go:build darwin

package restore

import (
	"os"

	"golang.org/x/sys/unix"
)

// renameNoReplace publishes a directory only when the final name is still free.
func renameNoReplace(from, to string) error {
	if err := unix.RenamexNp(from, to, unix.RENAME_EXCL); err != nil {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
	}
	return nil
}
