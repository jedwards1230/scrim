package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/scrim/internal/config"
	"github.com/jedwards1230/scrim/internal/oidc"
)

// The whole point of /logged-out is that someone with no session can read it --
// they have just had theirs destroyed. If the gate ever starts intercepting it,
// logout bounces the user into the login form and the page is worse than
// useless.
func TestLoggedOutPageServedToAnonymousBrowser(t *testing.T) {
	s, _, _ := newOIDCHub(t)

	req := httptest.NewRequest(http.MethodGet, loggedOutPath, nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200 (got Location=%q)",
			loggedOutPath, rec.Code, rec.Header().Get("Location"))
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "signed out") && !strings.Contains(body, "Sign back in") {
		t.Errorf("body does not look like the signed-out page: %q", body)
	}
	// A link back into the login flow is the page's only affordance; without it
	// the user is stranded on a dead end.
	if !strings.Contains(body, "/auth/login") {
		t.Errorf("body missing the login link, got: %q", body)
	}
	// Serving a cached copy to the next visitor would wrongly tell them their
	// session had ended.
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
}

// The exemption must be an EXACT match, not a prefix -- the same hazard #47
// covered for /healthz. A prefix would hand out anonymous access to anything
// under /logged-out/.
func TestLoggedOutExemptionIsExactNotPrefix(t *testing.T) {
	s, _, _ := newOIDCHub(t)

	for _, path := range []string{
		loggedOutPath + "/",
		loggedOutPath + "/secret",
		loggedOutPath + "x",
		"/logged-outsomething",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Accept", "text/html")
		rec := httptest.NewRecorder()
		s.routes().ServeHTTP(rec, req)

		// Assert the gate INTERCEPTED it, not merely that it wasn't a 200. A
		// bare "not 200" would also pass on a 404 -- which is what you get when
		// the request slips PAST the gate and the mux finds no route, i.e. the
		// exact failure this test exists to catch. Mirrors
		// TestHubOIDCOnlyExactAuthPathsAreExempt.
		if rec.Code != http.StatusFound {
			t.Errorf("GET %s status = %d, want 302 (gated, not exempt)", path, rec.Code)
			continue
		}
		if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, oidc.LoginPath) {
			t.Errorf("GET %s redirect Location = %q, want the login flow (gated)", path, loc)
		}
	}
}

// On a hub with no OIDC there is no logout round-trip to land from, so the route
// is never registered and its exemption must be inert.
func TestLoggedOutNotRegisteredWithoutOIDC(t *testing.T) {
	cfg := config.Config{Dir: t.TempDir(), Host: "127.0.0.1", Port: 0, IdleTimeout: time.Hour, NoAuth: true}
	s, err := NewHub(cfg, HubOptions{PushToken: "test-push-token"})
	if err != nil {
		t.Fatalf("NewHub() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, loggedOutPath, nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)

	if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "Sign back in") {
		t.Errorf("non-OIDC hub served the signed-out page; it should not be registered")
	}
}
