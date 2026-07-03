package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPidAlive(t *testing.T) {
	if !pidAlive(os.Getpid()) {
		t.Error("pidAlive(self) = false, want true")
	}
	if pidAlive(0) {
		t.Error("pidAlive(0) = true, want false")
	}
	if pidAlive(-1) {
		t.Error("pidAlive(-1) = true, want false")
	}
}

func TestIsStaleLock(t *testing.T) {
	dir := t.TempDir()

	t.Run("missing lock is not stale", func(t *testing.T) {
		if isStaleLock(filepath.Join(dir, "missing.lock")) {
			t.Error("isStaleLock(missing) = true, want false")
		}
	})

	t.Run("fresh lock held by live pid is not stale", func(t *testing.T) {
		path := filepath.Join(dir, "live.lock")
		writeLockFile(t, path, os.Getpid(), time.Now())
		if isStaleLock(path) {
			t.Error("isStaleLock(live) = true, want false")
		}
	})

	t.Run("lock held by dead pid is stale", func(t *testing.T) {
		path := filepath.Join(dir, "dead.lock")
		writeLockFile(t, path, deadPid(t), time.Now())
		if !isStaleLock(path) {
			t.Error("isStaleLock(dead pid) = false, want true")
		}
	})

	t.Run("old lock is stale regardless of pid", func(t *testing.T) {
		path := filepath.Join(dir, "old.lock")
		writeLockFile(t, path, os.Getpid(), time.Now().Add(-2*staleLockAge))
		if !isStaleLock(path) {
			t.Error("isStaleLock(old) = false, want true")
		}
	})
}

func TestWithSpawnLockMutualExclusion(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "daemon.lock")

	var inCritical int32
	var maxConcurrent int32
	var wg sync.WaitGroup
	const n = 8

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := withSpawnLock(lockPath, 5*time.Second, func() error {
				cur := atomic.AddInt32(&inCritical, 1)
				for {
					old := atomic.LoadInt32(&maxConcurrent)
					if cur <= old || atomic.CompareAndSwapInt32(&maxConcurrent, old, cur) {
						break
					}
				}
				time.Sleep(10 * time.Millisecond)
				atomic.AddInt32(&inCritical, -1)
				return nil
			})
			if err != nil {
				t.Errorf("withSpawnLock() error = %v", err)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&maxConcurrent); got != 1 {
		t.Errorf("max concurrent holders = %d, want 1", got)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Error("lock file was not cleaned up after all holders finished")
	}
}

func TestWithSpawnLockTimesOutOnLiveHolder(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "daemon.lock")
	writeLockFile(t, lockPath, os.Getpid(), time.Now())

	err := withSpawnLock(lockPath, 200*time.Millisecond, func() error {
		t.Fatal("fn should not run while lock is held by a live pid")
		return nil
	})
	if err == nil {
		t.Fatal("withSpawnLock() error = nil, want timeout error")
	}
}

func TestWithSpawnLockStealsStaleLock(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "daemon.lock")
	writeLockFile(t, lockPath, deadPid(t), time.Now())

	ran := false
	err := withSpawnLock(lockPath, 2*time.Second, func() error {
		ran = true
		return nil
	})
	if err != nil {
		t.Fatalf("withSpawnLock() error = %v", err)
	}
	if !ran {
		t.Error("withSpawnLock() did not run fn after stealing stale lock")
	}
}

func writeLockFile(t *testing.T, path string, pid int, mtime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, fmt.Appendf(nil, "%d\n", pid), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// deadPid returns a pid that is guaranteed not to be alive, by starting a
// trivial subprocess and waiting for it to exit.
func deadPid(t *testing.T) int {
	t.Helper()
	cmd := trivialExitingCommand()
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting throwaway process: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("waiting for throwaway process: %v", err)
	}
	return pid
}
