// Package daemon manages the scrim daemon's lifecycle from the CLI side:
// detecting whether a healthy daemon is already running, self-starting one
// when needed (with a filesystem lock so concurrent CLI invocations
// converge on a single spawn), and asking a running daemon to stop.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/jedwards1230/scrim/internal/apiclient"
	"github.com/jedwards1230/scrim/internal/config"
	"github.com/jedwards1230/scrim/internal/state"
)

const (
	// healthCheckTimeout bounds a single /api/status probe.
	healthCheckTimeout = 800 * time.Millisecond
	// healthCheckRetries is how many quick probes healthyState makes before
	// giving up, to absorb brief scheduling jitter right after the daemon
	// binds its listener and writes its state file.
	healthCheckRetries = 3
	// spawnLockTimeout bounds how long a CLI invocation waits for another
	// invocation's in-progress spawn to finish.
	spawnLockTimeout = 15 * time.Second
	// startupTimeout bounds how long a freshly spawned daemon has to become
	// healthy before Ensure gives up.
	startupTimeout = 10 * time.Second
	// stopTimeout bounds how long Stop waits for a graceful shutdown to
	// complete (process exit + state file removal).
	stopTimeout = 5 * time.Second
)

// Ensure returns the state of a healthy running daemon, self-starting one
// (under a spawn lock) if none is found.
func Ensure(cfg config.Config) (*state.State, error) {
	if st, ok := healthyState(cfg); ok {
		return st, nil
	}

	// The lockfile itself needs somewhere to live before we can even
	// attempt to acquire it.
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil { //nolint:gosec // config dir is a user-owned working directory
		return nil, fmt.Errorf("creating scrim dir: %w", err)
	}

	lockErr := withSpawnLock(cfg.LockFilePath(), spawnLockTimeout, func() error {
		// Re-check inside the lock: another invocation may have just
		// finished spawning while we were waiting for the lock.
		if _, ok := healthyState(cfg); ok {
			return nil
		}
		return spawnAndWait(cfg)
	})
	if lockErr != nil {
		return nil, lockErr
	}

	st, ok := healthyState(cfg)
	if !ok {
		return nil, errors.New("daemon did not become healthy after spawning")
	}
	return st, nil
}

// TryLoadHealthy reports whether a healthy daemon is currently running,
// without starting one. Verbs that shouldn't self-start (status, stop, rm's
// fallback path) use this.
func TryLoadHealthy(cfg config.Config) (*state.State, bool) {
	return healthyState(cfg)
}

// Stop asks a running daemon to shut down gracefully and waits for it to
// exit. It is a no-op (returns nil, false) if no healthy daemon is found.
func Stop(cfg config.Config) (found bool, err error) {
	st, ok := healthyState(cfg)
	if !ok {
		return false, nil
	}

	client := apiclient.NewWithToken(cfg.BaseURL(), st.Token)
	ctx, cancel := context.WithTimeout(context.Background(), healthCheckTimeout)
	defer cancel()
	if err := client.Stop(ctx); err != nil {
		return true, fmt.Errorf("requesting daemon stop: %w", err)
	}

	if err := finalizeStop(cfg, st.PID); err != nil {
		return true, err
	}
	return true, nil
}

// finalizeStop waits for pid to exit after a stop has been requested. A
// confirmed-dead pid is success on its own — a SIGKILL/OOM-kill leaves
// nothing to clean up the state file, so finalizeStop removes it itself
// rather than keep polling for it to also disappear (which would report a
// false "timed out" for an already-dead daemon).
func finalizeStop(cfg config.Config, pid int) error {
	deadline := time.Now().Add(stopTimeout)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			_ = state.Remove(cfg.StateFilePath())
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for daemon (pid %d) to stop", pid)
}

// healthyState reads the state file and, if present and well-formed,
// confirms the recorded pid is alive and its /api/status endpoint responds.
func healthyState(cfg config.Config) (*state.State, bool) {
	st, err := state.Load(cfg.StateFilePath())
	if err != nil {
		return nil, false
	}
	if !pidAlive(st.PID) {
		return nil, false
	}

	client := apiclient.NewWithToken(fmt.Sprintf("http://%s:%d", st.Host, st.Port), st.Token)
	for i := 0; i < healthCheckRetries; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), healthCheckTimeout)
		_, err := client.Status(ctx)
		cancel()
		if err == nil {
			return st, true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, false
}

// spawnAndWait re-execs the current binary as a detached `serve` process
// configured from cfg, then polls until it's healthy or startupTimeout
// elapses. Callers must hold the spawn lock.
func spawnAndWait(cfg config.Config) error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating scrim executable: %w", err)
	}

	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil { //nolint:gosec // config dir is a user-owned working directory
		return fmt.Errorf("creating scrim dir: %w", err)
	}
	logFile, err := os.OpenFile(cfg.LogFilePath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) //nolint:gosec // daemon log is not sensitive
	if err != nil {
		return fmt.Errorf("opening daemon log: %w", err)
	}
	defer logFile.Close() //nolint:errcheck // the child inherits its own fd copy; our copy's close error isn't actionable

	args := []string{
		"serve",
		"--dir", cfg.Dir,
		"--host", cfg.Host,
		"--port", strconv.Itoa(cfg.Port),
		"--idle-timeout", cfg.IdleTimeout.String(),
	}
	if cfg.NoAuth {
		args = append(args, "--no-auth")
	}

	cmd := exec.Command(exePath, args...) //nolint:gosec // exePath is our own binary (os.Executable), args are our own config
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	detach(cmd)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting daemon process: %w", err)
	}
	// The daemon is detached (its own session); we don't wait on it, so
	// release our handle to it rather than leaving it around uncollected.
	_ = cmd.Process.Release()

	deadline := time.Now().Add(startupTimeout)
	for time.Now().Before(deadline) {
		if _, ok := healthyState(cfg); ok {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("daemon did not become healthy within %s (see %s)", startupTimeout, cfg.LogFilePath())
}
