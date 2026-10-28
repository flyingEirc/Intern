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
// Faccessat with AT_EACCESS asks the kernel about the effective credentials — the
// same ones the exec will use — while a plain Access call consults the real
// uid/gid, which differ whenever a setuid or setgid program runs.
func executableForCurrentUser(path string, info os.FileInfo) bool {
	if !info.Mode().IsRegular() {
		return false
	}
	return unix.Faccessat(unix.AT_FDCWD, path, unix.X_OK, unix.AT_EACCESS) == nil
}
