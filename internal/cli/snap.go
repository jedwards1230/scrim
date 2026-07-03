package cli

import (
	"io"

	"github.com/jedwards1230/scrim/internal/canvas"
	"github.com/jedwards1230/scrim/internal/snapshot"
)

// cmdSnap implements `scrim snap <id> [--label L]`. It's a pure filesystem
// copy from the canvas directory to a new timestamped directory under the
// versions dir -- like path/rm's fallback, this doesn't need a running
// daemon, so it deliberately never self-starts one.
func cmdSnap(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("snap", stderr)
	cf := registerCommonFlags(fs)
	label := fs.String("label", "", "snapshot label (optional; appended to the timestamp)")
	if err := parseArgs(fs, args); err != nil {
		return exitForParseErr(err)
	}
	pos := fs.Args()
	if len(pos) != 1 {
		return usageError(stderr, "usage: scrim snap <id> [--label L]")
	}
	id := pos[0]
	if err := canvas.ValidateID(id); err != nil {
		errOut(stderr, err)
		return 2
	}

	cfg := cf.toConfig()
	entry, err := snapshot.Create(canvas.Dir(cfg.CanvasesDir(), id), cfg.VersionsDir(), id, *label)
	if err != nil {
		errOut(stderr, err)
		return 1
	}

	outf(stdout, "snapshot %s created for %s\n", entry.Name, id)
	outln(stdout, entry.Dir)
	return 0
}
