//go:build unix

package daemon

import "os/exec"

// trivialExitingCommand returns a Cmd for an external process that starts
// and exits almost immediately, for tests that need a real (but dead) pid.
func trivialExitingCommand() *exec.Cmd {
	return exec.Command("true")
}
