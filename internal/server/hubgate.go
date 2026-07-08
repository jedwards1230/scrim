package server

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/jedwards1230/scrim/internal/oidc"
)

// pushAuthHeader is the header a write request must present the hub's push
// token in, as a standard bearer credential.
const pushAuthHeader = "Authorization"

// bearerPrefix is the value prefix pushAuthHeader must carry before the
// token itself.
const bearerPrefix = "Bearer "

// withHubGate replaces withAuth in hub mode (see routes.go): it gates
// writes (any method other than GET/HEAD -- POST /api/push, POST /api/stop,
// any POST/DELETE under /api/canvases) behind the hub's push token, and
// reads (GET/HEAD -- the index, /c/..., SSE, favicon, /api/status) behind
// EITHER the OIDC session gate (when OIDC is configured) OR, otherwise, a
// CIDR allowlist check first and then, if configured, the hub's separate
// read token via checkToken (the same logic withAuth uses for the default
// daemon, just against a different expected token).
//
// OIDC vs CIDR is deliberately exclusive, not layered: when OIDC is on it is
// the whole read gate -- a valid session cookie is required and the CIDR/
// read-token path is not consulted, since OIDC proves identity regardless of
// the caller's network position (a homelab hub reached over the LAN, over
// Tailscale, or through a proxy all authenticate the same way). With no OIDC
// config the hub behaves EXACTLY as before -- this is the fail-closed,
// opt-in contract from issue #32.
//
// The push token is the write gate on its own -- writes are deliberately
// not also CIDR/OIDC-checked, since a legitimate push client is a machine
// with the token, commonly outside any human read allowlist (e.g. a
// developer's laptop pushing to a homelab hub it isn't itself permitted to
// browse from).
func (s *Server) withHubGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The OpenAPI spec is public (committed in the repo, carries no canvas
		// content) and must be fetchable by a non-browser OpenAPI tool without a
		// session or token -- exempt it from the gate entirely, regardless of
		// OIDC. Exact match only (same rationale as isAuthPath below): a prefix
		// would exempt paths it must not.
		if r.URL.Path == openAPISpecPath {
			next.ServeHTTP(w, r)
			return
		}

		// The OIDC login routes must be reachable by an unauthenticated
		// browser -- gating them would make logging in impossible. They are
		// registered only when oidcAuth is set (see routes.go), so this
		// exemption is inert for a non-OIDC hub. Match the exact three paths
		// rather than an "/auth/" prefix: a prefix would exempt any
		// /auth/-prefixed request (including "/auth/../c/secret" before the
		// mux cleans it), and there is no reason to exempt anything but the
		// real login endpoints.
		if s.oidcAuth != nil && isAuthPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		// A valid push token authorizes ANY method, reads included -- it's the
		// machine-API credential (MCP client / push), distinct from the browser
		// read gate (OIDC/CIDR/read-token) below.
		if s.hasValidPushToken(r) {
			next.ServeHTTP(w, r)
			return
		}
		// Reaching here means the bearer check above already failed, so any
		// write is unauthorized -- no second token check.
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "unauthorized: missing or invalid push token", http.StatusUnauthorized)
			return
		}

		// Reads. OIDC, when configured, is the entire read gate.
		if s.oidcAuth != nil {
			if _, ok := s.oidcAuth.SessionFromRequest(r); ok {
				next.ServeHTTP(w, r)
				return
			}
			// Unauthenticated: send a browser into the login flow (preserving
			// where it was headed), but hand a non-browser client (the SSE
			// EventSource, curl, an API caller) a 401 it can act on rather than
			// an HTML redirect it can't follow meaningfully.
			if wantsHTML(r) {
				target := oidc.LoginPath + "?return_to=" + url.QueryEscape(r.URL.RequestURI())
				http.Redirect(w, r, target, http.StatusFound)
				return
			}
			http.Error(w, "unauthorized: OIDC login required", http.StatusUnauthorized)
			return
		}

		ip, err := clientIP(r)
		if err != nil || !ipAllowed(ip, s.hubCfg.allowedNets) {
			http.Error(w, "forbidden: client is not in the allowed range", http.StatusForbidden)
			return
		}

		if s.hubCfg.readToken == "" {
			next.ServeHTTP(w, r)
			return
		}
		checkToken(w, r, next, s.hubCfg.readToken)
	})
}

// isAuthPath reports whether path is exactly one of the OIDC login routes the
// hub read gate must let through unauthenticated. Exact matches only -- see
// withHubGate's rationale for not using a prefix.
func isAuthPath(path string) bool {
	return path == oidc.LoginPath || path == oidc.CallbackPath || path == oidc.LogoutPath
}

// wantsHTML reports whether r looks like a top-level browser navigation --
// one whose Accept header includes text/html -- as opposed to a programmatic
// client (an EventSource, which sends text/event-stream; curl; the CLI's
// apiclient). It decides whether an unauthenticated OIDC read is answered with
// a redirect into the login flow (browser) or a plain 401 (everything else).
func wantsHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// hasValidPushToken reports whether r carries the hub's push token as a
// standard "Authorization: Bearer <token>" header, compared in constant
// time.
func (s *Server) hasValidPushToken(r *http.Request) bool {
	authz := r.Header.Get(pushAuthHeader)
	if !strings.HasPrefix(authz, bearerPrefix) {
		return false
	}
	return constantTimeEqual(strings.TrimPrefix(authz, bearerPrefix), s.hubCfg.pushToken)
}

// clientIP extracts the request's client IP from r.RemoteAddr, stripping
// the port net/http always attaches for a real network connection.
//
// X-Forwarded-For is deliberately NOT honored here: it's a plain request
// header any client can set to an arbitrary value, so trusting it would let
// any caller claim to be inside the allowlist. Phase 2 (a trusted-proxy
// layer in front of the hub, e.g. Traefik) will need to add explicit
// trusted-proxy handling before that header can be trusted; Phase 1 (this
// code) has no such layer, so it stays unused.
func clientIP(r *http.Request) (net.IP, error) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// No port present (uncommon outside of tests using a synthetic
		// RemoteAddr) -- treat the whole value as the host.
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, fmt.Errorf("hub: parsing client IP from remote addr %q", r.RemoteAddr)
	}
	return ip, nil
}

// ipAllowed reports whether ip falls inside any of nets.
func ipAllowed(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
