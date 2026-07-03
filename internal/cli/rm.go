package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/jedwards1230/scrim/internal/apiclient"
	"github.com/jedwards1230/scrim/internal/canvas"
	"github.com/jedwards1230/scrim/internal/daemon"
)

// cmdRm implements `scrim rm <id>`. It does not self-start the daemon:
// deleting a directory doesn't need a running server, so if none is found
// it just removes the canvas directory directly.
func cmdRm(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("rm", stderr)
	cf := registerCommonFlags(fs)
	if err := parseArgs(fs, args); err != nil {
		return exitForParseErr(err)
	}
	pos := fs.Args()
	if len(pos) != 1 {
		return usageError(stderr, "usage: scrim rm <id>")
	}
	id := pos[0]
	if err := canvas.ValidateID(id); err != nil {
		errOut(stderr, err)
		return 2
	}

	cfg := cf.toConfig()
	if st, ok := daemon.TryLoadHealthy(cfg); ok {
		client := apiclient.NewWithToken(fmt.Sprintf("http://%s:%d", st.Host, st.Port), st.Token)
		if err := client.DeleteCanvas(context.Background(), id); err != nil {
			errOut(stderr, err)
			return 1
		}
	} else if err := canvas.Delete(cfg.CanvasesDir(), cfg.MetaDir(), id); err != nil {
		errOut(stderr, err)
		return 1
	}

	outf(stdout, "removed %s\n", id)
	return 0
}
