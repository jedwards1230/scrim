package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jedwards1230/scrim/api"
)

// TestOpenAPISpecServedInHubMode confirms the hub serves the embedded spec at
// GET /api/openapi.yaml with the right content type, byte-for-byte, and that it
// is gate-exempt: reachable with NO push token AND from a non-loopback
// RemoteAddr outside the (loopback-only) CIDR allowlist, since the spec is
// public. A read-token hub must serve it without the read token too.
func TestOpenAPISpecServedInHubMode(t *testing.T) {
	// A read token would normally gate reads; the spec must bypass it.
	s, _ := newHubTestServer(t, []string{"127.0.0.0/8"}, "test-read-token")
	routes := s.routes()

	req := httptest.NewRequest(http.MethodGet, "/api/openapi.yaml", nil)
	// Deliberately outside the loopback allowlist and with no bearer/read token:
	// a gate-exempt path must still be served.
	req.RemoteAddr = "203.0.113.7:54321"
	rec := httptest.NewRecorder()
	routes.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/openapi.yaml status = %d, want 200 (gate-exempt public spec)", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/yaml" {
		t.Errorf("Content-Type = %q, want application/yaml", ct)
	}
	if !bytes.Equal(rec.Body.Bytes(), api.OpenAPISpecYAML) {
		t.Errorf("served spec body does not match the embedded api.OpenAPISpecYAML")
	}
	if len(api.OpenAPISpecYAML) == 0 {
		t.Fatal("api.OpenAPISpecYAML is empty -- the spec was not embedded")
	}
}

// TestOpenAPISpecExemptionIsExactMatch confirms the gate exemption is an exact
// path match, not a prefix: a look-alike path under the same prefix is NOT
// exempt and still falls to the read gate (a non-loopback RemoteAddr with no
// token is forbidden), so the exemption can't be used to reach anything else.
func TestOpenAPISpecExemptionIsExactMatch(t *testing.T) {
	s, _ := newHubTestServer(t, []string{"127.0.0.0/8"}, "")
	routes := s.routes()

	// A path that merely shares the prefix must not inherit the exemption. It
	// isn't a registered route, but the gate runs first: from a non-loopback
	// address it must be refused by the CIDR check (403), never served.
	req := httptest.NewRequest(http.MethodGet, "/api/openapi.yaml/extra", nil)
	req.RemoteAddr = "203.0.113.7:54321"
	rec := httptest.NewRecorder()
	routes.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("GET /api/openapi.yaml/extra from non-loopback status = %d, want 403 (exemption must be exact-match, not prefix)", rec.Code)
	}
}
