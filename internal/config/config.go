// Package config resolves scrim's runtime configuration (directory, host,
// port, idle timeout, auth mode) from flags, environment variables, and
// defaults, and derives the filesystem paths that hang off the base
// directory.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jedwards1230/scrim/internal/logging"
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
	// NoMDNS disables the daemon's mDNS ("scrim.local") advertisement even
	// when Host binds beyond loopback. It decouples "bound beyond loopback"
	// from "advertises on mDNS": a daemon can be reachable on the LAN
	// without broadcasting its presence to it.
	NoMDNS bool
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
		NoMDNS:      false,
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
	if v, ok := os.LookupEnv("SCRIM_NO_MDNS"); ok && v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.NoMDNS = b
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

// ResolveDir expands a leading "~" in dir (see ExpandHome) and then resolves
// the result to an absolute path. A relative --dir must be made absolute
// before it's ever handed to a self-started daemon: the CLI process and the
// detached daemon process it spawns could otherwise resolve the same
// relative path against different working directories (or, less exotically,
// a later CLI invocation from a different shell/cwd would silently target a
// different directory than the one that started the daemon). Like
// ExpandHome, this never fails a flag default -- a path that can't be
// resolved (e.g. os.Getwd fails) is returned unchanged rather than erroring.
func ResolveDir(dir string) string {
	dir = ExpandHome(dir)
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	return abs
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

// MetaDir is the directory external per-canvas metadata (title,
// description, custom icon) is stored under, keyed by canvas ID. It is
// deliberately outside CanvasesDir(): anything under the canvases dir is
// servable/watchable by the daemon, and metadata must be neither.
func (c Config) MetaDir() string { return filepath.Join(c.Dir, "meta") }

// VersionsDir is the directory canvas snapshots (see `scrim snap`) are
// stored under, keyed by canvas ID.
func (c Config) VersionsDir() string { return filepath.Join(c.Dir, "versions") }

// BaseURL is the daemon's HTTP base URL.
func (c Config) BaseURL() string {
	return "http://" + net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
}

const (
	// dirPerm is the permission enforced on c.Dir: owner-only
	// read/write/traverse. Nothing under it (the state file, the log file,
	// canvas contents) is reachable by another user on the same host once
	// this holds, regardless of those entries' own individual permissions.
	dirPerm = 0o700
	// filePerm is the permission enforced on the state file and log file:
	// owner-only read/write.
	filePerm = 0o600
)

// HardenPermissions ensures c.Dir exists and is owner-only (dirPerm), and
// tightens the state file and log file to owner-only (filePerm) if they
// already exist. It's idempotent and meant to be called on every daemon
// startup/self-start, whether c.Dir is brand new or was created by an
// older scrim version (or by hand, e.g. `mkdir ~/.scrim`) with looser
// permissions -- those don't get silently grandfathered in.
//
// This is a Unix-only guarantee -- see permissionHardeningSupported. On a
// platform where it doesn't hold, HardenPermissions still returns nil (there
// really is nothing more it can do), but logs a one-time warning rather than
// silently claiming success.
func (c Config) HardenPermissions() error {
	return c.hardenPermissionsForGOOS(runtime.GOOS)
}

// hardenPermissionsForGOOS is HardenPermissions's actual implementation,
// parameterized on goos so tests can exercise the Windows no-op path
// without needing to run on a real Windows host.
func (c Config) hardenPermissionsForGOOS(goos string) error {
	if !permissionHardeningSupported(goos) {
		warnPermissionHardeningUnsupported()
		return nil
	}
	if err := enforceDirPerm(c.Dir); err != nil {
		return err
	}
	if err := enforceFilePerm(c.StateFilePath()); err != nil {
		return err
	}
	return enforceFilePerm(c.LogFilePath())
}

// permissionHardeningSupported reports whether goos supports owner-only
// enforcement via os.Chmod the way enforceDirPerm/enforceFilePerm use it.
// It does not on Windows: os.Chmod there only toggles the
// FILE_ATTRIBUTE_READONLY flag (whether the file is writable at all) --
// there's no Unix-style "owner-only, unreadable by other accounts on the
// same host" primitive behind it, so the dirPerm/filePerm calls below would
// silently no-op rather than actually tightening anything.
func permissionHardeningSupported(goos string) bool {
	return goos != "windows"
}

var windowsPermissionWarnOnce sync.Once

// warnPermissionHardeningUnsupported logs, once per process, that owner-only
// permission hardening isn't available on this platform. HardenPermissions
// is called on every self-start check and daemon startup -- without the
// sync.Once guard, a single long-lived process could log this several
// times over instead of once.
func warnPermissionHardeningUnsupported() {
	windowsPermissionWarnOnce.Do(func() {
		logging.Error(logging.CategoryConfig, errPermissionHardeningUnsupported)
	})
}

// errPermissionHardeningUnsupported is a static, pre-scrubbed message (it
// carries no path or other request-derived text) describing the Windows
// gap in permission hardening -- see permissionHardeningSupported. Tracked
// as https://github.com/jedwards1230/scrim/issues/19.
var errPermissionHardeningUnsupported = errors.New(
	"owner-only permission hardening is unavailable on this platform: " +
		"--dir, the state file, and the log file are not being tightened " +
		"(tracked as scrim#19)",
)

// enforceDirPerm creates dir if missing and (re)sets its permission to
// dirPerm either way -- MkdirAll alone only applies a permission to a
// directory it actually creates, leaving a preexisting looser-permissioned
// directory untouched.
func enforceDirPerm(dir string) error {
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("creating dir %s: %w", dir, err)
	}
	if err := os.Chmod(dir, dirPerm); err != nil {
		return fmt.Errorf("tightening permissions on %s: %w", dir, err)
	}
	return nil
}

// enforceFilePerm tightens the permission of an existing file at path to
// filePerm. A missing file is not an error -- there's nothing to tighten
// yet, and whatever creates it is responsible for using filePerm from the
// start.
func enforceFilePerm(path string) error {
	if err := os.Chmod(path, filePerm); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("tightening permissions on %s: %w", path, err)
	}
	return nil
}
