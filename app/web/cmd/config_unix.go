//go:build unix

package cmd

import (
	"os"
	"syscall"

	"github.com/flyingEirc/Intern/internal/fsutil"
)

// openConfig opens path for reading. O_NONBLOCK keeps a FIFO with no writer from blocking in open(2).
func openConfig(path string) (*os.File, error) {
	return fsutil.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}

func openDatabaseSecret(path string) (*os.File, error) {
	return fsutil.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
}

// setConfigBlocking clears O_NONBLOCK so a regular file is read in blocking mode.
func setConfigBlocking(f *os.File) error {
	return syscall.SetNonblock(int(f.Fd()), false)
}
