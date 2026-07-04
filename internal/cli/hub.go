package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/jedwards1230/scrim/internal/config"
	"github.com/jedwards1230/scrim/internal/server"
)

// Hub-specific defaults, deliberately distinct from the default daemon's
// (config.Default*) so both can run on the same box: a separate data
// directory (never the default daemon's ~/.scrim) and a separate port.
const (
	defaultHubDirName = ".scrim-hub"
	defaultHubHost    = "0.0.0.0"
	defaultHubPort    = 7788
)

// defaultHubAllowCSV is the read allowlist used when --allow/SCRIM_HUB_ALLOW
// is unset: loopback only, on both address families -- a hub started with
// no explicit CIDR configuration is no more reachable than the default
// daemon is, until an operator deliberately widens it.
const defaultHubAllowCSV = "127.0.0.0/8,::1/128"

// cmdHub implements `scrim hub`: a scrim daemon in hub mode -- the same
// serving engine `scrim serve` runs, at its own data directory and port,
// with a push/read-token + CIDR gate (server.HubOptions) in place of the
// default daemon's capability-token auth (see internal/server/hubgate.go).
// It has no self-start logic of its own, same as cmdServe: an operator (or
// a container entrypoint) runs it directly and it blocks in the foreground
// until stopped.
//
// It deliberately does NOT use registerCommonFlags/commonFlags: the hub's
// defaults (data directory, host, port, mDNS, idle timeout) differ from the
// default daemon's on purpose, and reusing that flagset would either force
// the daemon's defaults onto the hub or vice versa.
func cmdHub(args []string, _, stderr io.Writer) int {
	fs := newFlagSet("hub", stderr)

	dataDir := fs.String("data", envOr("SCRIM_HUB_DATA", "~/"+defaultHubDirName), "directory for hub-served canvases + daemon state (env SCRIM_HUB_DATA)")
	host := fs.String("host", envOr("SCRIM_HOST", defaultHubHost), "host the hub binds to -- 0.0.0.0 by design; the CIDR allowlist is the read security (env SCRIM_HOST)")
	port := fs.Int("port", envIntOr("SCRIM_PORT", defaultHubPort), "port the hub listens on (env SCRIM_PORT)")
	pushToken := fs.String("push-token", os.Getenv("SCRIM_PUSH_TOKEN"), "REQUIRED: bearer token a push client must present (env SCRIM_PUSH_TOKEN)")
	readToken := fs.String("read-token", os.Getenv("SCRIM_READ_TOKEN"), "optional token additionally required to read, once the CIDR check passes (env SCRIM_READ_TOKEN)")
	allow := fs.String("allow", envOr("SCRIM_HUB_ALLOW", defaultHubAllowCSV), "comma-separated CIDR allowlist for reads (env SCRIM_HUB_ALLOW, default loopback-only)")
	idleTimeout := fs.Duration("idle-timeout", 0, "idle time before the hub exits; 0 or negative (the default) disables idle exit -- a hub is long-lived by design")
	noMDNS := fs.Bool("no-mdns", true, "don't advertise the hub over mDNS (default: no mDNS -- a hub isn't meant to be casually discovered)")

	if err := parseArgs(fs, args); err != nil {
		return exitForParseErr(err)
	}
	if fs.NArg() > 0 {
		return usageError(stderr, "usage: scrim hub [--data DIR] [--host HOST] [--port PORT] --push-token TOKEN [--read-token TOKEN] [--allow CIDR,...]")
	}

	if *pushToken == "" {
		errOut(stderr, errors.New("--push-token (or SCRIM_PUSH_TOKEN) is required -- a hub never runs write-accepting with no push gate"))
		return 1
	}

	cfg := config.Config{
		Dir:         config.ResolveDir(*dataDir),
		Host:        *host,
		Port:        *port,
		IdleTimeout: *idleTimeout,
		// The hub gate (server.HubOptions) replaces capability-token auth
		// entirely -- NoAuth: true bypasses withAuth's token minting so
		// canvasResponse/index URLs are emitted clean (no "?t=").
		NoAuth: true,
		NoMDNS: *noMDNS,
	}

	srv, err := server.NewHub(cfg, server.HubOptions{
		PushToken:  *pushToken,
		ReadToken:  *readToken,
		AllowCIDRs: splitCSV(*allow),
	})
	if err != nil {
		errOut(stderr, err)
		return 1
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := srv.Run(ctx); err != nil {
		errOut(stderr, err)
		return 1
	}
	return 0
}

// envOr returns the value of the named environment variable, or fallback if
// it's unset or empty.
func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

// envIntOr is envOr for an integer-valued environment variable; a malformed
// value falls back the same way an unset one does.
func envIntOr(key string, fallback int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

// splitCSV splits a comma-separated flag value into its trimmed,
// non-empty entries.
func splitCSV(csv string) []string {
	var out []string
	for _, part := range strings.Split(csv, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
