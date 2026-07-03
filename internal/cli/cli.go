// Package cli implements scrim's verb parsing and dispatch: add, path, list,
// open, rm, status, stop, and serve. Each verb is a thin wrapper that either
// talks to a running daemon over its local HTTP API (self-starting it first
// if needed) or, for path/rm's fallback/status/stop, works directly against
// the filesystem/daemon state.
package cli

import (
	"io"

	"github.com/jedwards1230/scrim/internal/version"
)

const usage = `scrim — projection surface for coding agents

Usage:
  scrim <verb> [args]

Verbs:
  add <id> [--title T]   Register a canvas (self-starts the daemon)
  path <id>               Print the filesystem path for a canvas
  list                    List registered canvases (self-starts the daemon)
  open [<id>]              Open a canvas or the dashboard in your browser (self-starts the daemon)
  rm <id>                 Remove a canvas
  status                  Show daemon status (does not self-start)
  stop                    Stop the daemon (does not self-start)
  serve                   Run the daemon in the foreground

Flags (all verbs):
  --dir DIR              Directory for canvases + daemon state (env SCRIM_DIR, default ~/.scrim)
  --host HOST            Host the daemon binds to (env SCRIM_HOST, default 127.0.0.1)
  --port PORT            Port the daemon listens on (env SCRIM_PORT, default 7777)
  --idle-timeout DUR     Idle time before the daemon exits (env SCRIM_IDLE_TIMEOUT, default 30m)
                         0 or negative disables idle exit entirely (the daemon
                         only stops via "scrim stop" or a signal)
  --no-auth              Disable the local auth token (env SCRIM_NO_AUTH)

  -h, --help             Show this help
  -v, --version          Show version
`

// Run parses argv (os.Args[1:]) and dispatches to the requested verb,
// returning a process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		outf(stderr, "%s", usage)
		return 2
	}

	verb := args[0]
	rest := args[1:]

	switch verb {
	case "add":
		return cmdAdd(rest, stdout, stderr)
	case "path":
		return cmdPath(rest, stdout, stderr)
	case "list":
		return cmdList(rest, stdout, stderr)
	case "open":
		return cmdOpen(rest, stdout, stderr)
	case "rm":
		return cmdRm(rest, stdout, stderr)
	case "status":
		return cmdStatus(rest, stdout, stderr)
	case "stop":
		return cmdStop(rest, stdout, stderr)
	case "serve":
		return cmdServe(rest, stdout, stderr)
	case "--version", "-v", "version":
		outln(stdout, "scrim", version.Info())
		return 0
	case "--help", "-h", "help":
		outf(stdout, "%s", usage)
		return 0
	default:
		outf(stderr, "scrim: unknown verb %q\n\n", verb)
		outf(stderr, "%s", usage)
		return 2
	}
}
