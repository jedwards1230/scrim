package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/jedwards1230/scrim/internal/apiclient"
	"github.com/jedwards1230/scrim/internal/daemon"
)

// cmdStatus implements `scrim status`. Checking status *is* the daemon
// health-check, so this verb deliberately does not self-start — it just
// reports what it finds, including "no daemon running".
func cmdStatus(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("status", stderr)
	cf := registerCommonFlags(fs)
	if err := parseArgs(fs, args); err != nil {
		return 2
	}

	cfg := cf.toConfig()
	st, ok := daemon.TryLoadHealthy(cfg)
	if !ok {
		outln(stdout, "no daemon running")
		return 0
	}

	client := apiclient.NewWithToken(fmt.Sprintf("http://%s:%d", st.Host, st.Port), st.Token)
	resp, err := client.Status(context.Background())
	if err != nil {
		errOut(stderr, err)
		return 1
	}

	outf(stdout, "pid:          %d\n", resp.PID)
	outf(stdout, "host:         %s\n", resp.Host)
	outf(stdout, "port:         %d\n", resp.Port)
	outf(stdout, "version:      %s\n", resp.Version)
	outf(stdout, "uptime:       %s\n", time.Duration(resp.UptimeSeconds*float64(time.Second)).Round(time.Second))
	outf(stdout, "canvases:     %d\n", resp.CanvasCount)
	outf(stdout, "sse clients:  %d\n", resp.SSEClients)
	outf(stdout, "idle for:     %s\n", time.Duration(resp.IdleSeconds*float64(time.Second)).Round(time.Second))
	outf(stdout, "idle timeout: %s\n", time.Duration(resp.IdleTimeoutSeconds*float64(time.Second)).Round(time.Second))

	lines := urlLines(st.Host, baseURLFor(st, "/"))
	outf(stdout, "url:          %s\n", lines[0])
	for _, fallback := range lines[1:] {
		outf(stdout, "url (plain):  %s\n", fallback)
	}
	return 0
}
