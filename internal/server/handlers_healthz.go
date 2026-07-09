package server

import "net/http"

// healthzPath is the single source of truth for the liveness-probe route: the
// mux registration (routes.go) and the gate exemption (hubgate.go) both
// reference it, so the served path and the exempt path can never drift.
const healthzPath = "/healthz"

// handleHealthz serves GET /healthz (hub mode only, see routes.go): a
// dependency-free liveness/readiness probe returning 200 with no body. It is
// gate-exempt (see withHubGate) so an orchestrator's probe -- a cookie-less,
// non-browser client -- gets a clean 200 instead of the 401 an OIDC-configured
// read gate would otherwise answer. It reveals nothing (no canvas content, no
// identity), mirroring the /healthz the `scrim mcp --http` server already
// ships for exactly this purpose.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}
