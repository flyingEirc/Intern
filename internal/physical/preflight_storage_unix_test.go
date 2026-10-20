//go:build linux || darwin

package physical

import (
	"math"
	"testing"
)

func TestRealStorageCapacityProbe(t *testing.T) {
	data, _ := preflightVolumeFixture(t)
	opts := PreflightOptions{
		Connection: Connection{DataDir: data},
		WorkRoot:   t.TempDir(), BackupRoot: t.TempDir(),
		MinWorkBytes: 1, MinBackupBytes: 1,
	}
	if err := checkPreflightStorage(t.Context(), opts); err != nil {
		t.Fatal(err)
	}
	space, err := inspectVolumeSpace(opts.WorkRoot)
	if err != nil || space.Available == 0 {
		t.Fatalf("real capacity probe failed: available=%d err=%v", space.Available, err)
	}
	opts.MinWorkBytes = math.MaxUint64
	requirePreflightCode(t, checkPreflightStorage(t.Context(), opts), "space_insufficient")
}
