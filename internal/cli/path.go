package cli

import (
	"io"

	"github.com/jedwards1230/scrim/internal/canvas"
)

// cmdPath implements `scrim path <id>`. It's a pure filesystem computation
// from --dir/SCRIM_DIR — it does not talk to the daemon and works whether
// or not the daemon is running or the canvas has been created yet, so it's
// deliberately the one verb that never self-starts.
func cmdPath(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("path", stderr)
	cf := registerCommonFlags(fs)
	if err := parseArgs(fs, args); err != nil {
		return 2
	}
	pos := fs.Args()
	if len(pos) != 1 {
		return usageError(stderr, "usage: scrim path <id>")
	}
	id := pos[0]
	if err := canvas.ValidateID(id); err != nil {
		errOut(stderr, err)
		return 2
	}

	cfg := cf.toConfig()
	outln(stdout, canvas.Dir(cfg.CanvasesDir(), id))
	return 0
}
