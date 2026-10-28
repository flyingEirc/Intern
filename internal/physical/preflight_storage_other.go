//go:build !linux && !darwin

package physical

import "errors"

func inspectVolumeSpace(string) (volumeSpace, error) {
	return volumeSpace{}, errors.New("physical backup capacity inspection is unsupported on this platform")
}
