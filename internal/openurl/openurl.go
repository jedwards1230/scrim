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
		// Deliberately not "cmd /c start <url>": Windows has no true argv --
		// exec.Command builds one command-line string that CreateProcess
		// hands to the child, quoted for a CommandLineToArgvW-style parser.
		// cmd.exe's "/c" tail does NOT go through that parser -- it does its
		// own bespoke scan for redirection/chaining metacharacters ("&",
		// "&&", "|", ">", "<", ...) independent of how the parent quoted the
		// argument. So a rawURL containing "&" could still be reinterpreted
		// by cmd.exe as a command separator, regardless of argv quoting.
		// rundll32 sidesteps this entirely: it's a normal Win32 program, so
		// rawURL arrives as a single argv element with no shell in between
		// to reparse it, and url.dll's FileProtocolHandler opens it in the
		// default browser the same way "start" would have.
		return "rundll32", []string{"url.dll,FileProtocolHandler", rawURL}, nil
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
