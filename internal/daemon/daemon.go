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
	"github.com/jedwards1230/scrim/internal/version"
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
// (under a spawn lock) if none is found. A running daemon whose reported
// version doesn't match this CLI's own build is treated the same as a
// stale/dead one: it's stopped gracefully first, then this function falls
// through to the normal spawn path below, which finds no healthy daemon and
// starts a fresh one -- transparently, without the caller needing to do
// anything differently. Canvases are untouched throughout: they live on
// disk under cfg.Dir, independent of the daemon process that happens to be
// serving them.
func Ensure(cfg config.Config) (*state.State, error) {
	if st, ok := healthyState(cfg); ok {
		stopped, err := stopIfVersionSkewed(cfg, st, version.Short())
		if err != nil {
			return nil, err
		}
		if !stopped {
			return st, nil
		}
	}

	// The lockfile itself needs somewhere to live before we can even
	// attempt to acquire it. HardenPermissions also tightens any
	// preexisting state/log file left behind at looser permissions by an
	// older scrim version, not just freshly-created ones.
	if err := cfg.HardenPermissions(); err != nil {
		return nil, fmt.Errorf("hardening scrim dir permissions: %w", err)
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

	// Use st's own Host/Port (where the daemon actually bound), not cfg's --
	// a running daemon can legitimately differ from the caller's config
	// (different --host/--port than it was started with, or an
	// auto-assigned port), and sending the stop request to the wrong
	// endpoint would silently no-op against whatever happens to be
	// listening there instead.
	client := apiclient.NewWithToken(st.BaseURL(), st.Token)
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

	client := apiclient.NewWithToken(st.BaseURL(), st.Token)
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

// isDevVersion reports whether v is version.Short()'s fallback sentinel for
// a build with no version information at all -- neither an -ldflags -X
// ...Version stamp nor a VCS revision picked up via debug.ReadBuildInfo
// (e.g. a binary built outside a git checkout, or with -buildvcs=false).
func isDevVersion(v string) bool {
	return v == "" || v == "dev"
}

// versionSkewed reports whether a running daemon's reported version differs
// from cliVersion closely enough that the daemon should be replaced.
//
// It is deliberately false whenever cliVersion is the "dev" sentinel
// (isDevVersion): an unversioned dev build (`go run`/`go test` outside a git
// checkout, or built with -buildvcs=false) would otherwise report a
// "mismatch" against every real daemon it ever finds -- including ones it
// started itself moments earlier -- restarting a perfectly healthy daemon on
// every single invocation. A daemon reporting the dev sentinel against a
// versioned CLI is still a genuine mismatch (e.g. upgrading from a dev build
// to a release build), so that direction is not exempted.
func versionSkewed(cliVersion, daemonVersion string) bool {
	if isDevVersion(cliVersion) {
		return false
	}
	return cliVersion != daemonVersion
}

// stopIfVersionSkewed stops the currently-healthy daemon described by st
// when its reported version differs from cliVersion, so Ensure's caller can
// fall through to its normal spawn path and start a fresh one. Returns true
// when a stop was actually issued.
func stopIfVersionSkewed(cfg config.Config, st *state.State, cliVersion string) (bool, error) {
	if !versionSkewed(cliVersion, st.Version) {
		return false, nil
	}
	if _, err := Stop(cfg); err != nil {
		return false, fmt.Errorf("restarting version-mismatched daemon (cli %s, daemon %s): %w", cliVersion, st.Version, err)
	}
	return true, nil
}

// spawnAndWait re-execs the current binary as a detached `serve` process
// configured from cfg, then polls until it's healthy or startupTimeout
// elapses. Callers must hold the spawn lock.
func spawnAndWait(cfg config.Config) error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating scrim executable: %w", err)
	}

	// HardenPermissions creates cfg.Dir if missing and tightens it (and any
	// preexisting state/log file under it) to owner-only, before the log
	// file is opened below -- so a stale daemon.log left behind at looser
	// permissions by an older scrim version gets tightened here rather than
	// silently staying world-readable.
	if err := cfg.HardenPermissions(); err != nil {
		return fmt.Errorf("hardening scrim dir permissions: %w", err)
	}
	logFile, err := os.OpenFile(cfg.LogFilePath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
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
	if cfg.NoMDNS {
		args = append(args, "--no-mdns")
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
