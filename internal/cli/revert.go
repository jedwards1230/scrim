package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/jedwards1230/scrim/internal/canvas"
	"github.com/jedwards1230/scrim/internal/snapshot"
)

// cmdRevert implements `scrim revert <id> [<snapshot>]`. It replaces the
// canvas directory's current contents with a snapshot's -- entirely, not
// merged -- defaulting to the latest snapshot for id when none is named.
// Before doing so, it takes its own labeled "prerevert" snapshot of
// whatever is there beforehand, so a revert is itself undoable via another
// revert (see snapshot.Create) rather than being a one-way door. The
// "latest" snapshot is resolved *before* that safety snapshot is taken --
// otherwise the safety snapshot itself would immediately become "latest"
// and a bare `scrim revert <id>` would just revert the canvas to its own
// current state. Like snap/snaps, this is a pure filesystem operation and
// never self-starts the daemon.
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
	canvasDir := canvas.Dir(cfg.CanvasesDir(), id)
	versionsDir := cfg.VersionsDir()

	target := name
	if target == "" {
		latest, ok, err := snapshot.Latest(versionsDir, id)
		if err != nil {
			errOut(stderr, err)
			return 1
		}
		if !ok {
			errOut(stderr, fmt.Errorf("no snapshots for canvas %s", id))
			return 1
		}
		target = latest.Name
	}

	if fi, statErr := os.Stat(canvasDir); statErr == nil && fi.IsDir() {
		if _, err := snapshot.Create(canvasDir, versionsDir, id, "prerevert"); err != nil {
			errOut(stderr, err)
			return 1
		}
	}

	entry, err := snapshot.Revert(canvasDir, versionsDir, id, target)
	if err != nil {
		errOut(stderr, err)
		return 1
	}

	outf(stdout, "reverted %s to snapshot %s (pre-revert state saved as a new snapshot)\n", id, entry.Name)
	return 0
}
