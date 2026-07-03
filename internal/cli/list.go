package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/jedwards1230/scrim/internal/apiclient"
	"github.com/jedwards1230/scrim/internal/daemon"
)

// cmdList implements `scrim list`. It self-starts the daemon if needed,
// then lists canvases via the control API.
func cmdList(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("list", stderr)
	cf := registerCommonFlags(fs)
	if err := parseArgs(fs, args); err != nil {
		return 2
	}

	cfg := cf.toConfig()
	st, err := daemon.Ensure(cfg)
	if err != nil {
		errOut(stderr, err)
		return 1
	}

	client := apiclient.New(fmt.Sprintf("http://%s:%d", st.Host, st.Port))
	canvases, err := client.ListCanvases(context.Background())
	if err != nil {
		errOut(stderr, err)
		return 1
	}

	if len(canvases) == 0 {
		outln(stdout, "no canvases")
		return 0
	}
	for _, c := range canvases {
		title := c.Title
		if title == "" {
			title = "-"
		}
		outf(stdout, "%s\t%s\t%s\n", c.ID, title, c.URL)
	}
	return 0
}
