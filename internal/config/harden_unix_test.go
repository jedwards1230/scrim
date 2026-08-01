//go:build !windows

package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestHardenPermissionsFreshDir confirms a brand-new --dir comes up at
// owner-only permissions with no state/log files yet to tighten.
func TestHardenPermissionsFreshDir(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "scrim-fresh")
	cfg := Config{Dir: dir, Host: "127.0.0.1", Port: 7777}

	if err := cfg.HardenPermissions(); err != nil {
		t.Fatalf("HardenPermissions() error = %v", err)
	}
	assertMode(t, dir, dirPerm)
}

// TestHardenPermissionsTightensExisting is the regression test for the
// actual privacy requirement: a --dir (and state/log files under it)
// created loose -- by an older scrim version, or by a user's own `mkdir` --
// must not silently stay world-readable forever. HardenPermissions must
// tighten them on this startup, not just on the directory's original
// creation.
func TestHardenPermissionsTightensExisting(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("os.Chmod(dir) setup error = %v", err)
	}

	statePath := filepath.Join(dir, "daemon.json")
	logPath := filepath.Join(dir, "daemon.log")
	if err := os.WriteFile(statePath, []byte("{}"), 0o644); err != nil {
		t.Fatalf("writing state file: %v", err)
	}
	if err := os.WriteFile(logPath, []byte("log\n"), 0o644); err != nil {
		t.Fatalf("writing log file: %v", err)
	}

	cfg := Config{Dir: dir, Host: "127.0.0.1", Port: 7777}
	if err := cfg.HardenPermissions(); err != nil {
		t.Fatalf("HardenPermissions() error = %v", err)
	}

	assertMode(t, dir, dirPerm)
	assertMode(t, statePath, filePerm)
	assertMode(t, logPath, filePerm)
}

// TestHardenPermissionsMissingFilesIsNotAnError confirms a --dir with no
// state/log file yet (e.g. before the daemon has ever written one) is not
// an error -- there's nothing to tighten yet.
func TestHardenPermissionsMissingFilesIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Dir: dir, Host: "127.0.0.1", Port: 7777}
	if err := cfg.HardenPermissions(); err != nil {
		t.Fatalf("HardenPermissions() error = %v, want nil for a dir with no state/log files yet", err)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("os.Stat(%s) error = %v", path, err)
	}
	if got := fi.Mode().Perm(); got != want {
		t.Errorf("mode of %s = %#o, want %#o", path, got, want)
	}
}
