package cli

import (
	"io"

	"github.com/jedwards1230/scrim/internal/canvas"
	"github.com/jedwards1230/scrim/internal/snapshot"
)

// cmdRevert implements `scrim revert <id> [<snapshot>]`. It replaces the
// canvas directory's current contents with a snapshot's -- entirely, not
// merged -- defaulting to the latest snapshot for id when none is named.
// The resolve-target -> prerevert-safety-snapshot -> revert protocol
// (including why the target is resolved and verified BEFORE the safety
// snapshot is taken) lives in snapshot.RevertWithSafety, shared with the
// MCP local backend and the hub machine API. Like snap/snaps, this is a
// pure filesystem operation and never self-starts the daemon.
func cmdRevert(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("revert", stderr)
	cf := registerCommonFlags(fs)
	if err := parseArgs(fs, args); err != nil {
		return exitForParseErr(err)
	}
	pos := fs.Args()
	if len(pos) < 1 || len(pos) > 2 {
		return usageError(stderr, "usage: scrim revert <id> [<snapshot>]")
	}
	id := pos[0]
	if err := canvas.ValidateID(id); err != nil {
		errOut(stderr, err)
		return 2
	}
	name := ""
	if len(pos) == 2 {
		name = pos[1]
	}

	cfg := cf.toConfig()

	entry, err := snapshot.RevertWithSafety(canvas.Dir(cfg.CanvasesDir(), id), cfg.VersionsDir(), id, name)
	if err != nil {
		errOut(stderr, err)
		return 1
	}

	outf(stdout, "reverted %s to snapshot %s (pre-revert state saved as a new snapshot)\n", id, entry.Name)
	return 0
}
