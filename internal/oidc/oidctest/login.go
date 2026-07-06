package oidctest

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jedwards1230/scrim/internal/oidc"
)

// Login drives a complete browser-less authorization-code login against this
// IdP and returns the resulting session cookie. It runs auth.HandleLogin,
// follows the authorization redirect through the IdP (which mints a code),
// and runs auth.HandleCallback -- exercising state, nonce, and PKCE exactly
// as a real browser round-trip would. t fails the test on any unexpected
// step, so a returned cookie is always a valid, minted session.
func (i *IdP) Login(t *testing.T, auth *oidc.Authenticator, returnTo string) *http.Cookie {
	t.Helper()

	// Step 1: initiate login. HandleLogin sets the flow cookie and 302s to the
	// IdP's authorization endpoint.
	loginURL := oidc.LoginPath
	if returnTo != "" {
		loginURL += "?return_to=" + returnTo
	}
	loginReq := httptest.NewRequest(http.MethodGet, loginURL, nil)
	loginRec := httptest.NewRecorder()
	auth.HandleLogin(loginRec, loginReq)
	if loginRec.Code != http.StatusFound {
		t.Fatalf("HandleLogin status = %d, want 302", loginRec.Code)
	}
	authorizeURL := loginRec.Header().Get("Location")
	flowCookie := findCookie(loginRec.Result().Cookies(), "scrim_oidc_flow")
	if flowCookie == nil {
		t.Fatal("HandleLogin set no flow cookie")
	}

	// Step 2: follow the redirect to the IdP's /authorize, which 302s back to
	// the client's redirect_uri with code+state. Don't auto-follow that final
	// hop (its host is the hub, which isn't listening here).
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	authzResp, err := client.Get(authorizeURL)
	if err != nil {
		t.Fatalf("GET authorize endpoint: %v", err)
	}
	defer func() { _ = authzResp.Body.Close() }()
	if authzResp.StatusCode != http.StatusFound {
		t.Fatalf("authorize status = %d, want 302", authzResp.StatusCode)
	}
	callbackLocation, err := authzResp.Location()
	if err != nil {
		t.Fatalf("authorize returned no Location: %v", err)
	}

	// Step 3: deliver the callback to the hub, carrying the flow cookie the
	// browser would still hold.
	cbReq := httptest.NewRequest(http.MethodGet, oidc.CallbackPath+"?"+callbackLocation.RawQuery, nil)
	cbReq.AddCookie(flowCookie)
	cbRec := httptest.NewRecorder()
	auth.HandleCallback(cbRec, cbReq)
	if cbRec.Code != http.StatusFound {
		t.Fatalf("HandleCallback status = %d, want 302 (body: %q)", cbRec.Code, cbRec.Body.String())
	}
	session := findCookie(cbRec.Result().Cookies(), "scrim_session")
	if session == nil || session.Value == "" {
		t.Fatal("HandleCallback minted no session cookie")
	}
	return session
}

// CallbackLocation runs steps 1-2 of a login and returns the flow cookie and
// the callback query string (code+state) WITHOUT delivering the callback, so
// a test can tamper with state/nonce/code before calling HandleCallback
// itself.
func (i *IdP) CallbackLocation(t *testing.T, auth *oidc.Authenticator) (flow *http.Cookie, callbackQuery string) {
	t.Helper()
	loginReq := httptest.NewRequest(http.MethodGet, oidc.LoginPath, nil)
	loginRec := httptest.NewRecorder()
	auth.HandleLogin(loginRec, loginReq)
	flow = findCookie(loginRec.Result().Cookies(), "scrim_oidc_flow")
	if flow == nil {
		t.Fatal("HandleLogin set no flow cookie")
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(loginRec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("GET authorize endpoint: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	loc, err := resp.Location()
	if err != nil {
		t.Fatalf("authorize returned no Location: %v", err)
	}
	return flow, loc.RawQuery
}

func findCookie(cookies []*http.Cookie, name string) *http.Cookie {
	for _, c := range cookies {
		if c.Name == name {
			return c
		}
	}
	return nil
}
