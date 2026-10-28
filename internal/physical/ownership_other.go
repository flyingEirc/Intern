//go:build !linux && !darwin

package physical

import "os"

func ownedByCurrentUser(os.FileInfo) bool { return false }

// executableForCurrentUser falls back to the mode bits where the platform cannot
// ask the kernel about this effective uid.
func executableForCurrentUser(_ string, info os.FileInfo) bool {
	return info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}
