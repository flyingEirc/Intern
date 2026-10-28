//go:build linux || darwin

package physical

import (
	"errors"
	"math"

	"golang.org/x/sys/unix"
)

func inspectVolumeSpace(path string) (volumeSpace, error) {
	var stat unix.Stat_t
	if err := unix.Stat(path, &stat); err != nil {
		return volumeSpace{}, err
	}
	var capacity unix.Statfs_t
	if err := unix.Statfs(path, &capacity); err != nil {
		return volumeSpace{}, err
	}
	if capacity.Bsize <= 0 {
		return volumeSpace{}, errors.New("invalid filesystem block size")
	}
	// Bavail, not Bfree: blocks reserved for root are not usable by the runner.
	blocks, blockSize := uint64(capacity.Bavail), uint64(capacity.Bsize)
	available := uint64(math.MaxUint64)
	if blocks <= math.MaxUint64/blockSize {
		available = blocks * blockSize
	}
	return volumeSpace{Available: available, Device: uint64(stat.Dev)}, nil
}
