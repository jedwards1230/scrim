package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jedwards1230/scrim/internal/config"
)

// MCP endpoint and health paths served by the streamable-HTTP transport.
const (
	mcpPath    = "/mcp"
	healthPath = "/healthz"
)

// IsLoopbackAddr reports whether an http listen address (as passed to --http,
// e.g. "127.0.0.1:9000", "[::1]:9000", "localhost:9000") binds only a loopback
// interface. A bare port (":9000") binds every interface (net/http's
// zero-value behaviour) and is therefore treated as non-loopback — the case
// the --allow-lan gate exists to catch. No DNS lookups are performed: only
// literal loopback IPs and the "localhost" literal count as loopback, so an
// arbitrary hostname is conservatively treated as non-loopback (fail closed).
//
// A malformed address (net.SplitHostPort fails — e.g. a bare host with no
// port, an operator typo of "127.0.0.1" instead of "127.0.0.1:9000") is ALSO
// treated as non-loopback, never as loopback: naively re-parsing the whole
// string as a host would misclassify exactly that typo as loopback and
// silently skip the LAN gate, the opposite of fail-closed.
func IsLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" {
		return false // bare ":9000" binds every interface, not just loopback
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// newHTTPHandler builds the scrim MCP server and returns an http.Handler
// exposing it as a streamable-HTTP MCP endpoint at /mcp plus a GET /healthz
// liveness probe. It reuses NewServer, so the tool set (and thus behaviour) is
// identical to the stdio transport.
func newHTTPHandler(cfg config.Config, ver string) http.Handler {
	srv := NewServer(cfg, ver)
	mux := http.NewServeMux()
	mux.Handle(mcpPath, mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv },
		nil,
	))
	mux.HandleFunc("GET "+healthPath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	return mux
}

// newHTTPServer constructs the http.Server for the streamable-HTTP transport
// with timeouts tuned for the MCP streaming contract. It is a separate
// function so the timeout policy is unit-testable without binding a listener.
//
// Timeout rationale (copied from labctl's mcpserver, satisfies gosec
// G112/G114 and blunts Slowloris):
//   - ReadHeaderTimeout (10s): bounds slow-header (Slowloris) attacks.
//   - ReadTimeout (60s): bounds the full request read. Streamable-HTTP MCP
//     requests are small JSON-RPC POST bodies (and bodyless GETs for the
//     server→client SSE listen stream), so 60s is generous headroom while
//     still adding resource-exhaustion protection.
//   - IdleTimeout (120s): bounds idle keep-alive connection reuse.
//   - WriteTimeout is intentionally LEFT AT 0 (unlimited): MCP streaming
//     responses have no upper bound on duration, so any finite WriteTimeout
//     would eventually truncate a long-lived stream mid-response. Do not set
//     it — that is not a bug to "fix".
func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
		// WriteTimeout deliberately omitted (0 = unlimited) — see doc comment.
	}
}

// ServeHTTP builds the scrim MCP server and serves it over the streamable-HTTP
// transport on addr, blocking until ctx is cancelled. The MCP endpoint is
// mounted at /mcp and a GET /healthz liveness probe at /healthz. On ctx
// cancellation it shuts the server down gracefully with a short timeout.
//
// This transport is UNAUTHENTICATED — OAuth for remote clients is tracked in
// scrim#33. The caller (cli.cmdMcp) is responsible for gating a non-loopback
// bind behind --allow-lan via isLoopbackAddr before calling this.
func ServeHTTP(ctx context.Context, addr string, cfg config.Config, ver string, stderr io.Writer) error {
	httpSrv := newHTTPServer(addr, newHTTPHandler(cfg, ver))

	if stderr != nil {
		// Diagnostics only: the bind address and endpoint paths, never a URL,
		// canvas content, or token.
		_, _ = fmt.Fprintf(stderr, "scrim mcp: serving streamable-HTTP on %s (MCP at %s, health at %s)\n",
			addr, mcpPath, healthPath)
	}

	errCh := make(chan error, 1)
	go func() {
		err := httpSrv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("mcp http shutdown: %w", err)
		}
		return <-errCh
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("mcp http serve: %w", err)
		}
		return nil
	}
}
