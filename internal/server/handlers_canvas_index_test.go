package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jedwards1230/scrim/internal/oidc"
)

// bareCanvasPaths are the two id-less canvas URLs that must land on the
// gallery: /c and /c/ (#30).
var bareCanvasPaths = []string{"/c", "/c/"}

// TestBareCanvasPathRedirectsToIndex covers the daemon and the hub in one
// table: both modes build their handler from the SAME routes(), so the
// redirect must be present in each. Hub mode is exercised through its own
// constructor (NewHub + withHubGate) rather than asserted by inspection --
// a loopback request past the CIDR read gate proves the route is reachable
// there too, not merely registered.
func TestBareCanvasPathRedirectsToIndex(t *testing.T) {
	_, daemonTS := newTestServer(t)
	_, hubTS := newHubTestServer(t, []string{"127.0.0.0/8"}, "")

	modes := []struct {
		name string
		url  string
	}{
		{name: "daemon", url: daemonTS.URL},
		{name: "hub", url: hubTS.URL},
	}

	client := noRedirectClient()
	for _, mode := range modes {
		for _, path := range bareCanvasPaths {
			t.Run(mode.name+" "+path, func(t *testing.T) {
				resp, err := client.Get(mode.url + path)
				if err != nil {
					t.Fatalf("GET %s: %v", path, err)
				}
				defer func() { _ = resp.Body.Close() }()

				// 302, deliberately not 301: /c is not a stable resource, and a
				// permanent redirect would be cached by browsers indefinitely.
				if resp.StatusCode != http.StatusFound {
					t.Errorf("GET %s status = %d, want %d", path, resp.StatusCode, http.StatusFound)
				}
				if loc := resp.Header.Get("Location"); loc != "/" {
					t.Errorf("GET %s Location = %q, want %q", path, loc, "/")
				}
			})
		}
	}
}

// TestBareCanvasPathRedirectLeavesCanvasRoutesIntact pins the routes the new
// patterns sit next to: the /c/{id} trailing-slash normalization, the canvas
// view itself, the SSE endpoint, and the per-canvas favicon must all behave
// exactly as before -- verified by request, not by reading routes.go.
func TestBareCanvasPathRedirectLeavesCanvasRoutesIntact(t *testing.T) {
	s, ts := newTestServer(t)
	canvasDir := filepath.Join(s.canvasesDir, "report")
	if err := os.MkdirAll(canvasDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canvasDir, "index.html"), []byte("<html><body>hi</body></html>"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		path       string
		wantStatus int
		headerName string
		wantHeader string
	}{
		{
			name:       "canvas id without trailing slash still 301s to the slashed form",
			path:       "/c/report",
			wantStatus: http.StatusMovedPermanently,
			headerName: "Location",
			wantHeader: "/c/report/",
		},
		{
			name:       "canvas view still served",
			path:       "/c/report/",
			wantStatus: http.StatusOK,
			headerName: "Content-Type",
			wantHeader: "text/html; charset=utf-8",
		},
		{
			name:       "SSE endpoint still wins over the static wildcard",
			path:       "/c/report/__events",
			wantStatus: http.StatusOK,
			headerName: "Content-Type",
			wantHeader: "text/event-stream",
		},
		{
			name:       "per-canvas favicon still generated",
			path:       "/c/report/favicon.ico",
			wantStatus: http.StatusOK,
			headerName: "Content-Type",
			wantHeader: "image/svg+xml",
		},
	}

	client := noRedirectClient()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := client.Get(ts.URL + tt.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tt.path, err)
			}
			// The SSE response body is an open stream; closing it ends the
			// handler's write loop.
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != tt.wantStatus {
				t.Errorf("GET %s status = %d, want %d", tt.path, resp.StatusCode, tt.wantStatus)
			}
			if got := resp.Header.Get(tt.headerName); got != tt.wantHeader {
				t.Errorf("GET %s %s = %q, want %q", tt.path, tt.headerName, got, tt.wantHeader)
			}
		})
	}
}

