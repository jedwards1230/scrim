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
		return exitForParseErr(err)
	}

	cfg := cf.toConfig()
	st, err := daemon.Ensure(cfg)
	if err != nil {
		errOut(stderr, err)
		return 1
	}

	client := apiclient.NewWithToken(fmt.Sprintf("http://%s:%d", st.Host, st.Port), st.Token)
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
		lines := urlLines(st.Host, c.URL)
		outf(stdout, "%s\t%s\t%s\n", c.ID, title, lines[0])
		// When mDNS is active there's a second (fallback, plain host:port)
		// line — print it indented under the same row rather than widening
		// the tab-separated columns for every canvas.
		for _, fallback := range lines[1:] {
			outf(stdout, "\t\t%s\n", fallback)
		}
	}
	return 0
}
