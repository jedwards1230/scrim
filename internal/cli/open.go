package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/jedwards1230/scrim/internal/apiclient"
	"github.com/jedwards1230/scrim/internal/canvas"
	"github.com/jedwards1230/scrim/internal/daemon"
	"github.com/jedwards1230/scrim/internal/openurl"
)

// cmdOpen implements `scrim open [<id>]`. It self-starts the daemon if
// needed, prints the URL for a canvas (or the dashboard, with no id), and
// launches it in the platform's default browser. The printed URL is always
// there as a fallback, whether or not the auto-open succeeds -- a failed or
// unsupported auto-open is reported as a notice, not a command failure.
func cmdOpen(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("open", stderr)
	cf := registerCommonFlags(fs)
	if err := parseArgs(fs, args); err != nil {
		return exitForParseErr(err)
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
	apiBaseURL := fmt.Sprintf("http://%s:%d", st.Host, st.Port)

	if len(pos) == 0 {
		url := baseURLFor(st, "/")
		printURLLines(stdout, st.Host, url)
		openBrowser(url, stderr)
		return 0
	}

	id := pos[0]
	if err := canvas.ValidateID(id); err != nil {
		errOut(stderr, err)
		return 2
	}

	client := apiclient.NewWithToken(apiBaseURL, st.Token)
	canvases, err := client.ListCanvases(context.Background())
	if err != nil {
		errOut(stderr, err)
		return 1
	}
	for _, c := range canvases {
		if c.ID == id {
			printURLLines(stdout, st.Host, c.URL)
			openBrowser(c.URL, stderr)
			return 0
		}
	}
	outf(stderr, "error: canvas %q not found\n", id)
	return 1
}

// openBrowser launches url in the platform's default browser, printing a
// one-line notice to stderr on failure. It never affects cmdOpen's exit
// code -- the URL is already on stdout, so a failed or unsupported auto-open
// is a nice-to-have that didn't pan out, not an error.
func openBrowser(url string, stderr io.Writer) {
	if err := openurl.Open(url); err != nil {
		outf(stderr, "notice: could not open a browser automatically (%v); use the URL above\n", err)
	}
}
