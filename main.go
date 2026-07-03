// Command scrim is a self-starting daemon that serves agent-authored HTML
// canvases with live reload, viewed by a human in a browser.
//
// This is a Phase 1 scaffold: the CLI verbs (add, path, list, open, rm,
// status, stop, serve) are not implemented yet. See CLAUDE.md for the
// planned architecture.
package main

import (
	"fmt"
	"os"

	"github.com/jedwards1230/scrim/internal/version"
)

const usage = `scrim — projection surface for coding agents

Usage:
  scrim <verb> [args]

Verbs (planned, not yet implemented):
  add <id>      Register a canvas
  path <id>     Print the filesystem path for a canvas
  list          List registered canvases
  open [<id>]   Open a canvas (or the dashboard) in a browser
  rm <id>       Remove a canvas
  status        Show daemon status
  stop          Stop the daemon
  serve         Run the daemon in the foreground

Flags:
  -h, --help     Show this help
  -v, --version  Show version
`

func main() {
	if len(os.Args) == 2 {
		switch os.Args[1] {
		case "--version", "-v", "version":
			fmt.Println("scrim", version.Info())
			return
		case "--help", "-h", "help":
			fmt.Print(usage)
			return
		}
	}

	fmt.Fprintf(os.Stderr, "scrim %s — not yet implemented\n", version.Short())
	fmt.Fprint(os.Stderr, usage)
	os.Exit(1)
}
