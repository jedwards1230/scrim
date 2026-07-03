package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/jedwards1230/scrim/internal/apiclient"
	"github.com/jedwards1230/scrim/internal/daemon"
)

// cmdAdd implements `scrim add <id> [--title T]`. It self-starts the daemon
// if needed, then creates the canvas via the control API.
func cmdAdd(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("add", stderr)
	cf := registerCommonFlags(fs)
	title := fs.String("title", "", "canvas title")
	if err := parseArgs(fs, args); err != nil {
		return 2
	}
	pos := fs.Args()
	if len(pos) != 1 {
		return usageError(stderr, "usage: scrim add <id> [--title T]")
	}
	id := pos[0]

	cfg := cf.toConfig()
	st, err := daemon.Ensure(cfg)
	if err != nil {
		errOut(stderr, err)
		return 1
	}

	client := apiclient.NewWithToken(fmt.Sprintf("http://%s:%d", st.Host, st.Port), st.Token)
	info, err := client.CreateCanvas(context.Background(), id, *title)
	if err != nil {
		errOut(stderr, err)
		return 1
	}

	outln(stdout, info.Dir)
	printURLLines(stdout, st.Host, info.URL)
	return 0
}
