package cli

import "io"

// cmdLink implements `scrim link [<id>]`. It self-starts the daemon and
// prints the URL for a canvas (or the dashboard, with no id), using the
// exact same resolution logic as cmdOpen (resolveAndPrintURLs) -- but link
// is permanently print-only: no flag or environment variable can ever make
// it launch a browser. It's the verb agents (and, eventually, an agent
// skill built on top of scrim) should reach for by default, since open's
// browser auto-launch is opt-in specifically for a human operator asking
// for it -- an agent should never be the one flipping that switch on
// someone else's machine.
func cmdLink(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("link", stderr)
	cf := registerCommonFlags(fs)
	if err := parseArgs(fs, args); err != nil {
		return exitForParseErr(err)
	}
	pos := fs.Args()
	if len(pos) > 1 {
		return usageError(stderr, "usage: scrim link [<id>]")
	}

	id := ""
	if len(pos) == 1 {
		id = pos[0]
	}

	cfg := cf.toConfig()
	_, code, ok := resolveAndPrintURLs(cfg, id, stdout, stderr)
	if !ok {
		return code
	}
	return 0
}
