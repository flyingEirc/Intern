//go:build !linux && !darwin

package restore

import "errors"

// renameNoReplace is unavailable where the platform cannot publish exclusively.
func renameNoReplace(string, string) error {
	return errors.New("restore: exclusive directory rename is unsupported on this platform")
}
