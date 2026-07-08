package server

import (
	"net/http"

	"github.com/jedwards1230/scrim/api"
)

// openAPISpecPath is the single source of truth for the spec route: the mux
// registration (routes.go), the gate exemption (hubgate.go), and this handler
// all reference it, so the exempt path and the served path can never drift.
const openAPISpecPath = "/api/openapi.yaml"

// handleOpenAPISpec serves GET /api/openapi.yaml (hub mode only, see
// routes.go): the embedded hand-authored OpenAPI 3.1 document describing this
// machine API, so standard OpenAPI tooling can consume the contract straight
// from a live hub. It is gate-exempt (see withHubGate) -- the spec is public
// (committed in the repo), carries no canvas content, and must be fetchable by
// a non-browser tool with no session or token; the hub's ingress is LAN-only.
// Served verbatim from the embedded bytes; a single Write lets net/http set
// Content-Length.
func (s *Server) handleOpenAPISpec(w http.ResponseWriter, r *http.Request) {
	// application/yaml is the registered media type for YAML (RFC 9512).
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(api.OpenAPISpecYAML)
}
