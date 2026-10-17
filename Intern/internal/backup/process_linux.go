package backup

import (
	"os/exec"
	"runtime"
	"syscall"
)

func executeTool(cmd *exec.Cmd) error {
	// Linux delivers Pdeathsig when the creating thread dies. Keep that thread
	// alive for the entire child lifetime, including a blocking Wait.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	return cmd.Run()
}
