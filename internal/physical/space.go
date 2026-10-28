package physical

import "math"

// Space is one filesystem observation. Device identifies the filesystem so a
// caller can tell whether two roots share capacity. AvailableBytes is usable
// space, not the total size.
type Space struct {
	AvailableBytes int64
	Device         uint64
}

// QuerySpace reports usable bytes and the device of the filesystem holding an
// existing path. It is an observation, not a reservation: another writer can
// consume the space before it is used.
func QuerySpace(path string) (Space, error) {
	volume, err := inspectVolumeSpace(path)
	if err != nil {
		return Space{}, err
	}
	available := int64(math.MaxInt64)
	if volume.Available < math.MaxInt64 {
		available = int64(volume.Available)
	}
	return Space{AvailableBytes: available, Device: volume.Device}, nil
}

// AvailableBytes reports usable bytes on the filesystem holding an existing path.
func AvailableBytes(path string) (int64, error) {
	space, err := QuerySpace(path)
	if err != nil {
		return 0, err
	}
	return space.AvailableBytes, nil
}
