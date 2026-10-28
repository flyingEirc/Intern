//go:build linux || darwin

package physical

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// ownedByCurrentUser reports whether the inode belongs to this effective uid.
func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

// executableForCurrentUser reports whether this effective uid may execute path.
// The kernel decides it, so the check follows the same owner/group/other class the
// exec will, including supplementary groups and ACLs.
func executableForCurrentUser(path string, info os.FileInfo) bool {
	if !info.Mode().IsRegular() {
		return false
	}
	return unix.Access(path, unix.X_OK) == nil
}
