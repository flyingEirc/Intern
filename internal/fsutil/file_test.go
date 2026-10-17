package fsutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenFileRejectsEscapingSymlink(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "payload")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if f, err := OpenFile(link, os.O_WRONLY|os.O_TRUNC, 0600); err == nil {
		f.Close()
		t.Fatal("write escaped selected directory through symlink")
	}
	contents, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "unchanged" {
		t.Fatal("outside file was modified")
	}
}
