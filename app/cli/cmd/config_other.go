//go:build !unix

package cmd

import (
	"os"

	"github.com/flyingEirc/Intern/internal/fsutil"
)

func openConfig(path string) (*os.File, error) {
	return fsutil.Open(path)
}

func setConfigBlocking(*os.File) error { return nil }
