// Package config resolves scrim's runtime configuration (directory, host,
// port, idle timeout, auth mode) from flags, environment variables, and
// defaults, and derives the filesystem paths that hang off the base
// directory.
package config

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultPort is the port the daemon listens on when none is given.
	DefaultPort = 7777
	// DefaultHost is the host the daemon binds to when none is given.
	DefaultHost = "127.0.0.1"
	// DefaultIdleTimeout is how long the daemon waits for activity before
	// exiting when none is given.
	DefaultIdleTimeout = 30 * time.Minute
	// DefaultDirName is the base directory name under the user's home
	// directory used when neither --dir nor SCRIM_DIR is set.
	DefaultDirName = ".scrim"
)

// Config is scrim's resolved runtime configuration.
type Config struct {
	Dir         string
	Host        string
	Port        int
	IdleTimeout time.Duration
	NoAuth      bool
}

// Default returns the configuration that would be used with no flags and no
// environment variables set.
func Default() Config {
	return Config{
		Dir:         defaultDir(),
		Host:        DefaultHost,
		Port:        DefaultPort,
		IdleTimeout: DefaultIdleTimeout,
		NoAuth:      false,
	}
}

// FromEnv returns the configuration with each field defaulted from its
// SCRIM_* environment variable when set, falling back to the built-in
// default otherwise. Malformed environment values are ignored (the built-in
// default is used) rather than causing an error, since env-sourced defaults
// are always overridable by explicit flags.
func FromEnv() Config {
	cfg := Default()
	if v, ok := os.LookupEnv("SCRIM_DIR"); ok && v != "" {
		cfg.Dir = v
	}
	if v, ok := os.LookupEnv("SCRIM_HOST"); ok && v != "" {
		cfg.Host = v
	}
	if v, ok := os.LookupEnv("SCRIM_PORT"); ok && v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			cfg.Port = p
		}
	}
	if v, ok := os.LookupEnv("SCRIM_IDLE_TIMEOUT"); ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.IdleTimeout = d
		}
	}
	if v, ok := os.LookupEnv("SCRIM_NO_AUTH"); ok && v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.NoAuth = b
		}
	}
	cfg.Dir = ExpandHome(cfg.Dir)
	return cfg
}

// ExpandHome replaces a leading "~" (or "~/...") in path with the current
// user's home directory. Paths that don't start with "~" are returned
// unchanged.
func ExpandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~/"))
}

func defaultDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return DefaultDirName
	}
	return filepath.Join(home, DefaultDirName)
}

// StateFilePath is the path to the daemon's state file.
func (c Config) StateFilePath() string { return filepath.Join(c.Dir, "daemon.json") }

// LockFilePath is the path to the spawn-coordination lockfile.
func (c Config) LockFilePath() string { return filepath.Join(c.Dir, "daemon.lock") }

// LogFilePath is the path the spawned daemon's stdout/stderr are redirected
// to.
func (c Config) LogFilePath() string { return filepath.Join(c.Dir, "daemon.log") }

// CanvasesDir is the directory canvases live under.
func (c Config) CanvasesDir() string { return filepath.Join(c.Dir, "canvases") }

// BaseURL is the daemon's HTTP base URL.
func (c Config) BaseURL() string {
	return "http://" + net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
}
