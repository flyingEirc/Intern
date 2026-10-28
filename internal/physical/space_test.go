package physical

import (
	"math"
	"testing"
)

// TestQuerySpaceReportsTheFilesystemBlockSize requires one capacity observation to
// carry the block size the filesystem reports, not a page size this process
// assumed. The restore engine refuses a failed tool step as disk_full when less
// than one block is free, so a filesystem with larger blocks has to be measured in
// its own unit instead of a fixed 4 KiB guess.
func TestQuerySpaceReportsTheFilesystemBlockSize(t *testing.T) {
	space, err := QuerySpace(t.TempDir())
	if err != nil {
		t.Fatalf("capacity inspection is unavailable on this platform: %v", err)
	}
	if space.BlockBytes <= 0 {
		t.Fatalf("block size = %d, want the positive block size statfs reports", space.BlockBytes)
	}
	if space.AvailableBytes < 0 {
		t.Fatalf("available = %d, want a nonnegative observation", space.AvailableBytes)
	}
	// Free space is a whole number of blocks, so the observation can never claim a
	// byte count the filesystem could not actually hand out.
	if space.AvailableBytes != math.MaxInt64 && space.AvailableBytes%space.BlockBytes != 0 {
		t.Fatalf("available = %d is not a multiple of the %d-byte block", space.AvailableBytes, space.BlockBytes)
	}
}
