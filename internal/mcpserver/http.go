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
//
// When oauth is non-nil the endpoint becomes an RFC 9728 OAuth 2.0 protected
// resource: /mcp is wrapped with bearer validation + per-tool scope enforcement
// and the protected-resource metadata is served UNAUTHENTICATED at its
// well-known path. A nil oauth leaves the transport exactly as before (no
// bearer requirement, no metadata endpoint). Either way the CF header-trust
// plane (identity.go) is unchanged -- the two identity layers are orthogonal.
func newHTTPHandler(cfg config.Config, ver string, hub *HubTarget, oauth *oauthValidator) http.Handler {
	srv := NewServer(cfg, ver, hub)
	mux := http.NewServeMux()
	var mcpHandler http.Handler = mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv },
		nil,
	)
	if oauth != nil {
		// The metadata endpoint is deliberately mounted OUTSIDE the bearer gate:
		// a client fetches it precisely because it holds no token yet.
		mux.HandleFunc("GET "+protectedResourceMetadataPath, oauth.handleMetadata)
		mcpHandler = oauth.middleware(mcpHandler)
	}
	mux.Handle(mcpPath, mcpHandler)
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
// When oauth.Enabled() the endpoint is an RFC 9728 OAuth 2.0 protected resource
// (scrim#33): OIDC discovery runs HERE, before the listener binds, so a bad
// issuer fails fast and the "serving on ..." banner never prints for a server
// that can't validate tokens. An empty OAuthConfig leaves the transport
// unauthenticated as before -- the caller (cli.cmdMcp) still gates a
// non-loopback bind behind --allow-lan via IsLoopbackAddr, unless OAuth makes
// the endpoint authenticated.
func ServeHTTP(ctx context.Context, addr string, cfg config.Config, ver string, hub *HubTarget, oauth OAuthConfig, stderr io.Writer) error {
	var validator *oauthValidator
	if oauth.Enabled() {
		v, err := newOAuthValidator(ctx, oauth)
		if err != nil {
			return fmt.Errorf("mcp http oauth: %w", err)
		}
		validator = v
		if stderr != nil {
			// Diagnostics only: the mode and metadata path, never the token,
			// issuer secret, or any request content.
			_, _ = fmt.Fprintf(stderr, "scrim mcp: OAuth protected-resource mode enabled (issuer discovered; metadata at %s)\n", protectedResourceMetadataPath)
		}
	}

	httpSrv := newHTTPServer(addr, newHTTPHandler(cfg, ver, hub, validator))

	// Bind synchronously up front so a bind failure (port in use, bad addr)
	// is returned to the caller BEFORE the "serving on ..." banner prints —
	// the banner must never claim success for a listener that never came up.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("mcp http listen on %s: %w", addr, err)
	}

	if stderr != nil {
		// Diagnostics only: the (now bound) address and endpoint paths, never
		// a URL, canvas content, or token.
		_, _ = fmt.Fprintf(stderr, "scrim mcp: serving streamable-HTTP on %s (MCP at %s, health at %s)\n",
			ln.Addr(), mcpPath, healthPath)
	}

	errCh := make(chan error, 1)
	go func() {
		err := httpSrv.Serve(ln)
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
