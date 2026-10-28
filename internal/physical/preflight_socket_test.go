//go:build linux || darwin

package physical

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestVolumeAllowsOnlyTheConfiguredSocketAlias(t *testing.T) {
	dir, identity := preflightVolumeFixture(t)
	socketDir, err := os.MkdirTemp("", "p2-sock-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if removeErr := os.RemoveAll(socketDir); removeErr != nil {
			t.Fatal(removeErr)
		}
	})
	socket := filepath.Join(socketDir, "mysql.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := listener.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	})
	alias := filepath.Join(dir, "mysql.sock")
	if err = os.Symlink(socket, alias); err != nil {
		t.Fatal(err)
	}
	if err = checkDataVolume(t.Context(), dir, identity, socket); err != nil {
		t.Fatal(err)
	}
	requirePreflightCode(t, checkDataVolume(t.Context(), dir, identity, filepath.Join(socketDir, "other.sock")), "datadir_symlink_unsupported")
	if err = os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(dir, "ibdata1")
	if err = os.Symlink(payload, alias); err != nil {
		t.Fatal(err)
	}
	requirePreflightCode(t, checkDataVolume(t.Context(), dir, identity, payload), "datadir_symlink_unsupported")
}
