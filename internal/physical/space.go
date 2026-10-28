package physical

import "math"

// Space is one filesystem observation. Device identifies the filesystem so a
// caller can tell whether two roots share capacity. AvailableBytes is usable
// space, not the total size. BlockBytes is the block size statfs reports, the
// unit every write is charged in: a filesystem with fewer free bytes than one of
// its own blocks cannot take another write, however large that block is.
type Space struct {
	AvailableBytes int64
	Device         uint64
	BlockBytes     int64
}

// QuerySpace reports usable bytes, the device and the block size of the
// filesystem holding an existing path. It is an observation, not a reservation:
// another writer can consume the space before it is used.
func QuerySpace(path string) (Space, error) {
	volume, err := inspectVolumeSpace(path)
	if err != nil {
		return Space{}, err
	}
	available := int64(math.MaxInt64)
	if volume.Available < math.MaxInt64 {
		available = int64(volume.Available)
	}
	block := int64(math.MaxInt64)
	if volume.Block < math.MaxInt64 {
		block = int64(volume.Block)
	}
	return Space{AvailableBytes: available, Device: volume.Device, BlockBytes: block}, nil
}

// AvailableBytes reports usable bytes on the filesystem holding an existing path.
func AvailableBytes(path string) (int64, error) {
	space, err := QuerySpace(path)
	if err != nil {
		return 0, err
	}
	return space.AvailableBytes, nil
}
