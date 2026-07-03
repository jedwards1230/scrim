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

	return s.withAuth(s.withActivity(mux))
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
