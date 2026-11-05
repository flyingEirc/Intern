//go:build !unix

package cmd

import "testing"

func configFIFO(t *testing.T) string {
	t.Helper()
	t.Skip("FIFO configuration open is unix-specific")
	return ""
}
