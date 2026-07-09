package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jedwards1230/scrim/internal/config"
)

// TestHubHealthzReachableWithoutAuth proves GET /healthz is served in hub mode
// without a token, session, or CIDR membership -- the whole point of the route
// (#47): an orchestrator probe is cookie-less and non-browser, so an
// OIDC-configured read gate would otherwise 401 it. It is exempt by exact match
// alongside the openapi-spec exemption.
func TestHubHealthzReachableWithoutAuth(t *testing.T) {
	// An OIDC-configured hub is the strictest read gate; if healthz is reachable
	// here it is reachable on every hub. Reuse the OIDC hub helper from
	// hubgate_oidc_test.go.
	s, _, _ := newOIDCHub(t)

	// No cookie, no bearer, a non-loopback RemoteAddr outside any allowlist --
	// exactly an external probe.
	req := httptest.NewRequest(http.MethodGet, healthzPath, nil)
	req.RemoteAddr = "203.0.113.7:54321"
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want 200 (gate-exempt probe)", rec.Code)
	}
	if body := rec.Body.String(); body != "" {
		t.Errorf("GET /healthz body = %q, want empty", body)
	}
}

// TestDefaultDaemonHasNoHealthz mirrors the hub_test.go hard invariant: the
// default daemon path (server.New) gains zero new surface from hub mode, so
// /healthz -- a hub-only route -- must not exist there. A 404 (route absent),
// not a gate rejection, is the proof.
func TestDefaultDaemonHasNoHealthz(t *testing.T) {
	cfg := config.Config{
		Dir:         t.TempDir(),
		Host:        "127.0.0.1",
		Port:        0,
		IdleTimeout: time.Hour,
		NoAuth:      true,
	}
	s := New(cfg)
	if s.isHub() {
		t.Fatal("server.New(cfg) reports isHub() = true, want false")
	}

	req := httptest.NewRequest(http.MethodGet, healthzPath, nil)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /healthz on default daemon status = %d, want 404 (hub-only route absent)", rec.Code)
	}
}
