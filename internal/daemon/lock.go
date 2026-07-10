package daemon

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// staleLockAge is how old an unremovable lock file must be before it's
// considered abandoned (e.g. the holder crashed between creating the
// lockfile and writing its pid) and safe to steal.
const staleLockAge = 60 * time.Second

// withSpawnLock runs fn while holding an exclusive filesystem lock at
// lockPath, so that concurrent CLI invocations racing to spawn a daemon
// converge on exactly one spawn. It uses O_CREATE|O_EXCL for the lock
// itself (portable across darwin/linux/windows), records the holder's pid
// in the file for stale-lock detection, and polls with a short sleep while
// waiting for a lock held by another process.
func withSpawnLock(lockPath string, timeout time.Duration, fn func() error) error {
	deadline := time.Now().Add(timeout)
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // G304: lockPath is derived from the daemon's own data dir, not user input
		if err == nil {
			_, writeErr := fmt.Fprintf(f, "%d\n", os.Getpid())
			closeErr := f.Close()
			release := func() { os.Remove(lockPath) } //nolint:errcheck // best-effort lock cleanup
			defer release()
			if writeErr != nil {
				return fmt.Errorf("writing spawn lock: %w", writeErr)
			}
			if closeErr != nil {
				return fmt.Errorf("closing spawn lock: %w", closeErr)
			}
			return fn()
		}
		if !os.IsExist(err) {
			return fmt.Errorf("creating spawn lock %s: %w", lockPath, err)
		}

		if isStaleLock(lockPath) {
			_ = os.Remove(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting %s for spawn lock %s", timeout, lockPath)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// isStaleLock reports whether the lock file at path was left behind by a
// process that's no longer running, or is simply old enough to assume its
// holder is gone.
func isStaleLock(path string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false // lock disappeared concurrently; not our problem to steal
	}
	if time.Since(fi.ModTime()) > staleLockAge {
		return true
	}

	data, err := os.ReadFile(path) //nolint:gosec // path is our own lockfile under the configured --dir
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return false
	}
	return !pidAlive(pid)
}
