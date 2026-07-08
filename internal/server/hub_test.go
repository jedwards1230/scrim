package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jedwards1230/scrim/internal/config"
)

// TestDefaultServerHasNoHubSurface is the HARD INVARIANT test: the default
// daemon path (server.New) gets zero new behavior, deps, or surface from
// hub mode. It asserts both halves in one place:
//
//  1. A default-mode Server's hubCfg is nil (isHub() is false), and its
//     routes() never registers the push route at all -- POST /api/push/foo
//     returns 404, not merely "401 from some gate", proving the route
//     itself doesn't exist rather than existing-but-rejecting.
//  2. A default-mode Server does not CIDR-gate: a request carrying a
//     non-loopback RemoteAddr is served exactly as it always has been --
//     still capability-token gated by withAuth (unchanged), never 403'd by
//     a CIDR check that must not run in this mode at all.
func TestDefaultServerHasNoHubSurface(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{
		Dir:         dir,
		Host:        "127.0.0.1",
		Port:        0,
		IdleTimeout: time.Hour,
		NoAuth:      true,
	}
	s := New(cfg)

	if s.isHub() {
		t.Fatal("server.New(cfg) reports isHub() = true, want false")
	}
	if s.hubCfg != nil {
		t.Fatal("server.New(cfg) has a non-nil hubCfg, want nil")
	}

	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)

	// 1. The push route doesn't exist at all in default mode.
	resp, err := http.Post(ts.URL+"/api/push/foo", "application/x-tar", nil)
	if err != nil {
		t.Fatalf("POST /api/push/foo: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST /api/push/foo status = %d, want 404 (route not registered in default mode)", resp.StatusCode)
	}

	// 1b. None of the machine-API routes exist in default mode either -- the
	// list-files and copy routes added alongside the file/snapshot surface are
	// hub-only exactly like push. A 404 (not a gate rejection) proves the
	// routes themselves are unregistered.
	for _, mr := range []struct {
		method, path string
	}{
		{http.MethodGet, "/api/canvases/foo/files"},
		{http.MethodGet, "/api/canvases/foo/files/index.html"},
		{http.MethodPost, "/api/canvases/foo/copy"},
		{http.MethodGet, "/api/canvases/foo/snapshots"},
	} {
		req := httptest.NewRequest(mr.method, mr.path, nil)
		rec := httptest.NewRecorder()
		s.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s status = %d, want 404 (hub-only route absent in default mode)", mr.method, mr.path, rec.Code)
		}
	}

	// 2. No CIDR gating: a request claiming a non-loopback RemoteAddr must
	// be served identically to one from loopback -- default mode's only
	// gate is withAuth (capability token), which NoAuth: true bypasses
	// entirely here, so both should succeed with 200.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.7:54321" // TEST-NET-3, deliberately non-loopback
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("GET / from non-loopback RemoteAddr status = %d, want 200 (default mode must not CIDR-gate)", rec.Code)
	}
}

// TestDefaultServerAuthUnaffectedByNonLoopbackRemoteAddr confirms the same
// non-CIDR-gating invariant holds when auth IS enabled: a non-loopback
// RemoteAddr changes nothing about withAuth's token requirement -- no token
// still gets 401 (never a CIDR-driven 403), and a valid token still
// succeeds, exactly as if the request had come from loopback.
func TestDefaultServerAuthUnaffectedByNonLoopbackRemoteAddr(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{
		Dir:         dir,
		Host:        "127.0.0.1",
		Port:        0,
		IdleTimeout: time.Hour,
		NoAuth:      false,
	}
	s := New(cfg)
	s.token = testToken

	routes := s.routes()

	// No token: 401, not 403 -- proves the gate is still withAuth, not a
	// CIDR check that would 403 instead.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.7:54321"
	rec := httptest.NewRecorder()
	routes.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("GET / (no token, non-loopback RemoteAddr) status = %d, want 401", rec.Code)
	}

	// Valid token: succeeds despite the non-loopback RemoteAddr.
	req2 := httptest.NewRequest(http.MethodGet, "/?t="+testToken, nil)
	req2.RemoteAddr = "203.0.113.7:54321"
	rec2 := httptest.NewRecorder()
	routes.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusFound {
		t.Errorf("GET /?t=<valid> (non-loopback RemoteAddr) status = %d, want 302 (token-stripping redirect)", rec2.Code)
	}
}
