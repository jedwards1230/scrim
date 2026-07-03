package daemon

import (
	"os"
	"testing"
	"time"

	"github.com/jedwards1230/scrim/internal/config"
	"github.com/jedwards1230/scrim/internal/state"
)

// TestFinalizeStopTreatsDeadPidAsImmediateSuccess reproduces the
// SIGKILL/OOM-kill scenario: the pid is already dead but nothing cleaned up
// the state file. finalizeStop must return success right away (not burn the
// full stopTimeout polling for the file to also disappear) and remove the
// stale file itself.
func TestFinalizeStopTreatsDeadPidAsImmediateSuccess(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{Dir: dir, Host: "127.0.0.1", Port: 7777, IdleTimeout: time.Hour}

	pid := deadPid(t)
	if err := state.Save(cfg.StateFilePath(), &state.State{
		PID:  pid,
		Host: cfg.Host,
		Port: cfg.Port,
	}); err != nil {
		t.Fatalf("state.Save() error = %v", err)
	}

	start := time.Now()
	if err := finalizeStop(cfg, pid); err != nil {
		t.Fatalf("finalizeStop() error = %v, want nil (pid already dead)", err)
	}
	if elapsed := time.Since(start); elapsed > stopTimeout/2 {
		t.Errorf("finalizeStop() took %v for an already-dead pid, want a fast return well under stopTimeout=%v", elapsed, stopTimeout)
	}

	if _, err := os.Stat(cfg.StateFilePath()); !os.IsNotExist(err) {
		t.Errorf("state file still exists after finalizeStop(), want it cleaned up (stat err = %v)", err)
	}
}
