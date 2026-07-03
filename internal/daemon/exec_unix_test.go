//go:build unix

package daemon

import "os/exec"

// trivialExitingCommand returns a Cmd for an external process that starts
// and exits almost immediately, for tests that need a real (but dead) pid.
func trivialExitingCommand() *exec.Cmd {
	return exec.Command("true")
}

// shortLivedCommand returns a Cmd for an external process that stays alive
// briefly (long enough for a test to observe it as a live pid) and then
// exits on its own well within stopTimeout, for tests that need a real pid
// that is alive when the test starts but dies shortly after.
func shortLivedCommand() *exec.Cmd {
	return exec.Command("sleep", "0.3")
}
