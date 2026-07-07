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
		mux.HandleFunc("GET /api/canvases/{id}/files", s.handleListCanvasFiles)
		mux.HandleFunc("GET /api/canvases/{id}/files/{path...}", s.handleReadCanvasFile)
		mux.HandleFunc("PUT /api/canvases/{id}/files/{path...}", s.handleWriteCanvasFile)
		// PATCH is non-GET/HEAD, so withHubGate already bearer-gates it like
		// every other machine-API write -- no extra gate code.
		mux.HandleFunc("PATCH /api/canvases/{id}/files/{path...}", s.handleEditCanvasFile)
		mux.HandleFunc("POST /api/canvases/{id}/copy", s.handleCopyCanvas)
		mux.HandleFunc("GET /api/canvases/{id}/snapshots", s.handleListSnapshots)
		mux.HandleFunc("POST /api/canvases/{id}/snapshots", s.handleCreateSnapshot)
		mux.HandleFunc("POST /api/canvases/{id}/snapshots/{name}/revert", s.handleRevertSnapshot)

		gate = s.withHubGate

		// The OIDC login routes exist only when a hub was started with OIDC
		// configured. They must be reachable WITHOUT a session (that is how a
		// user logs in), so withHubGate exempts the /auth/ prefix; registering
		// them only here keeps that exemption inert for a non-OIDC hub, where
		// the paths simply 404.
		if s.oidcAuth != nil {
			mux.HandleFunc("GET "+oidc.LoginPath, s.oidcAuth.HandleLogin)
			mux.HandleFunc("GET "+oidc.CallbackPath, s.oidcAuth.HandleCallback)
			mux.HandleFunc("GET "+oidc.LogoutPath, s.oidcAuth.HandleLogout)
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
