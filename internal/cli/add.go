package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/jedwards1230/scrim/internal/apiclient"
	"github.com/jedwards1230/scrim/internal/daemon"
)

// cmdAdd implements `scrim add <id> [--title T] [--desc D] [--icon I]`. It
// self-starts the daemon if needed, then creates the canvas via the control
// API. Leaving --icon unset lets the daemon derive a deterministic default
// glyph (and accent color) from the canvas ID.
func cmdAdd(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("add", stderr)
	cf := registerCommonFlags(fs)
	title := fs.String("title", "", "canvas title")
	desc := fs.String("desc", "", "canvas description")
	icon := fs.String("icon", "", "canvas icon (an emoji); derived deterministically from the id when omitted")
	if err := parseArgs(fs, args); err != nil {
		return exitForParseErr(err)
	}
	pos := fs.Args()
	if len(pos) != 1 {
		return usageError(stderr, "usage: scrim add <id> [--title T] [--desc D] [--icon I]")
	}
	id := pos[0]

	cfg := cf.toConfig()
	st, err := daemon.Ensure(cfg)
	if err != nil {
		errOut(stderr, err)
		return 1
	}

	client := apiclient.NewWithToken(fmt.Sprintf("http://%s:%d", st.Host, st.Port), st.Token)
	info, err := client.CreateCanvas(context.Background(), id, *title, *desc, *icon)
	if err != nil {
		errOut(stderr, err)
		return 1
	}

	outln(stdout, info.Dir)
	printURLLines(stdout, urlLines(st.Host, info.URL))
	return 0
}
