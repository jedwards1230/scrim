package config

import (
	"path/filepath"
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
			},
			want: Config{
				Dir:         "/tmp/scrim-test",
				Host:        "0.0.0.0",
				Port:        9999,
				IdleTimeout: 5 * time.Second,
				NoAuth:      true,
			},
		},
		{
			name: "malformed values fall back to defaults",
			env: map[string]string{
				"SCRIM_PORT":         "not-a-number",
				"SCRIM_IDLE_TIMEOUT": "not-a-duration",
				"SCRIM_NO_AUTH":      "not-a-bool",
			},
			want: Default(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, k := range []string{"SCRIM_DIR", "SCRIM_HOST", "SCRIM_PORT", "SCRIM_IDLE_TIMEOUT", "SCRIM_NO_AUTH"} {
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
