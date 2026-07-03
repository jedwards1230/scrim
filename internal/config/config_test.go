package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestFromEnv(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want Config
	}{
		{
			name: "no env uses defaults",
			env:  nil,
			want: Default(),
		},
		{
			name: "valid overrides applied",
			env: map[string]string{
				"SCRIM_DIR":          "/tmp/scrim-test",
				"SCRIM_HOST":         "0.0.0.0",
				"SCRIM_PORT":         "9999",
				"SCRIM_IDLE_TIMEOUT": "5s",
				"SCRIM_NO_AUTH":      "true",
				"SCRIM_NO_MDNS":      "true",
			},
			want: Config{
				Dir:         "/tmp/scrim-test",
				Host:        "0.0.0.0",
				Port:        9999,
				IdleTimeout: 5 * time.Second,
				NoAuth:      true,
				NoMDNS:      true,
			},
		},
		{
			name: "malformed values fall back to defaults",
			env: map[string]string{
				"SCRIM_PORT":         "not-a-number",
				"SCRIM_IDLE_TIMEOUT": "not-a-duration",
				"SCRIM_NO_AUTH":      "not-a-bool",
				"SCRIM_NO_MDNS":      "not-a-bool",
			},
			want: Default(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, k := range []string{"SCRIM_DIR", "SCRIM_HOST", "SCRIM_PORT", "SCRIM_IDLE_TIMEOUT", "SCRIM_NO_AUTH", "SCRIM_NO_MDNS"} {
				t.Setenv(k, "")
			}
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			got := FromEnv()
			if got != tt.want {
				t.Errorf("FromEnv() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestExpandHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"tilde only", "~", home},
		{"tilde slash path", "~/.scrim", filepath.Join(home, ".scrim")},
		{"no tilde", "/tmp/scrim", "/tmp/scrim"},
		{"tilde mid-path unchanged", "/tmp/~/scrim", "/tmp/~/scrim"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExpandHome(tt.in); got != tt.want {
				t.Errorf("ExpandHome(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestResolveDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	origWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd() error = %v", err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("os.Chdir() error = %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(origWd); err != nil {
			t.Fatalf("os.Chdir() restore error = %v", err)
		}
	})
	// Resolve the test's own cwd through the same os.Getwd path ResolveDir's
	// filepath.Abs uses internally, so this comparison isn't thrown off by a
	// symlinked temp dir (e.g. macOS's /tmp -> /private/tmp).
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd() error = %v", err)
	}

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"already absolute", "/tmp/scrim", "/tmp/scrim"},
		{"tilde expands and is already absolute", "~/.scrim", filepath.Join(home, ".scrim")},
		{"relative dir resolves against cwd", "relative-dir", filepath.Join(cwd, "relative-dir")},
		{"dot resolves to cwd", ".", cwd},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolveDir(tt.in); got != tt.want {
				t.Errorf("ResolveDir(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestConfigPaths(t *testing.T) {
	cfg := Config{Dir: "/base", Host: "127.0.0.1", Port: 7777}

	if got, want := cfg.StateFilePath(), filepath.Join("/base", "daemon.json"); got != want {
		t.Errorf("StateFilePath() = %q, want %q", got, want)
	}
	if got, want := cfg.LockFilePath(), filepath.Join("/base", "daemon.lock"); got != want {
		t.Errorf("LockFilePath() = %q, want %q", got, want)
	}
	if got, want := cfg.LogFilePath(), filepath.Join("/base", "daemon.log"); got != want {
		t.Errorf("LogFilePath() = %q, want %q", got, want)
	}
	if got, want := cfg.CanvasesDir(), filepath.Join("/base", "canvases"); got != want {
		t.Errorf("CanvasesDir() = %q, want %q", got, want)
	}
	if got, want := cfg.BaseURL(), "http://127.0.0.1:7777"; got != want {
		t.Errorf("BaseURL() = %q, want %q", got, want)
	}
}

// TestHardenPermissionsFreshDir confirms a brand-new --dir comes up at
// owner-only permissions with no state/log files yet to tighten.
func TestHardenPermissionsFreshDir(t *testing.T) {
	skipOnWindows(t)
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
	skipOnWindows(t)
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
	skipOnWindows(t)
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

func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits don't apply on windows")
	}
}
