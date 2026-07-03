package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jedwards1230/scrim/internal/config"
)

const testToken = "test-capability-token-0123456789abcdef"

// newAuthTestServer returns a Server with auth enabled and a known,
// deterministic token (bypassing Run(), which would otherwise mint a random
// one), plus one real canvas ("report") so tests can exercise the static
// canvas route and the SSE route, not just the index page.
func newAuthTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
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

	canvasDir := filepath.Join(s.canvasesDir, "report")
	if err := os.MkdirAll(canvasDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canvasDir, "index.html"), []byte("<html><body>hi</body></html>"), 0o644); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)
	return s, ts
}

// TestConstantTimeEqual exercises constantTimeEqual for correctness. The
// timing-attack resistance itself can't be asserted in a unit test (there's
// no reliable way to measure constant-time behavior here); that property
// comes from delegating to crypto/subtle.ConstantTimeCompare directly (see
// the implementation and its doc comment in auth.go) rather than a plain
// "==" or bytes.Equal comparison.
func TestConstantTimeEqual(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{name: "equal", a: "abc123", b: "abc123", want: true},
		{name: "different content, same length", a: "abc123", b: "abc124", want: false},
		{name: "different length", a: "abc", b: "abc123", want: false},
		{name: "both empty", a: "", b: "", want: true},
		{name: "one empty", a: "", b: "abc123", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := constantTimeEqual(tt.a, tt.b); got != tt.want {
				t.Errorf("constantTimeEqual(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

// TestWithAuthAcceptRejectMatrix is the accept/reject matrix for the auth
// middleware: a valid token in the query, a valid cookie, an invalid token,
// and missing-everything, each run against a representative sample of every
// kind of route the daemon serves -- the index page, a static canvas asset,
// and the SSE endpoint -- since the middleware must gate all of them, not
// just /api/*.
func TestWithAuthAcceptRejectMatrix(t *testing.T) {
	_, ts := newAuthTestServer(t)

	routes := []string{"/", "/c/report/", "/c/report/__events"}

	tests := []struct {
		name       string
		query      string
		cookie     *http.Cookie
		wantStatus int
	}{
		{
			name:       "valid token in query",
			query:      "?t=" + testToken,
			wantStatus: http.StatusOK,
		},
		{
			name:       "invalid token in query",
			query:      "?t=wrong-token",
			wantStatus: http.StatusUnauthorized,
		},
		{
			// Regression test for the documented interaction in auth.go: a
			// present-but-wrong "?t=" is rejected even when the request also
			// carries an otherwise-valid session cookie -- it must not fall
			// back to cookie auth.
			name:       "invalid token in query with an otherwise-valid cookie",
			query:      "?t=wrong-token",
			cookie:     &http.Cookie{Name: authCookieName, Value: testToken},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "valid cookie, no query token",
			cookie:     &http.Cookie{Name: authCookieName, Value: testToken},
			wantStatus: http.StatusOK,
		},
		{
			name:       "invalid cookie, no query token",
			cookie:     &http.Cookie{Name: authCookieName, Value: "wrong-token"},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "missing everything",
			wantStatus: http.StatusUnauthorized,
		},
	}

	client := &http.Client{Timeout: 2 * time.Second}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, route := range routes {
				req, err := http.NewRequest(http.MethodGet, ts.URL+route+tt.query, nil)
				if err != nil {
					t.Fatal(err)
				}
				if tt.cookie != nil {
					req.AddCookie(tt.cookie)
				}
				resp, err := client.Do(req)
				if err != nil {
					t.Fatalf("GET %s: %v", route, err)
				}
				_ = resp.Body.Close()
				if resp.StatusCode != tt.wantStatus {
					t.Errorf("GET %s%s status = %d, want %d", route, tt.query, resp.StatusCode, tt.wantStatus)
				}
			}
		})
	}
}

// TestWithAuthQueryTokenSetsCookie confirms a valid ?t= hit sets the auth
// cookie, so a browser's subsequent same-origin requests (including its own
// EventSource request against the SSE endpoint, and requests for injected
// static assets) don't need the query param repeated.
func TestWithAuthQueryTokenSetsCookie(t *testing.T) {
	_, ts := newAuthTestServer(t)

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(ts.URL + "/?t=" + testToken)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var found *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == authCookieName {
			found = c
			break
		}
	}
	if found == nil {
		t.Fatal("response did not set the auth cookie")
	}
	if found.Value != testToken {
		t.Errorf("cookie value = %q, want %q", found.Value, testToken)
	}
}

// TestWithAuthNoAuthBypassesGating confirms --no-auth disables the auth
// check entirely: even the SSE endpoint (which is otherwise gated like
// everything else) must serve a request with no token and no cookie at all.
func TestWithAuthNoAuthBypassesGating(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{
		Dir:         dir,
		Host:        "127.0.0.1",
		Port:        0,
		IdleTimeout: time.Hour,
		NoAuth:      true,
	}
	s := New(cfg)
	canvasDir := filepath.Join(s.canvasesDir, "report")
	if err := os.MkdirAll(canvasDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canvasDir, "index.html"), []byte("<html><body>hi</body></html>"), 0o644); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)

	client := &http.Client{Timeout: 2 * time.Second}
	for _, route := range []string{"/", "/c/report/", "/c/report/__events"} {
		resp, err := client.Get(ts.URL + route)
		if err != nil {
			t.Fatalf("GET %s: %v", route, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s with --no-auth status = %d, want 200 (auth should be fully bypassed)", route, resp.StatusCode)
		}
	}
}
