package server

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/jedwards1230/scrim/internal/canvas"
	"github.com/jedwards1230/scrim/internal/identity"
	"github.com/jedwards1230/scrim/internal/logging"
	"github.com/jedwards1230/scrim/internal/oidc"
	"github.com/jedwards1230/scrim/internal/principal"
	"github.com/jedwards1230/scrim/internal/usertoken"
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

		// Resolve the request's identity ONCE and stash it (plus the resolving
		// user token, if any) in the context so every downstream handler
		// (gallery/list filtering, owner attribution, auto-share) reads the same
		// claims without re-deriving them.
		c, tok := s.resolveClaims(r)
		ctx := withClaims(r.Context(), c)
		if tok != nil {
			ctx = withToken(ctx, tok)
		}
		r = r.WithContext(ctx)

		// The admin push token is the machine/bootstrap credential: it
		// authorizes ANY method, reads included, and is a visibility superuser
		// (see identity.CanView). It is distinct from the browser read gate
		// (OIDC / CIDR / read-token) below. A request carrying a verified
		// CF-forwarded actor is Admin=false (resolveClaims branch 1), so it does
		// NOT hit this bypass: it is authorized as the actor -- writes via
		// serveWrite's machine-plane branch, reads via serveOIDCRead's CanView.
		if c.Admin {
			next.ServeHTTP(w, r)
			return
		}

		// Writes (any method other than GET/HEAD).
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			s.serveWrite(w, r, next, c, tok)
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

// CF-forwarded actor attribution headers. scrim-mcp verifies the HMAC-signed
// X-Forwarded-User-* identity from ContextForge and re-emits the verified
// principal to the hub as these headers, on top of the admin push-token bearer
// (see internal/mcpserver's identity.go). The hub trusts them ONLY when they
// ride a valid admin push token (resolveClaims branch 1); they are never
// honored on any other request, so a spoofed header on a tokenless (or
// session/user-token) request is ignored by construction.
//
// Two-sided defense: this admin-bearer trust rule is one half. The other is a
// NetworkPolicy pinning the hub's machine-API ingress to scrim-mcp, so only the
// trusted attributor can even present these headers. Neither alone suffices --
// together they bound actor attribution to "scrim-mcp, holding the admin push
// token, on the allowed network path".
const (
	actorHeaderID     = "X-Scrim-Actor-Id"
	actorHeaderEmail  = "X-Scrim-Actor-Email"
	actorHeaderGroups = "X-Scrim-Actor-Groups"
)

// resolveClaims determines the identity a hub request carries, in precedence
// order, returning the resolving user token too (nil unless a user bearer token
// matched). It never calls out to the IdP -- it reads the presented credential
// only. Its one side effect is feeding the display-only principal registry when
// a CF actor is resolved (best-effort; enforcement never reads that registry).
func (s *Server) resolveClaims(r *http.Request) (identity.Claims, *usertoken.Token) {
	// 1. The global admin push token: the machine/bootstrap credential.
	if s.hasValidPushToken(r) {
		// A CF-forwarded actor rides the admin push token: when scrim-mcp
		// attaches verified X-Scrim-Actor-* headers, the request acts AS that
		// actor (Admin:false) rather than as the raw admin superuser. This is the
		// ONLY branch that honors those headers -- their trust derives entirely
		// from the accompanying valid admin bearer.
		if email := r.Header.Get(actorHeaderEmail); email != "" {
			c := identity.Claims{
				Subject: r.Header.Get(actorHeaderID),
				Email:   email,
				Groups:  splitActorGroups(r.Header.Get(actorHeaderGroups)),
			}
			s.observeCFActor(c)
			return c, nil
		}
		return identity.Claims{Admin: true}, nil
	}

	// 2. A valid user bearer token acts AS its owner. Looked up ABOVE the
	// session check so a machine client presenting a user token is attributed to
	// that token's owner, not to any session cookie it also happens to carry.
	if raw, ok := bearerToken(r); ok && s.tokens != nil {
		if tok, ok := s.tokens.Lookup(raw); ok {
			return identity.Claims{Email: tok.OwnerEmail}, tok
		}
	}

	// 3. A valid OIDC session cookie.
	if s.oidcAuth != nil {
		if sess, ok := s.oidcAuth.SessionFromRequest(r); ok {
			return identity.Claims{
				Subject: sess.Subject,
				Email:   sess.Email,
				Name:    sess.Name,
				Groups:  sess.Groups,
			}, nil
		}
	}

	// 4. Anonymous.
	return identity.Claims{}, nil
}

