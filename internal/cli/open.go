package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/jedwards1230/scrim/internal/apiclient"
	"github.com/jedwards1230/scrim/internal/canvas"
	"github.com/jedwards1230/scrim/internal/daemon"
)

// cmdOpen implements `scrim open [<id>]`. It self-starts the daemon if
// needed and prints the URL for a canvas (or the dashboard, with no id).
// Actually opening a browser is Phase 4 — this phase only prints the URL.
func cmdOpen(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("open", stderr)
	cf := registerCommonFlags(fs)
	if err := parseArgs(fs, args); err != nil {
		return 2
	}
	pos := fs.Args()
	if len(pos) > 1 {
		return usageError(stderr, "usage: scrim open [<id>]")
	}

	cfg := cf.toConfig()
	st, err := daemon.Ensure(cfg)
	if err != nil {
		errOut(stderr, err)
		return 1
	}
	baseURL := fmt.Sprintf("http://%s:%d", st.Host, st.Port)

	if len(pos) == 0 {
		outln(stdout, baseURL+"/")
		return 0
	}

	id := pos[0]
	if err := canvas.ValidateID(id); err != nil {
		errOut(stderr, err)
		return 2
	}

	client := apiclient.New(baseURL)
	canvases, err := client.ListCanvases(context.Background())
	if err != nil {
		errOut(stderr, err)
		return 1
	}
	for _, c := range canvases {
		if c.ID == id {
			outln(stdout, c.URL)
			return 0
		}
	}
	outf(stderr, "error: canvas %q not found\n", id)
	return 1
}
