//go:build windows

package daemon

import (
	"os/exec"
	"syscall"
)

// detach configures cmd to run detached from the spawning CLI process's
// console, so it survives after the CLI exits.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | 0x00000008} // DETACHED_PROCESS
}
