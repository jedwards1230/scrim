package server

import (
	"net/http"

	"github.com/jedwards1230/scrim/internal/oidc"
)

// routes builds the daemon's full HTTP handler: the index page, per-canvas
// static serving + SSE, and the /api/* control surface, wrapped with
// activity tracking.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", s.handleIndex)

	mux.HandleFunc("GET /c/{id}", s.handleCanvasRedirect)
	mux.HandleFunc("GET /c/{id}/__events", s.handleSSE)
	mux.HandleFunc("GET /c/{id}/favicon.ico", s.handleCanvasFavicon)
	mux.HandleFunc("GET /c/{id}/{rest...}", s.handleCanvas)

	mux.HandleFunc("GET /api/status", s.handleAPIStatus)
	mux.HandleFunc("POST /api/canvases", s.handleCreateCanvas)
	mux.HandleFunc("GET /api/canvases", s.handleListCanvases)
	mux.HandleFunc("DELETE /api/canvases/{id}", s.handleDeleteCanvas)
	mux.HandleFunc("POST /api/stop", s.handleStop)

	// The push route only exists in hub mode -- registering it
	// unconditionally would give the default daemon a write surface it
	// never asked for and has no gate for.
	gate := s.withAuth
	if s.isHub() {
		mux.HandleFunc("POST /api/push/{id}", s.handlePush)

		// Machine-API surface for a remote MCP client (see mcpserver's
		// hubBackend): per-file read/write and per-canvas snapshot control,
		// so a client with no shared disk can author canvas content over the
		// wire. All bearer-gated via withHubGate (reads included -- the push
		// token authorizes any method). Registered ONLY in hub mode so the
		// default daemon gets zero new surface (hub_test.go invariant).
		// Per-canvas machine-API reads are private-by-default enforced at the
		// gate (withHubGate resolves claims + CanView by canvas id for any
		// /api/canvases/{id}/… read under OIDC); writes are authorized there too
		// (admin push token, or a user token whose owner CanWrite the canvas --
		// see serveWrite/userTokenMayWrite), so these handlers need no per-route
		// auth.
		mux.HandleFunc("GET /api/canvases/{id}/files", s.handleListCanvasFiles)
		mux.HandleFunc("GET /api/canvases/{id}/files/{path...}", s.handleReadCanvasFile)
		mux.HandleFunc("PUT /api/canvases/{id}/files/{path...}", s.handleWriteCanvasFile)
		// PATCH is non-GET/HEAD, so withHubGate already bearer-gates it like
		// every other machine-API write -- no extra gate code.
		mux.HandleFunc("PATCH /api/canvases/{id}/files/{path...}", s.handleEditCanvasFile)
		mux.HandleFunc("POST /api/canvases/{id}/copy", s.handleCopyCanvas)

		// Per-canvas sharing grants (#52). GET is a read (visibility-gated like
		// any other per-canvas read); POST/DELETE are writes authorized in
		// withHubGate (owner/admin/CF-actor), with POST additionally bounded by a
		// user token's allowance in the handler.
		mux.HandleFunc("GET /api/canvases/{id}/grants", s.handleListGrants)
		mux.HandleFunc("POST /api/canvases/{id}/grants", s.handleCreateGrant)
		mux.HandleFunc("DELETE /api/canvases/{id}/grants/{grantRef}", s.handleDeleteGrant)

		// Legacy-canvas ownership claim (#55). A write, but authorized for any
		// authenticated principal in withHubGate (serveWrite's claim branch): it
		// is how a logged-in principal takes ownership of an admin-owned canvas.
		mux.HandleFunc("POST /api/canvases/{id}/claim", s.handleClaimCanvas)

		// User-token management (#50). Hub-only. POST/DELETE are authorized in
		// withHubGate for a browser session (or admin); GET lists the caller's
		// own tokens. Raw secrets are returned only once, by POST.
		mux.HandleFunc("POST /api/tokens", s.handleCreateToken)
		mux.HandleFunc("GET /api/tokens", s.handleListTokens)
		mux.HandleFunc("DELETE /api/tokens/{id}", s.handleRevokeToken)

		// Principal autocomplete (#53): the share dialog's grantee suggestions.
		// A session-gated read (general non-canvas read at the gate), thin over
		// the principalLister seam so #54 can layer a directory driver (#54).
		mux.HandleFunc("GET /api/principals", s.handlePrincipals)

		// The my-tokens management page (#53): a server-rendered HTML page that
		// drives the /api/tokens JSON endpoints from the browser. A read like the
		// index, so the gate lets an authenticated session (or a CIDR-allowed
		// non-OIDC reader) reach it; it is NOT part of the machine API (an HTML
		// page, like the index, so it stays out of api/openapi.yaml).
		mux.HandleFunc("GET /tokens", s.handleTokensPage)
		mux.HandleFunc("GET /api/canvases/{id}/snapshots", s.handleListSnapshots)
		mux.HandleFunc("POST /api/canvases/{id}/snapshots", s.handleCreateSnapshot)
		mux.HandleFunc("POST /api/canvases/{id}/snapshots/{name}/revert", s.handleRevertSnapshot)

		// The machine API's own contract, served so standard OpenAPI tooling can
		// consume it from a live hub. Hub-only like the routes it documents; the
		// default daemon never serves it (hub_test.go invariant). Gate-exempt in
		// withHubGate -- the spec is public and must be fetchable without a token.
		mux.HandleFunc("GET "+openAPISpecPath, s.handleOpenAPISpec)

		// A dependency-free liveness/readiness probe for orchestrators (e.g.
		// kubelet). Hub-only like the machine API it fronts, and gate-exempt in
		// withHubGate (exact match) so a cookie-less probe gets a 200 rather than
		// the 401 an OIDC read gate would otherwise return (#47).
		mux.HandleFunc("GET "+healthzPath, s.handleHealthz)

		gate = s.withHubGate

		// The OIDC login routes exist only when a hub was started with OIDC
		// configured. They must be reachable WITHOUT a session (that is how a
		// user logs in), so withHubGate exempts the /auth/ prefix; registering
		// them only here keeps that exemption inert for a non-OIDC hub, where
		// the paths simply 404.
		if s.oidcAuth != nil {
			mux.HandleFunc("GET "+oidc.LoginPath, s.oidcAuth.HandleLogin)
			mux.HandleFunc("GET "+oidc.CallbackPath, s.oidcAuth.HandleCallback)
			// Logout is POST-only: a plain GET logout is CSRF-able (any page
			// could force a logout via an <img>/link), so the mux answers a GET
			// with 405. isAuthPath still exempts the path (it matches by path,
			// method-agnostic), so the POST reaches the handler.
			mux.HandleFunc("POST "+oidc.LogoutPath, s.oidcAuth.HandleLogout)
		}
	}

	return withSecurityHeaders(gate(s.withActivity(mux)))
}

// withSecurityHeaders sets response headers that apply to every request
// regardless of outcome -- including a 401 from withAuth or a 302 from its
// token-redirect, neither of which reach the mux's own handlers -- so it
// wraps outermost. Referrer-Policy: no-referrer keeps the current URL (and,
// before a redirect strips it, the capability token riding in its query
// string) from ever being sent to a destination this page might link out
// to.
func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// withActivity marks the server as active on every request. SSE
// connections additionally mark activity again on disconnect (see
// handleSSE) so the idle clock restarts from the moment the connection
// actually closes, not from when it was opened.
func (s *Server) withActivity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.activity.touch()
		next.ServeHTTP(w, r)
	})
}
