package server

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/jedwards1230/scrim/internal/canvas"
	"github.com/jedwards1230/scrim/internal/identity"
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

		// The liveness/readiness probe is public and reveals nothing (200, no
		// body) -- exempt it by EXACT match (same rationale as the spec and
		// isAuthPath below) so an orchestrator probe with no cookie/token/CIDR
		// membership isn't answered with the 401 the OIDC read gate would give
		// it. A prefix would exempt paths it must not (#47).
		if r.URL.Path == healthzPath {
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

		// Resolve the request's identity ONCE and stash it in the context so
		// every downstream handler (gallery/list filtering, owner attribution)
		// reads the same claims without re-deriving them.
		c := s.resolveClaims(r)
		r = r.WithContext(withClaims(r.Context(), c))

		// The admin push token is the machine/bootstrap credential: it
		// authorizes ANY method, reads included, and is a visibility superuser
		// (see identity.CanView). It is distinct from the browser read gate
		// (OIDC / CIDR / read-token) below.
		//
		// TODO(#51): resolveClaims will additionally return a CF-forwarded actor
		// (Admin=false) when a valid admin push token carries verified
		// X-Scrim-Actor-* headers; such a request is authorized as that actor
		// (still not admin) rather than served here unconditionally.
		if c.Admin {
			next.ServeHTTP(w, r)
			return
		}

		// No admin credential. Every write (any method other than GET/HEAD) is
		// unauthorized -- the machine API is admin-gated in this phase.
		//
		// TODO(#50): a valid user bearer token whose owner CanWrite the target
		// canvas authorizes its own writes here (resolveClaims resolves it, the
		// per-canvas handlers enforce CanWrite) before this rejection.
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "unauthorized: missing or invalid push token", http.StatusUnauthorized)
			return
		}

		// Reads. OIDC, when configured, makes the hub private by default:
		// visibility is decided from the canvas's owner+grants and the request's
		// claims, never the caller's network position.
		if s.oidcAuth != nil {
			s.serveOIDCRead(w, r, next, c)
			return
		}

		// No OIDC: the hub keeps its exact legacy read gate -- a CIDR allowlist
		// match plus, if configured, a separate read token. There is no
		// per-canvas visibility here (no identity to enforce against); the CIDR
		// gate is the whole read security, as it was before #49.
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

// resolveClaims determines the identity a hub request carries, in precedence
// order. It never mutates the request and never calls out to the IdP -- it
// reads the presented credential only.
func (s *Server) resolveClaims(r *http.Request) identity.Claims {
	// 1. The global admin push token: the machine/bootstrap credential.
	if s.hasValidPushToken(r) {
		// TODO(#51): when r additionally carries verified HMAC-signed
		// X-Scrim-Actor-* headers, return the forwarded CF principal
		// (Claims{Subject/Email/Groups: actor…, Admin: false}) and feed the
		// principal registry with source "cf-header". X-Scrim-Actor-* is trusted
		// ONLY on this branch (a request bearing the admin token); it is never
		// honored otherwise, so a spoofed header on a tokenless request is
		// ignored by construction.
		return identity.Claims{Admin: true}
	}

	// 2. TODO(#50): a valid user bearer token (looked up in usertoken.Store,
	// constant-time hash compare over non-revoked tokens) resolves to
	// Claims{Email: token.OwnerEmail}; stash the *Token too, for its auto_share
	// grants + grant-target allowance. It sits ABOVE the session check so a
	// machine client presenting a user token is attributed to that token's owner.

	// 3. A valid OIDC session cookie.
	if s.oidcAuth != nil {
		if sess, ok := s.oidcAuth.SessionFromRequest(r); ok {
			return identity.Claims{
				Subject: sess.Subject,
				Email:   sess.Email,
				Name:    sess.Name,
				Groups:  sess.Groups,
			}
		}
	}

	// 4. Anonymous.
	return identity.Claims{}
}

// serveOIDCRead applies private-by-default read enforcement for a hub with OIDC
// configured. A per-canvas path (a canvas view, its SSE stream or favicon, or a
// per-canvas machine-API read) is served only when the claims (or a presented
// share-link secret) CanView the canvas; anything else is a general
// authenticated read (the index, the canvas list, status), served to any
// authenticated principal and privacy-filtered by the handler.
func (s *Server) serveOIDCRead(w http.ResponseWriter, r *http.Request, next http.Handler, c identity.Claims) {
	if id, ok := canvasIDFromURLPath(r.URL.Path); ok {
		owner, grants, _ := canvas.GetOwnerGrants(s.metaDir, id)
		if identity.CanView(ownerOrAdmin(owner), grants, c, linkSecretFrom(r)) {
			next.ServeHTTP(w, r)
			return
		}
		s.denyRead(w, r, c)
		return
	}
	// A non-canvas read (index / list / status): any authenticated principal may
	// reach it; the handler filters what it returns by CanView.
	if c.Authenticated() {
		next.ServeHTTP(w, r)
		return
	}
	s.denyRead(w, r, c)
}

// denyRead answers a read the caller may not have. An unauthenticated caller
// (no session and no valid link) is sent into the login flow if it's a browser
// navigation, or given a plain 401 it can act on otherwise (an SSE EventSource,
// curl, an API client). An authenticated-but-not-permitted caller gets a 404 --
// never a 403 -- so the response never reveals that a canvas it can't see even
// exists.
func (s *Server) denyRead(w http.ResponseWriter, r *http.Request, c identity.Claims) {
	if c.Authenticated() {
		http.NotFound(w, r)
		return
	}
	if wantsHTML(r) {
		target := oidc.LoginPath + "?return_to=" + url.QueryEscape(r.URL.RequestURI())
		http.Redirect(w, r, target, http.StatusFound)
		return
	}
	http.Error(w, "unauthorized: OIDC login required", http.StatusUnauthorized)
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
