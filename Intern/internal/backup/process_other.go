//go:build !linux

package backup

import "os/exec"

// Other kernels retain CommandContext cancellation and Wait cleanup; Linux's
// parent-death guarantee is not advertised for native non-Linux executions.
func executeTool(cmd *exec.Cmd) error { return cmd.Run() }
