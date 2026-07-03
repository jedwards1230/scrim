package cli

import (
	"io"
	"time"

	"github.com/jedwards1230/scrim/internal/canvas"
	"github.com/jedwards1230/scrim/internal/snapshot"
)

// cmdSnaps implements `scrim snaps <id>`. A pure filesystem read -- like
// snap/revert, it never self-starts the daemon.
func cmdSnaps(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("snaps", stderr)
	cf := registerCommonFlags(fs)
	if err := parseArgs(fs, args); err != nil {
		return exitForParseErr(err)
	}
	pos := fs.Args()
	if len(pos) != 1 {
		return usageError(stderr, "usage: scrim snaps <id>")
	}
	id := pos[0]
	if err := canvas.ValidateID(id); err != nil {
		errOut(stderr, err)
		return 2
	}

	cfg := cf.toConfig()
	entries, err := snapshot.List(cfg.VersionsDir(), id)
	if err != nil {
		errOut(stderr, err)
		return 1
	}
	if len(entries) == 0 {
		outln(stdout, "no snapshots")
		return 0
	}

	// snapshot.List returns oldest-first (its Name sorts chronologically);
	// print newest-first since that's what's most often wanted at a glance
	// (e.g. picking the default `scrim revert` would restore).
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		label := e.Label
		if label == "" {
			label = "-"
		}
		outf(stdout, "%s\t%s\t%s\n", e.Name, e.Timestamp.Format(time.RFC3339), label)
	}
	return 0
}
