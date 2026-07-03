//go:build unix

package daemon

import (
	"errors"
	"os"
	"syscall"
)

// pidAlive reports whether a process with the given pid currently exists,
// by sending it signal 0 (which performs existence/permission checks
// without actually delivering a signal). Only "permission denied" is
// treated as "alive but not signalable by us" — every other error
// (including ESRCH, and Go's own "process already finished" shortcut for
// pids this process has already reaped via Wait, which never reaches the
// underlying syscall) is treated as "not alive".
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}
