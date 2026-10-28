//go:build !linux && !darwin

package physical

import "os"

func ownedByCurrentUser(os.FileInfo) bool { return false }
