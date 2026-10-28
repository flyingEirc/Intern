//go:build linux || darwin

package physical

import (
	"os"
	"syscall"
)

// ownedByCurrentUser reports whether the inode belongs to this effective uid.
func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}
