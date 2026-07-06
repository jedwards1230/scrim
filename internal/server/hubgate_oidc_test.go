package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/scrim/internal/config"
	"github.com/jedwards1230/scrim/internal/oidc"
	"github.com/jedwards1230/scrim/internal/oidc/oidctest"
)

// oidcTestSecret is shared between the hub's own authenticator and the
// parallel one the test uses to mint session cookies, so a cookie minted by
// the latter verifies against the former (both sign with the same HMAC key).
var oidcTestSecret = []byte("server-gate-oidc-test-secret-key")

// newOIDCHub builds a hub with OIDC configured against a fresh fake IdP, plus
// a parallel Authenticator (same signing secret + IdP) the test uses to mint
// real session cookies.
func newOIDCHub(t *testing.T) (*Server, *oidc.Authenticator, *oidctest.IdP) {
	t.Helper()
	idp := oidctest.New(t)
	oidcCfg := oidc.Config{
		IssuerURL:     idp.Issuer(),
		ClientID:      idp.ClientID(),
		ClientSecret:  idp.ClientSecret(),
		RedirectURL:   "https://hub.test/auth/callback",
		SessionSecret: oidcTestSecret,
		SecureCookies: false,
	}
	cfg := config.Config{Dir: t.TempDir(), Host: "127.0.0.1", Port: 0, IdleTimeout: time.Hour, NoAuth: true}
	s, err := NewHub(cfg, HubOptions{PushToken: "test-push-token", OIDC: &oidcCfg})
	if err != nil {
		t.Fatalf("NewHub with OIDC error = %v", err)
	}
	auth, err := oidc.New(context.Background(), oidcCfg)
	if err != nil {
		t.Fatalf("parallel oidc.New error = %v", err)
	}
	return s, auth, idp
}

func TestHubOIDCUnauthenticatedBrowserRedirectsToLogin(t *testing.T) {
	s, _, _ := newOIDCHub(t)

	req := httptest.NewRequest(http.MethodGet, "/c/demo/", nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("unauthenticated browser read status = %d, want 302", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if want := oidc.LoginPath + "?return_to="; !strings.HasPrefix(loc, want) {
		t.Errorf("redirect Location = %q, want a redirect to %q", loc, want)
	}
}

func TestHubOIDCUnauthenticatedNonBrowserGets401(t *testing.T) {
	s, _, _ := newOIDCHub(t)

	// An SSE EventSource (text/event-stream) or any non-HTML client must get a
	// 401 it can act on, never an HTML redirect it can't follow.
	req := httptest.NewRequest(http.MethodGet, "/c/demo/__events", nil)
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated SSE read status = %d, want 401", rec.Code)
	}
}

func TestHubOIDCValidSessionIsServed(t *testing.T) {
	s, auth, idp := newOIDCHub(t)
	session := idp.Login(t, auth, "/")

	// A read carrying the session cookie is served -- even from a non-loopback
	// RemoteAddr, proving OIDC (not the CIDR allowlist) is the read gate.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept", "text/html")
	req.RemoteAddr = "203.0.113.7:54321"
	req.AddCookie(session)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("authenticated read status = %d, want 200", rec.Code)
	}
}

func TestHubOIDCLoginRouteIsExemptFromGate(t *testing.T) {
	s, _, idp := newOIDCHub(t)

	// /auth/login must be reachable with no session (otherwise login is
	// impossible): the gate exempts it and it 302s to the IdP.
	req := httptest.NewRequest(http.MethodGet, oidc.LoginPath, nil)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("GET /auth/login status = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, idp.Issuer()) {
		t.Errorf("login redirect = %q, want a redirect to the IdP %q", loc, idp.Issuer())
	}
}

func TestHubOIDCOnlyExactAuthPathsAreExempt(t *testing.T) {
	s, _, _ := newOIDCHub(t)

	// A non-exact /auth/ path is NOT exempt: it's an ordinary unauthenticated
	// read, so it redirects to login rather than slipping past the gate. This
	// pins the exact-match exemption (a prefix match would wrongly let it by).
	req := httptest.NewRequest(http.MethodGet, "/auth/not-a-real-route", nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("GET /auth/not-a-real-route status = %d, want 302 (gated, not exempt)", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, oidc.LoginPath) {
		t.Errorf("redirect Location = %q, want a redirect to the login flow (gated)", loc)
	}
}

func TestHubOIDCWriteGateStillPushTokenOnly(t *testing.T) {
	s, _, _ := newOIDCHub(t)

	// A write with no push token is 401 regardless of OIDC -- the write gate
	// is unchanged and OIDC never applies to it.
	noToken := httptest.NewRequest(http.MethodPost, "/api/push/foo", http.NoBody)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, noToken)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("push with no token status = %d, want 401", rec.Code)
	}

	// A write WITH the push token proceeds (no OIDC session required). An empty
	// body is a valid empty archive, so this succeeds.
	withToken := httptest.NewRequest(http.MethodPost, "/api/push/foo", http.NoBody)
	withToken.Header.Set("Authorization", "Bearer test-push-token")
	rec2 := httptest.NewRecorder()
	s.routes().ServeHTTP(rec2, withToken)
	if rec2.Code == http.StatusUnauthorized || rec2.Code == http.StatusForbidden {
		t.Errorf("push with valid token status = %d, want it not gated (got an auth rejection)", rec2.Code)
	}
}

// TestHubWithoutOIDCIsUnchanged is the opt-in regression: a hub with no OIDC
// config has a nil oidcAuth and still enforces exactly the CIDR read gate it
// always did -- a non-loopback read is 403, never redirected to a login that
// doesn't exist.
func TestHubWithoutOIDCIsUnchanged(t *testing.T) {
	cfg := config.Config{Dir: t.TempDir(), Host: "127.0.0.1", Port: 0, IdleTimeout: time.Hour, NoAuth: true}
	s, err := NewHub(cfg, HubOptions{PushToken: "test-push-token", AllowCIDRs: []string{"127.0.0.0/8"}})
	if err != nil {
		t.Fatalf("NewHub error = %v", err)
	}
	if s.oidcAuth != nil {
		t.Fatal("hub with no OIDC config has a non-nil oidcAuth, want nil")
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept", "text/html")
	req.RemoteAddr = "203.0.113.7:54321" // non-loopback, outside the allowlist
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("non-OIDC hub, non-loopback read status = %d, want 403 (CIDR gate unchanged)", rec.Code)
	}

	// A login route must not exist on a non-OIDC hub.
	loginReq := httptest.NewRequest(http.MethodGet, oidc.LoginPath, nil)
	loginReq.RemoteAddr = "127.0.0.1:1234"
	loginRec := httptest.NewRecorder()
	s.routes().ServeHTTP(loginRec, loginReq)
	if loginRec.Code != http.StatusNotFound {
		t.Errorf("GET /auth/login on a non-OIDC hub status = %d, want 404 (route not registered)", loginRec.Code)
	}
}
