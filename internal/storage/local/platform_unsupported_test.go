//go:build !linux && !darwin

package local

import (
	"errors"
	"testing"
)

func TestOpenUnsupportedPlatform(t *testing.T) {
	repo, err := Open(t.TempDir())
	if repo != nil {
		t.Cleanup(func() { repo.Close() })
	}
	if !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("Open error = %v, want ErrUnsupported", err)
	}
}