// splitActorGroups parses the comma-separated X-Scrim-Actor-Groups header into a
// trimmed, empty-free slice (nil when there are none).
func splitActorGroups(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if g := strings.TrimSpace(p); g != "" {
			out = append(out, g)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// observeCFActor feeds the display-only principal registry with a verified
// CF-forwarded actor. Best-effort: a registry write failure is logged (scrubbed)
// but never fails the request, and enforcement never reads the registry.
func (s *Server) observeCFActor(c identity.Claims) {
	if s.principals == nil || c.Email == "" {
		return
	}
	if err := s.principals.Observe(c.Email, c.Name, c.Groups, principal.SourceCFHeader); err != nil {
		logging.Error(logging.CategoryAuth, fmt.Errorf("principal registry: %w", err))
	}
}

// serveWrite authorizes a write (a non-GET/HEAD request) for a non-admin caller
// (admin is served earlier). The token-management endpoints are open to any
// session (a logged-in principal mints/revokes its own tokens); every other
// write requires a user bearer token whose owner may write the target canvas.
func (s *Server) serveWrite(w http.ResponseWriter, r *http.Request, next http.Handler, c identity.Claims, tok *usertoken.Token) {
	// A CF-forwarded actor rides the admin push token (the machine plane) but
	// acts AS the actor (Admin:false, no user token). It is distinguished from a
	// browser session -- also non-admin and tokenless -- by the presence of the
	// valid admin bearer, so recompute it once here.
	machineActor := tok == nil && c.Authenticated() && s.hasValidPushToken(r)

	// Claiming ownership of a legacy (admin-owned) canvas is the one write the
	// browser plane permits: it's how a logged-in principal takes ownership, so
	// any authenticated caller (session, user token, or CF actor) may reach the
	// claim handler, which enforces the admin-owned/409 rules itself.
	if isClaimPath(r.URL.Path) {
		if c.Authenticated() {
			next.ServeHTTP(w, r)
			return
		}
		http.Error(w, "unauthorized: login required to claim a canvas", http.StatusUnauthorized)
		return
	}

	// Token management: a logged-in OIDC session mints/revokes its OWN tokens.
	// A user-token principal may not mint further tokens (no privilege
	// escalation); nor may the machine plane (admin bearer / CF actor) or an
	// anonymous caller.
	if isTokenPath(r.URL.Path) {
		if c.Email != "" && tok == nil && !machineActor {
			next.ServeHTTP(w, r)
			return
		}
		if c.Authenticated() {
			http.Error(w, "forbidden: token management requires a browser session", http.StatusForbidden)
			return
		}
		http.Error(w, "unauthorized: login required", http.StatusUnauthorized)
		return
	}

	// A CF-forwarded actor writes AS itself on the machine plane: authorized by
	// the same ownership check a user token uses (creating a new canvas is always
	// allowed; writing an existing one requires CanWrite).
	if machineActor {
		if !s.userTokenMayWrite(r, c) {
			http.Error(w, "forbidden: your identity does not own this canvas", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
		return
	}

	// Every other write is a machine-API mutation: it needs the admin push token
	// (served earlier) or a user bearer token. A session/anonymous write is
	// unauthorized -- the browser plane is read-only.
	if tok == nil {
		http.Error(w, "unauthorized: missing or invalid token", http.StatusUnauthorized)
		return
	}
	// A user token may write only canvases its owner owns (admin is a superuser,
	// handled earlier). Centralized here so every mutating machine-API route is
	// covered uniformly without per-handler checks.
	if !s.userTokenMayWrite(r, c) {
		// 403, not 404: the caller supplied a valid token and named the id, so a
		// "not yours" answer leaks nothing it doesn't already know.
		http.Error(w, "forbidden: your token does not own this canvas", http.StatusForbidden)
		return
	}
	next.ServeHTTP(w, r)
}

// userTokenMayWrite reports whether a user-token principal may perform this
// write. Creating a brand-new canvas is always allowed (the principal will own
// it); the daemon-lifecycle stop is admin-only; every other write targets a
// canvas the principal must be able to CanWrite (a not-yet-existing target is
// allowed -- it's a create, or the handler will 404).
func (s *Server) userTokenMayWrite(r *http.Request, c identity.Claims) bool {
	// Stopping the hub is an admin-only lifecycle operation.
	if r.URL.Path == "/api/stop" {
		return false
	}
	// Creating a brand-new canvas: allowed, the principal owns it.
	if r.Method == http.MethodPost && r.URL.Path == "/api/canvases" {
		return true
	}
	id, ok := writeTargetCanvasID(r.URL.Path)
	if !ok {
		// An unrecognized write target -- fail closed.
		return false
	}
	if !canvas.Exists(s.canvasesDir, id) {
		// A not-yet-existing canvas: a first push creates it (owned by the
		// principal); a file write/edit will 404 at the handler. Either way the
		// principal isn't writing over someone else's canvas.
		return true
	}
	owner, _, err := canvas.GetOwnerGrants(s.metaDir, id)
	if err != nil {
		return false
	}
	return identity.CanWrite(ownerOrAdmin(owner), c)
}

// isTokenPath reports whether path addresses the token-management endpoints.
func isTokenPath(path string) bool {
	return path == "/api/tokens" || strings.HasPrefix(path, "/api/tokens/")
}

// isClaimPath reports whether path is a canvas-claim request
// (POST /api/canvases/{id}/claim), which any authenticated principal may reach.
func isClaimPath(path string) bool {
	rest, ok := strings.CutPrefix(path, "/api/canvases/")
	return ok && strings.HasSuffix(rest, "/claim") && !strings.Contains(strings.TrimSuffix(rest, "/claim"), "/")
}

// writeTargetCanvasID extracts the canvas id a write path mutates, covering the
// whole-canvas push route and the per-canvas machine-API routes.
func writeTargetCanvasID(p string) (string, bool) {
	for _, prefix := range []string{"/api/push/", "/api/canvases/"} {
		rest, ok := strings.CutPrefix(p, prefix)
		if !ok {
			continue
		}
		id := rest
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			id = rest[:i]
		}
		if id == "" || canvas.ValidateID(id) != nil {
			return "", false
		}
		return id, true
	}
	return "", false
}

// bearerToken returns the token presented in the Authorization: Bearer header,
// or ("", false) when absent/malformed.
func bearerToken(r *http.Request) (string, bool) {
	authz := r.Header.Get(pushAuthHeader)
	if !strings.HasPrefix(authz, bearerPrefix) {
		return "", false
	}
	return strings.TrimPrefix(authz, bearerPrefix), true
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
