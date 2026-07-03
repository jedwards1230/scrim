package server

import "net/http"

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
		gate = s.withHubGate
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