// TestBareCanvasPathRedirectUnderOIDC pins how the identity plane classifies
// the id-less paths on an OIDC hub: they carry no canvas id
// (canvasIDFromURLPath returns ok=false for both), so they are general
// authenticated reads exactly like the gallery they point at -- an
// unauthenticated browser is sent to login, never to "/", and only a real
// session gets the redirect.
func TestBareCanvasPathRedirectUnderOIDC(t *testing.T) {
	s, auth, idp := newOIDCHub(t)
	session := idp.Login(t, auth, "/")

	for _, path := range bareCanvasPaths {
		t.Run(path, func(t *testing.T) {
			anon := httptest.NewRequest(http.MethodGet, path, nil)
			anon.Header.Set("Accept", "text/html")
			rec := httptest.NewRecorder()
			s.routes().ServeHTTP(rec, anon)
			if rec.Code != http.StatusFound {
				t.Fatalf("GET %s (anonymous) status = %d, want 302 into the login flow", path, rec.Code)
			}
			if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, oidc.LoginPath+"?return_to=") {
				t.Errorf("GET %s (anonymous) Location = %q, want the login flow", path, loc)
			}

			authed := httptest.NewRequest(http.MethodGet, path, nil)
			authed.Header.Set("Accept", "text/html")
			authed.AddCookie(session)
			rec = httptest.NewRecorder()
			s.routes().ServeHTTP(rec, authed)
			if rec.Code != http.StatusFound {
				t.Fatalf("GET %s (session) status = %d, want 302", path, rec.Code)
			}
			if loc := rec.Header().Get("Location"); loc != "/" {
				t.Errorf("GET %s (session) Location = %q, want %q", path, loc, "/")
			}
		})
	}
}

// TestBareCanvasPathRedirectRespectsAuth checks the new routes inherit the
// daemon's capability-token gate exactly like every other browser-facing
// route: no credential is a 401 (never a redirect that would leak the
// gallery's existence past the gate), a valid "?t=" is answered by the gate's
// own token-stripping 302 (the mux is not reached at all), and only the
// cookie-authenticated follow-up gets the redirect to "/".
func TestBareCanvasPathRedirectRespectsAuth(t *testing.T) {
	_, ts := newAuthTestServer(t)
	client := noRedirectClient()

	for _, path := range bareCanvasPaths {
		t.Run(path, func(t *testing.T) {
			// 1. No credential at all.
			resp, err := client.Get(ts.URL + path)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("GET %s (no token) status = %d, want 401", path, resp.StatusCode)
			}

			// 2. A valid query token: the auth middleware answers first with the
			// token-stripping redirect back to the same path, setting the cookie.
			req, err := http.NewRequest(http.MethodGet, ts.URL+path+"?t="+testToken, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err = client.Do(req)
			if err != nil {
				t.Fatalf("GET %s?t=: %v", path, err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusFound {
				t.Fatalf("GET %s?t= status = %d, want 302", path, resp.StatusCode)
			}
			if loc := resp.Header.Get("Location"); loc != path {
				t.Errorf("GET %s?t= Location = %q, want %q (token stripped, same path)", path, loc, path)
			}
			var authCookie *http.Cookie
			for _, c := range resp.Cookies() {
				if c.Name == authCookieName {
					authCookie = c
				}
			}
			if authCookie == nil {
				t.Fatalf("GET %s?t= set no %s cookie", path, authCookieName)
			}

			// 3. The cookie-authenticated follow-up reaches the mux and lands on
			// the gallery.
			req, err = http.NewRequest(http.MethodGet, ts.URL+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.AddCookie(authCookie)
			resp, err = client.Do(req)
			if err != nil {
				t.Fatalf("GET %s (cookie): %v", path, err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusFound {
				t.Errorf("GET %s (cookie) status = %d, want 302", path, resp.StatusCode)
			}
			if loc := resp.Header.Get("Location"); loc != "/" {
				t.Errorf("GET %s (cookie) Location = %q, want %q", path, loc, "/")
			}
		})
	}
}
