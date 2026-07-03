//go:build windows

package daemon

import "os"

// pidAlive reports whether a process with the given pid currently exists.
// Unlike Unix, os.FindProcess on Windows opens a real handle to the process
// via OpenProcess and fails if it doesn't exist, so success alone is a
// sufficient liveness check (Process.Signal on Windows only supports
// os.Kill, so it can't be used the way Unix's Signal(0) is).
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	_, err := os.FindProcess(pid)
	return err == nil
}
