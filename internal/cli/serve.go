package cli

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/jedwards1230/scrim/internal/server"
)

// cmdServe implements `scrim serve`: the daemon itself, run in the
// foreground. This is what self-start re-execs as a detached process, and
// what a user/container/systemd unit can also run directly. It has no
// self-start logic of its own — it just runs the HTTP server, filesystem
// watcher, and idle reaper until asked to stop (idle timeout, /api/stop, or
// a signal).
func cmdServe(args []string, _, stderr io.Writer) int {
	fs := newFlagSet("serve", stderr)
	cf := registerCommonFlags(fs)
	if err := parseArgs(fs, args); err != nil {
		return 2
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	cfg := cf.toConfig()
	srv := server.New(cfg)
	if err := srv.Run(ctx); err != nil {
		errOut(stderr, err)
		return 1
	}
	return 0
}
