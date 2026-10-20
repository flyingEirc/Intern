//go:build !linux

package physical

import (
	"context"
	"errors"
)

func executeProcess(_ context.Context, _ processSpec) error {
	return errors.New("physical: native tool lifecycle requires Linux")
}
