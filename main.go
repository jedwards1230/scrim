// Command scrim is a self-starting daemon that serves agent-authored HTML
// canvases with live reload, viewed by a human in a browser. See CLAUDE.md
// for the architecture.
package main

import (
	"os"

	"github.com/jedwards1230/scrim/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
