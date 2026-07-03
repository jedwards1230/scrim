//go:build unix

package daemon

import (
	"os/exec"
	"syscall"
)

// detach configures cmd to run in its own session, fully independent of the
// spawning CLI process's process group and controlling terminal, so it
// survives after the CLI exits.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
