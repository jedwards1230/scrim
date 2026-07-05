package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/jedwards1230/scrim/internal/mcpserver"
	"github.com/jedwards1230/scrim/internal/version"
)

// cmdMcp implements `scrim mcp [--http ADDR] [--allow-lan]`. It runs an MCP
// server exposing scrim's verbs as tools, driving the same primitives the CLI
// verbs do. The default transport is stdio (stdout is the MCP protocol
// channel); --http switches to streamable HTTP.
//
// The HTTP transport is unauthenticated (remote auth is tracked in scrim#33),
// so it binds loopback by default and refuses a non-loopback bind unless
// --allow-lan explicitly opts in.
func cmdMcp(args []string, _, stderr io.Writer) int {
	fs := newFlagSet("mcp", stderr)
	cf := registerCommonFlags(fs)
	httpAddr := fs.String("http", "", "serve streamable-HTTP MCP on this addr (e.g. 127.0.0.1:7799); default empty = stdio")
	allowLAN := fs.Bool("allow-lan", false, "allow a non-loopback --http bind despite the endpoint being unauthenticated (remote auth tracked in scrim#33)")
	if err := parseArgs(fs, args); err != nil {
		return exitForParseErr(err)
	}
	if fs.NArg() != 0 {
		return usageError(stderr, "usage: scrim mcp [--http ADDR] [--allow-lan]")
	}

	plan, err := planMcp(*httpAddr, *allowLAN)
	if err != nil {
		return usageError(stderr, "%s", err.Error())
	}
	if plan.warnAllowLAN {
		outf(stderr, "scrim mcp: warning: --allow-lan has no effect without --http (stdio has no network bind to gate)\n")
	}

	cfg := cf.toConfig()

	// A signal-cancellable context so Ctrl-C (SIGINT) or SIGTERM stops the
	// server cleanly, mirroring cli.cmdPush.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if plan.http {
		return serveResult(mcpserver.ServeHTTP(ctx, *httpAddr, cfg, version.Short(), stderr), stderr)
	}
	return serveResult(mcpserver.Serve(ctx, cfg, version.Short(), stderr), stderr)
}

// mcpPlan is the decision planMcp derives from the mcp verb's transport flags,
// separated out so the flag/gate logic is unit-testable without starting a
// blocking server.
type mcpPlan struct {
	// http is true when the streamable-HTTP transport was requested (--http).
	http bool
	// warnAllowLAN is true when --allow-lan was passed without --http (a no-op
	// worth a stderr warning, not a hard error).
	warnAllowLAN bool
}

// planMcp validates the transport flags and returns the resulting plan, or a
// usage error when a non-loopback --http bind is requested without --allow-lan
// (the HTTP MCP endpoint is unauthenticated — see scrim#33). It performs no
// I/O, so tests can exercise every branch without binding a listener.
func planMcp(httpAddr string, allowLAN bool) (mcpPlan, error) {
	if httpAddr == "" {
		return mcpPlan{http: false, warnAllowLAN: allowLAN}, nil
	}
	if !mcpserver.IsLoopbackAddr(httpAddr) && !allowLAN {
		return mcpPlan{}, fmt.Errorf(
			"scrim mcp --http %s binds a non-loopback address (a bare :PORT binds every interface), "+
				"but the HTTP MCP endpoint is UNAUTHENTICATED (remote auth is tracked in scrim#33). "+
				"Pass --allow-lan to accept an unauthenticated LAN-reachable server, "+
				"or bind loopback instead (e.g. 127.0.0.1%s)",
			httpAddr, portSuffix(httpAddr))
	}
	return mcpPlan{http: true}, nil
}

// serveResult maps a server's exit to a process exit code: 0 for a clean stop
// (nil, or a ctx-cancelled shutdown that surfaces as nil or context.Canceled),
// 1 for a genuine serve error — which it prints to stderr first, like every
// other verb, so a failed bind or serve isn't silently swallowed behind a bare
// exit code.
func serveResult(err error, stderr io.Writer) int {
	if err == nil || errors.Is(err, context.Canceled) {
		return 0
	}
	errOut(stderr, err)
	return 1
}

// portSuffix returns the ":PORT" tail of addr for the loopback suggestion in
// the refusal message, or ":"+addr when addr has no ':'. Cosmetic — a
// best-effort hint, never load-bearing.
func portSuffix(addr string) string {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[i:]
		}
	}
	return ":" + addr
}
