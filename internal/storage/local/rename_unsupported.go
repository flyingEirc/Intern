//go:build !linux && !darwin

package local

import (
	"errors"
	"fmt"
	"os"
	"runtime"
)

// errUnsupportedPlatform is shared by Open and renameExclusive so unsupported
// platforms fail closed with errors.ErrUnsupported. It is a package variable
// because fmt.Errorf in the function body is inlined and staticcheck then
// treats that call as never returning a nil interface (SA4023).
var errUnsupportedPlatform = fmt.Errorf("exclusive directory rename is unsupported on %s: %w", runtime.GOOS, errors.ErrUnsupported)

func unsupportedPlatform() error { return errUnsupportedPlatform }

func renameExclusive(*os.File, string, *os.File, string) error {
	return errUnsupportedPlatform
}
