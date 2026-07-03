package cli

import (
	"io"

	"github.com/jedwards1230/scrim/internal/daemon"
)

// cmdStop implements `scrim stop`. It does not self-start — there's nothing
// to stop if nothing is running, and it reports that cleanly.
func cmdStop(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("stop", stderr)
	cf := registerCommonFlags(fs)
	if err := parseArgs(fs, args); err != nil {
		return 2
	}

	cfg := cf.toConfig()
	found, err := daemon.Stop(cfg)
	if !found {
		outln(stdout, "no daemon running")
		return 0
	}
	if err != nil {
		errOut(stderr, err)
		return 1
	}
	outln(stdout, "stopped")
	return 0
}
