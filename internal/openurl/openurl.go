// Package openurl launches the user's default browser at a given URL, using
// the appropriate command for the current platform.
package openurl

import (
	"fmt"
	"os/exec"
	"runtime"
)

// commandFor returns the external command and arguments used to open rawURL
// in the default browser on goos. It's a pure, side-effect-free function
// (no exec, no I/O) precisely so the platform-selection logic can be unit
// tested without actually launching a browser -- something that can't be
// meaningfully verified in CI anyway (no browser, no display).
func commandFor(goos, rawURL string) (name string, args []string, err error) {
	switch goos {
	case "darwin":
		return "open", []string{rawURL}, nil
	case "linux":
		return "xdg-open", []string{rawURL}, nil
	case "windows":
		// "start" is a cmd.exe builtin, not its own executable, so it must be
		// invoked via "cmd /c". "start"'s first argument is treated as the
		// new window's title when present -- passing an empty string there
		// keeps rawURL (which may contain "&" or other characters cmd.exe
		// would otherwise try to interpret) from being misread as that
		// title instead of the thing to open.
		return "cmd", []string{"/c", "start", "", rawURL}, nil
	default:
		return "", nil, fmt.Errorf("don't know how to open a browser on %s", goos)
	}
}

// Open launches rawURL in the current platform's default browser. Callers
// should treat a returned error as non-fatal: the caller always has the URL
// itself as a fallback to print/hand to the user.
func Open(rawURL string) error {
	name, args, err := commandFor(runtime.GOOS, rawURL)
	if err != nil {
		return err
	}
	cmd := exec.Command(name, args...) //nolint:gosec // name/args come from commandFor's fixed per-platform table; rawURL is passed as a single argv element (never through a shell), so it can't inject additional arguments or commands
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("running %s: %w", name, err)
	}
	return nil
}
