package local

import (
	"os"

	"golang.org/x/sys/unix"
)

func renameExclusive(from *os.File, oldName string, to *os.File, newName string) error {
	if err := unix.RenameatxNp(int(from.Fd()), oldName, int(to.Fd()), newName, unix.RENAME_EXCL); err != nil {
		return &os.LinkError{Op: "rename", Old: oldName, New: newName, Err: err}
	}
	return nil
}
