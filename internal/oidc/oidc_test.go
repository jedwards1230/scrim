package oidc_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/scrim/internal/oidc"
	"github.com/jedwards1230/scrim/internal/oidc/oidctest"
)

const testRedirectURL = "https://hub.test/auth/callback"

// newAuth builds an Authenticator wired to a fresh fake IdP, returning both.
func newAuth(t *testing.T) (*oidc.Authenticator, *oidctest.IdP) {
	t.Helper()
	idp := oidctest.New(t)
	auth, err := oidc.New(context.Background(), oidc.Config{
		IssuerURL:     idp.Issuer(),
		ClientID:      idp.ClientID(),
		ClientSecret:  idp.ClientSecret(),
		RedirectURL:   testRedirectURL,
		SessionSecret: []byte("deterministic-test-session-secret"),
	})
	if err != nil {
		t.Fatalf("oidc.New error = %v", err)
	}
	return auth, idp
}

func TestNewFailsClosed(t *testing.T) {
	idp := oidctest.New(t)
	base := oidc.Config{
		IssuerURL:    idp.Issuer(),
		ClientID:     idp.ClientID(),
		ClientSecret: idp.ClientSecret(),
		RedirectURL:  testRedirectURL,
	}
	t.Run("missing issuer", func(t *testing.T) {
		c := base
		c.IssuerURL = ""
		if _, err := oidc.New(context.Background(), c); err == nil {
			t.Error("New with no issuer error = nil, want an error")
		}
	})
	t.Run("missing client id", func(t *testing.T) {
		c := base
		c.ClientID = ""
		if _, err := oidc.New(context.Background(), c); err == nil {
			t.Error("New with no client id error = nil, want an error")
		}
	})
	t.Run("missing client secret", func(t *testing.T) {
		c := base
		c.ClientSecret = ""
		if _, err := oidc.New(context.Background(), c); err == nil {
			t.Error("New with no client secret error = nil, want an error")
		}
	})
	t.Run("missing redirect url", func(t *testing.T) {
		c := base
		c.RedirectURL = ""
		if _, err := oidc.New(context.Background(), c); err == nil {
			t.Error("New with no redirect url error = nil, want an error")
		}
	})
	t.Run("relative redirect url", func(t *testing.T) {
		c := base
		c.RedirectURL = "/auth/callback" // no scheme/host -- fails closed at boot
		if _, err := oidc.New(context.Background(), c); err == nil {
			t.Error("New with a relative redirect url error = nil, want an error")
		}
	})
	t.Run("hostless redirect url", func(t *testing.T) {
		c := base
		c.RedirectURL = "https:///auth/callback" // scheme but no host
		if _, err := oidc.New(context.Background(), c); err == nil {
			t.Error("New with a host-less redirect url error = nil, want an error")
		}
	})
	t.Run("unreachable issuer", func(t *testing.T) {
		c := base
		c.IssuerURL = "https://127.0.0.1:1/nonexistent"
		if _, err := oidc.New(context.Background(), c); err == nil {
			t.Error("New with an unreachable issuer error = nil, want an error")
		}
	})
	t.Run("short session secret", func(t *testing.T) {
		c := base
		c.SessionSecret = []byte("too-short") // non-empty but < 32 bytes
		if _, err := oidc.New(context.Background(), c); err == nil {
			t.Error("New with a short session secret error = nil, want a rejection")
		}
	})
}

func TestLoginRedirectsToIdPWithSecurityParams(t *testing.T) {
	auth, idp := newAuth(t)

	req := httptest.NewRequest(http.MethodGet, oidc.LoginPath+"?return_to=/c/x/", nil)
	rec := httptest.NewRecorder()
	auth.HandleLogin(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("HandleLogin status = %d, want 302", rec.Code)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parsing Location: %v", err)
	}
	if got := loc.Scheme + "://" + loc.Host + loc.Path; got != idp.Issuer()+"/authorize" {
		t.Errorf("redirect target = %q, want the IdP authorize endpoint", got)
	}
	q := loc.Query()
	for _, param := range []string{"state", "nonce", "code_challenge"} {
		if q.Get(param) == "" {
			t.Errorf("authorize URL missing %q", param)
		}
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Errorf("code_challenge_method = %q, want S256 (PKCE)", q.Get("code_challenge_method"))
	}
	if q.Get("response_type") != "code" {
		t.Errorf("response_type = %q, want code", q.Get("response_type"))
	}

	// The flow cookie is set, HttpOnly and SameSite=Lax (so it survives the
	// IdP redirect yet is withheld cross-site).
	flow := findCookie(rec.Result().Cookies(), "scrim_oidc_flow")
	if flow == nil {
		t.Fatal("HandleLogin set no flow cookie")
	}
	if !flow.HttpOnly {
		t.Error("flow cookie is not HttpOnly")
	}
	if flow.SameSite != http.SameSiteLaxMode {
		t.Errorf("flow cookie SameSite = %v, want Lax", flow.SameSite)
	}
}

func TestHappyPathLoginAuthenticatesSession(t *testing.T) {
	auth, idp := newAuth(t)
	idp.Subject = "user-happy-path"

	session := idp.Login(t, auth, "/c/x/")

	// The minted session cookie authenticates a subsequent request.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(session)
	sess, ok := auth.SessionFromRequest(req)
	if !ok {
		t.Fatal("SessionFromRequest with the minted cookie = not ok, want ok")
	}
	if sess.Subject != "user-happy-path" {
		t.Errorf("subject = %q, want %q", sess.Subject, "user-happy-path")
	}
	if !session.HttpOnly {
		t.Error("session cookie is not HttpOnly")
	}
}

// TestCallbackCapturesClaimsAndFeedsRegistry proves HandleCallback captures the
// email/name/groups claims into the session and fires the OnLogin hook with
// them -- the #49 identity-capture requirement.
func TestCallbackCapturesClaimsAndFeedsRegistry(t *testing.T) {
	idp := oidctest.New(t)
	idp.Subject = "sub-1"
	idp.Email = "alice@example.com"
	idp.Name = "Alice"
	idp.Groups = []string{"eng", "ops"}

	var gotEmail, gotName string
	var gotGroups []string
	auth, err := oidc.New(context.Background(), oidc.Config{
		IssuerURL:     idp.Issuer(),
		ClientID:      idp.ClientID(),
		ClientSecret:  idp.ClientSecret(),
		RedirectURL:   testRedirectURL,
		SessionSecret: []byte("deterministic-test-session-secret"),
		OnLogin: func(email, name string, groups []string) {
			gotEmail, gotName, gotGroups = email, name, groups
		},
	})
	if err != nil {
		t.Fatalf("oidc.New error = %v", err)
	}

	session := idp.Login(t, auth, "/")

	if gotEmail != "alice@example.com" || gotName != "Alice" {
		t.Errorf("OnLogin got email/name = %q/%q, want alice@example.com/Alice", gotEmail, gotName)
	}
	if len(gotGroups) != 2 || gotGroups[0] != "eng" || gotGroups[1] != "ops" {
		t.Errorf("OnLogin got groups = %v, want [eng ops]", gotGroups)
	}

	// The minted session cookie carries the same claims back.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(session)
	sess, ok := auth.SessionFromRequest(req)
	if !ok {
		t.Fatal("SessionFromRequest = not ok, want ok")
	}
	if sess.Email != "alice@example.com" || sess.Name != "Alice" {
		t.Errorf("session claims = %+v, want alice@example.com/Alice", sess)
	}
	if len(sess.Groups) != 2 {
		t.Errorf("session groups = %v, want [eng ops]", sess.Groups)
	}
}

// TestCallbackWithoutGroupsStillLogsIn proves an IdP that omits name/groups (a
// bare openid+email token) yields empty profile fields, not a failed login.
func TestCallbackWithoutGroupsStillLogsIn(t *testing.T) {
	auth, idp := newAuth(t) // idp.Name/Groups unset -> omitted from the token
	session := idp.Login(t, auth, "/")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(session)
	sess, ok := auth.SessionFromRequest(req)
	if !ok {
		t.Fatal("SessionFromRequest = not ok, want ok")
	}
	if len(sess.Groups) != 0 || sess.Name != "" {
		t.Errorf("session = %+v, want empty name/groups (IdP omitted them)", sess)
	}
	// email defaults in the fake IdP, so it is present; the point is the login
	// succeeded with no groups claim.
	if sess.Subject == "" {
		t.Error("session has no subject, want the login to have succeeded")
	}
}

func TestCallbackRejectsStateMismatch(t *testing.T) {
	auth, idp := newAuth(t)
	flow, callbackQuery := idp.CallbackLocation(t, auth)

	// Replace the state with an attacker-chosen value the flow cookie won't
	// match.
	q, _ := url.ParseQuery(callbackQuery)
	q.Set("state", "attacker-supplied-state")
	req := httptest.NewRequest(http.MethodGet, oidc.CallbackPath+"?"+q.Encode(), nil)
	req.AddCookie(flow)
	rec := httptest.NewRecorder()
	auth.HandleCallback(rec, req)

	assertNoSession(t, rec, "state mismatch")
}

func TestCallbackRejectsNonceMismatch(t *testing.T) {
	auth, idp := newAuth(t)
	// The IdP will embed a nonce that doesn't match the one bound in the flow
	// cookie -- an ID-token-injection style attack.
	idp.ForceNonce = "not-the-real-nonce"
	flow, callbackQuery := idp.CallbackLocation(t, auth)

	req := httptest.NewRequest(http.MethodGet, oidc.CallbackPath+"?"+callbackQuery, nil)
	req.AddCookie(flow)
	rec := httptest.NewRecorder()
	auth.HandleCallback(rec, req)

	assertNoSession(t, rec, "nonce mismatch")
}

func TestCallbackRejectsMissingIDToken(t *testing.T) {
	auth, idp := newAuth(t)
	idp.OmitIDToken = true
	flow, callbackQuery := idp.CallbackLocation(t, auth)

	req := httptest.NewRequest(http.MethodGet, oidc.CallbackPath+"?"+callbackQuery, nil)
	req.AddCookie(flow)
	rec := httptest.NewRecorder()
	auth.HandleCallback(rec, req)

	assertNoSession(t, rec, "missing id_token")
}

func TestCallbackRejectsMissingFlowCookie(t *testing.T) {
	auth, idp := newAuth(t)
	_, callbackQuery := idp.CallbackLocation(t, auth)

	// Deliver the callback with no flow cookie at all.
	req := httptest.NewRequest(http.MethodGet, oidc.CallbackPath+"?"+callbackQuery, nil)
	rec := httptest.NewRecorder()
	auth.HandleCallback(rec, req)

	assertNoSession(t, rec, "missing flow cookie")
}

func TestSessionFromRequestRejectsForeignSecret(t *testing.T) {
	// A session minted by one authenticator must not authenticate against
	// another with a different signing secret.
	auth1, idp := newAuth(t)
	session := idp.Login(t, auth1, "/")

	// A second authenticator with a deliberately different signing secret.
	authOther, err := oidc.New(context.Background(), oidc.Config{
		IssuerURL:     idp.Issuer(),
		ClientID:      idp.ClientID(),
		ClientSecret:  idp.ClientSecret(),
		RedirectURL:   testRedirectURL,
		SessionSecret: []byte("a-completely-different-secret-value-32b"),
	})
	if err != nil {
		t.Fatalf("oidc.New (other) error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(session)
	if _, ok := authOther.SessionFromRequest(req); ok {
		t.Error("SessionFromRequest accepted a cookie signed with a different secret")
	}
}

func TestSessionFromRequestNoCookie(t *testing.T) {
	auth, _ := newAuth(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if _, ok := auth.SessionFromRequest(req); ok {
		t.Error("SessionFromRequest with no cookie = ok, want not ok")
	}
}

func TestLogoutClearsSession(t *testing.T) {
	auth, _ := newAuth(t)
	req := httptest.NewRequest(http.MethodGet, oidc.LogoutPath, nil)
	rec := httptest.NewRecorder()
	auth.HandleLogout(rec, req)

	if rec.Code != http.StatusFound {
		t.Errorf("HandleLogout status = %d, want 302", rec.Code)
	}
	cleared := findCookie(rec.Result().Cookies(), "scrim_session")
	if cleared == nil || cleared.MaxAge >= 0 {
		t.Errorf("HandleLogout did not expire the session cookie (got %+v)", cleared)
	}
}

// logout runs HandleLogout with the given cookies attached and returns the
// recorder plus the parsed Location it redirected to.
func logout(t *testing.T, auth *oidc.Authenticator, cookies ...*http.Cookie) (*httptest.ResponseRecorder, *url.URL) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, oidc.LogoutPath, nil)
	for _, c := range cookies {
		if c != nil {
			req.AddCookie(c)
		}
	}
	rec := httptest.NewRecorder()
	auth.HandleLogout(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("HandleLogout status = %d, want 302", rec.Code)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("HandleLogout Location %q is not a URL: %v", rec.Header().Get("Location"), err)
	}
	return rec, loc
}

// TestLogoutRedirectsToEndSessionEndpoint is the regression test for the bug
// this feature fixes: a logout that only cleared scrim's own cookie left the
// IdP's SSO session intact, so the very next request was silently
// re-authenticated and the user saw the logout button "refresh them back in".
// A logout that redirects to "/" instead of the IdP is exactly that bug, so
// this test fails on it explicitly rather than only checking a status code.
func TestLogoutRedirectsToEndSessionEndpoint(t *testing.T) {
	auth, idp := newAuth(t)
	session, all := idp.LoginCookies(t, auth, "")

	_, loc := logout(t, auth, all...)

	if loc.Path == "" || loc.Host == "" {
		t.Fatalf("logout redirected to %q, want the IdP end-session endpoint -- a local-only redirect IS the bug being fixed", loc)
	}
	want, err := url.Parse(idp.EndSessionEndpoint())
	if err != nil {
		t.Fatal(err)
	}
	if loc.Host != want.Host || loc.Path != want.Path {
		t.Errorf("logout redirected to %s://%s%s, want %s://%s%s", loc.Scheme, loc.Host, loc.Path, want.Scheme, want.Host, want.Path)
	}
	// The discovered endpoint already carried ?realm=test; scrim must add its
	// parameters to that query, not replace it.
	if got := loc.Query().Get("realm"); got != "test" {
		t.Errorf("logout URL realm = %q, want %q -- the discovered endpoint's own query was clobbered", got, "test")
	}
	// The hint must be the real ID token from the login, so the IdP can end
	// that exact session without prompting.
	hint := loc.Query().Get("id_token_hint")
	if hint == "" {
		t.Fatal("logout URL has no id_token_hint, want the retained ID token")
	}
	if strings.Count(hint, ".") != 2 {
		t.Errorf("id_token_hint = %q, want a three-part JWT", hint)
	}
	// With a hint present, client_id is redundant and must not be sent.
	if got := loc.Query().Get("client_id"); got != "" {
		t.Errorf("logout URL client_id = %q, want it omitted when id_token_hint is present", got)
	}
	// Not configured by default -- sending an unregistered one is what IdPs
	// reject, so the default must omit it.
	if got := loc.Query().Get("post_logout_redirect_uri"); got != "" {
		t.Errorf("logout URL post_logout_redirect_uri = %q, want it omitted when unconfigured", got)
	}
	_ = session
}

// TestLogoutClearsBothCookies pins that the local session AND the retained ID
// token are expired, so nothing usable survives a logout locally either.
func TestLogoutClearsBothCookies(t *testing.T) {
	auth, idp := newAuth(t)
	_, all := idp.LoginCookies(t, auth, "")

	rec, _ := logout(t, auth, all...)

	for _, name := range []string{"scrim_session", "scrim_oidc_idt"} {
		cleared := findCookie(rec.Result().Cookies(), name)
		if cleared == nil {
			t.Errorf("logout did not clear cookie %s at all", name)
			continue
		}
		if cleared.MaxAge >= 0 || cleared.Value != "" {
			t.Errorf("logout left cookie %s alive (%+v), want it expired", name, cleared)
		}
	}
}

// TestLogoutWithoutSessionStaysLocal pins the open-redirector guard: a
// session-less logout (what a cross-site forged POST produces, since
// SameSite=Lax withholds the cookie) must not drive the browser to the IdP's
// end-session endpoint. It still clears cookies.
func TestLogoutWithoutSessionStaysLocal(t *testing.T) {
	auth, _ := newAuth(t)

	rec, loc := logout(t, auth)

	if loc.String() != "/" {
		t.Errorf("session-less logout redirected to %q, want %q -- scrim must not be an open IdP-logout redirector", loc, "/")
	}
	if cleared := findCookie(rec.Result().Cookies(), "scrim_session"); cleared == nil || cleared.MaxAge >= 0 {
		t.Error("session-less logout did not still clear the session cookie")
	}
}

// TestLogoutWithForgedSessionStaysLocal is the same guard against a cookie
// that is present but not signed by this hub.
func TestLogoutWithForgedSessionStaysLocal(t *testing.T) {
	auth, _ := newAuth(t)
	forged := &http.Cookie{Name: "scrim_session", Value: "not.a.valid.signature"}

	_, loc := logout(t, auth, forged)

	if loc.String() != "/" {
		t.Errorf("logout with a forged session redirected to %q, want %q", loc, "/")
	}
}

// TestLogoutWithoutEndSessionEndpointStaysLocal covers an IdP that advertises
// no RP-initiated logout: scrim clears what it owns and goes home rather than
// guessing a provider-shaped URL.
func TestLogoutWithoutEndSessionEndpointStaysLocal(t *testing.T) {
	idp := oidctest.New(t)
	idp.OmitEndSessionEndpoint = true
	auth, err := oidc.New(context.Background(), oidc.Config{
		IssuerURL:     idp.Issuer(),
		ClientID:      idp.ClientID(),
		ClientSecret:  idp.ClientSecret(),
		RedirectURL:   testRedirectURL,
		SessionSecret: []byte("deterministic-test-session-secret"),
	})
	if err != nil {
		t.Fatalf("oidc.New error = %v", err)
	}
	_, all := idp.LoginCookies(t, auth, "")

	rec, loc := logout(t, auth, all...)

	if loc.String() != "/" {
		t.Errorf("logout against an IdP with no end_session_endpoint redirected to %q, want %q", loc, "/")
	}
	if cleared := findCookie(rec.Result().Cookies(), "scrim_session"); cleared == nil || cleared.MaxAge >= 0 {
		t.Error("logout did not clear the session cookie on the local-only path")
	}
}

// TestLogoutSendsClientIDWithoutHint covers the degraded path: the session is
// valid but no ID token was retained (cookie dropped, expired, or too large).
// Logout must still reach the IdP, identifying the RP by client_id instead --
// per OIDC RP-Initiated Logout 1.0 §2, client_id is the substitute when
// id_token_hint is absent.
func TestLogoutSendsClientIDWithoutHint(t *testing.T) {
	auth, idp := newAuth(t)
	session, _ := idp.LoginCookies(t, auth, "")

	// Deliberately withhold the ID-token cookie.
	_, loc := logout(t, auth, session)

	if loc.Host == "" {
		t.Fatalf("logout without a retained ID token redirected to %q, want the IdP end-session endpoint", loc)
	}
	if got := loc.Query().Get("id_token_hint"); got != "" {
		t.Errorf("id_token_hint = %q, want empty when no token was retained", got)
	}
	if got := loc.Query().Get("client_id"); got != idp.ClientID() {
		t.Errorf("client_id = %q, want %q", got, idp.ClientID())
	}
}

// TestLogoutIgnoresTamperedIDTokenCookie pins that a forged retained-token
// cookie is treated as absent, not trusted through to the IdP.
func TestLogoutIgnoresTamperedIDTokenCookie(t *testing.T) {
	auth, idp := newAuth(t)
	session, all := idp.LoginCookies(t, auth, "")
	idt := findCookie(all, "scrim_oidc_idt")
	if idt == nil {
		t.Fatal("login retained no ID-token cookie")
	}
	tampered := &http.Cookie{Name: idt.Name, Value: idt.Value + "x"}

	_, loc := logout(t, auth, session, tampered)

	if got := loc.Query().Get("id_token_hint"); got != "" {
		t.Errorf("id_token_hint = %q, want empty for a tampered cookie", got)
	}
	if got := loc.Query().Get("client_id"); got != idp.ClientID() {
		t.Errorf("client_id = %q, want the hint-less fallback %q", got, idp.ClientID())
	}
}

// TestIDTokenCookieIsDomainSeparated pins that the retained-token cookie and
// the session cookie cannot be swapped for one another, even though both are
// HMAC'd with the same secret.
func TestIDTokenCookieIsDomainSeparated(t *testing.T) {
	auth, idp := newAuth(t)
	session, all := idp.LoginCookies(t, auth, "")
	idt := findCookie(all, "scrim_oidc_idt")
	if idt == nil {
		t.Fatal("login retained no ID-token cookie")
	}

	// A session cookie presented as the ID-token cookie must not decode.
	swapped := &http.Cookie{Name: "scrim_oidc_idt", Value: session.Value}
	_, loc := logout(t, auth, session, swapped)
	if got := loc.Query().Get("id_token_hint"); got != "" {
		t.Errorf("a session cookie verified as an ID-token cookie (hint = %q), want domain separation to reject it", got)
	}

	// And the reverse: an ID-token cookie presented as the session cookie must
	// not authenticate.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: "scrim_session", Value: idt.Value})
	if _, ok := auth.SessionFromRequest(req); ok {
		t.Error("an ID-token cookie verified as a session cookie, want domain separation to reject it")
	}
}

// TestIDTokenCookieAttributes pins the retained-token cookie to the same
// hardening the session cookie gets -- it carries an IdP assertion about the
// user, so script access or a plaintext hop would both be leaks.
func TestIDTokenCookieAttributes(t *testing.T) {
	idp := oidctest.New(t)
	auth, err := oidc.New(context.Background(), oidc.Config{
		IssuerURL:     idp.Issuer(),
		ClientID:      idp.ClientID(),
		ClientSecret:  idp.ClientSecret(),
		RedirectURL:   testRedirectURL,
		SessionSecret: []byte("deterministic-test-session-secret"),
		SecureCookies: true,
	})
	if err != nil {
		t.Fatalf("oidc.New error = %v", err)
	}
	_, all := idp.LoginCookies(t, auth, "")
	idt := findCookie(all, "scrim_oidc_idt")
	if idt == nil {
		t.Fatal("login retained no ID-token cookie")
	}
	if !idt.HttpOnly {
		t.Error("ID-token cookie is not HttpOnly")
	}
	if !idt.Secure {
		t.Error("ID-token cookie is not Secure under SecureCookies")
	}
	if idt.SameSite != http.SameSiteLaxMode {
		t.Errorf("ID-token cookie SameSite = %v, want Lax", idt.SameSite)
	}
	if idt.Path != "/" {
		t.Errorf("ID-token cookie Path = %q, want %q", idt.Path, "/")
	}
}

// TestOversizedIDTokenDoesNotBreakLogin is the reason the retained token lives
// in its OWN cookie: an ID token too large to store must cost only the logout
// hint. The login still succeeds and logout still reaches the IdP.
func TestOversizedIDTokenDoesNotBreakLogin(t *testing.T) {
	idp := oidctest.New(t)
	idp.PadIDTokenClaim = strings.Repeat("x", 8000)
	auth, err := oidc.New(context.Background(), oidc.Config{
		IssuerURL:     idp.Issuer(),
		ClientID:      idp.ClientID(),
		ClientSecret:  idp.ClientSecret(),
		RedirectURL:   testRedirectURL,
		SessionSecret: []byte("deterministic-test-session-secret"),
	})
	if err != nil {
		t.Fatalf("oidc.New error = %v", err)
	}

	session, all := idp.LoginCookies(t, auth, "")
	if session.Value == "" {
		t.Fatal("an oversized ID token broke the login itself")
	}
	if idt := findCookie(all, "scrim_oidc_idt"); idt != nil && idt.Value != "" {
		t.Errorf("an oversized ID token was retained anyway (%d bytes), want it skipped", len(idt.Value))
	}

	_, loc := logout(t, auth, all...)
	if loc.Host == "" {
		t.Fatalf("logout redirected to %q, want the IdP end-session endpoint even without a hint", loc)
	}
	if got := loc.Query().Get("client_id"); got != idp.ClientID() {
		t.Errorf("client_id = %q, want the hint-less fallback %q", got, idp.ClientID())
	}
}

// TestLogoutSendsPostLogoutRedirectWhenConfigured covers the opt-in nicer
// landing: configured, the parameter is sent; unconfigured (the default) it is
// omitted, which is asserted in TestLogoutRedirectsToEndSessionEndpoint.
func TestLogoutSendsPostLogoutRedirectWhenConfigured(t *testing.T) {
	idp := oidctest.New(t)
	const postLogout = "https://hub.test/"
	auth, err := oidc.New(context.Background(), oidc.Config{
		IssuerURL:             idp.Issuer(),
		ClientID:              idp.ClientID(),
		ClientSecret:          idp.ClientSecret(),
		RedirectURL:           testRedirectURL,
		PostLogoutRedirectURL: postLogout,
		SessionSecret:         []byte("deterministic-test-session-secret"),
	})
	if err != nil {
		t.Fatalf("oidc.New error = %v", err)
	}
	_, all := idp.LoginCookies(t, auth, "")

	_, loc := logout(t, auth, all...)

	if got := loc.Query().Get("post_logout_redirect_uri"); got != postLogout {
		t.Errorf("post_logout_redirect_uri = %q, want %q", got, postLogout)
	}
}

// TestNewRejectsBadPostLogoutRedirectURL pins the boot-time validation: a
// value that could never work is refused at startup, not at the first logout.
func TestNewRejectsBadPostLogoutRedirectURL(t *testing.T) {
	idp := oidctest.New(t)
	base := oidc.Config{
		IssuerURL:    idp.Issuer(),
		ClientID:     idp.ClientID(),
		ClientSecret: idp.ClientSecret(),
		RedirectURL:  testRedirectURL,
	}
	for _, bad := range []string{"/", "not a url", "https:///"} {
		c := base
		c.PostLogoutRedirectURL = bad
		if _, err := oidc.New(context.Background(), c); err == nil {
			t.Errorf("New with post-logout redirect %q error = nil, want an error", bad)
		}
	}
}

// assertNoSession fails unless rec represents a fail-closed callback: not a
// 302 success, and no session cookie set.
func assertNoSession(t *testing.T, rec *httptest.ResponseRecorder, label string) {
	t.Helper()
	if rec.Code == http.StatusFound {
		t.Errorf("%s: callback returned 302 (success), want a rejection", label)
	}
	if s := findCookie(rec.Result().Cookies(), "scrim_session"); s != nil && s.Value != "" {
		t.Errorf("%s: callback set a session cookie, want none", label)
	}
}

func findCookie(cookies []*http.Cookie, name string) *http.Cookie {
	for _, c := range cookies {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// TestCallbackRegistersSessionWithUserAgent proves the login records itself in
// the server-side registry BEFORE the cookie is handed out, carrying the same
// session id the cookie will present, the browser's User-Agent, and the
// cookie's expiry -- and nothing else. No IP address is passed, by design (#145
// decision 2); the hook's signature is the enforcement of that.
func TestCallbackRegistersSessionWithUserAgent(t *testing.T) {
	idp := oidctest.New(t)
	idp.Subject = "sub-registered"

	var gotSession oidc.Session
	var gotUA string
	var gotExpiry time.Time
	calls := 0
	auth, err := oidc.New(context.Background(), oidc.Config{
		IssuerURL:     idp.Issuer(),
		ClientID:      idp.ClientID(),
		ClientSecret:  idp.ClientSecret(),
		RedirectURL:   testRedirectURL,
		SessionSecret: []byte("deterministic-test-session-secret"),
		RegisterSession: func(sess oidc.Session, userAgent string, expiry time.Time) error {
			calls++
			gotSession, gotUA, gotExpiry = sess, userAgent, expiry
			return nil
		},
	})
	if err != nil {
		t.Fatalf("oidc.New error = %v", err)
	}

	flow, query := idp.CallbackLocation(t, auth)
	req := httptest.NewRequest(http.MethodGet, oidc.CallbackPath+"?"+query, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) Firefox/135.0")
	req.AddCookie(flow)
	rec := httptest.NewRecorder()
	auth.HandleCallback(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("HandleCallback status = %d, want 302 (body %q)", rec.Code, rec.Body.String())
	}
	if calls != 1 {
		t.Fatalf("RegisterSession called %d times, want exactly 1", calls)
	}
	if gotSession.ID == "" {
		t.Error("registered session has no id, want the id the cookie carries")
	}
	if gotSession.Subject != "sub-registered" {
		t.Errorf("registered subject = %q, want %q", gotSession.Subject, "sub-registered")
	}
	if gotUA != "Mozilla/5.0 (X11; Linux x86_64) Firefox/135.0" {
		t.Errorf("registered user agent = %q, want the request's User-Agent", gotUA)
	}
	if gotExpiry.IsZero() || !gotExpiry.After(time.Now()) {
		t.Errorf("registered expiry = %v, want a future session expiry", gotExpiry)
	}

	// The cookie the browser gets carries that very id, so the gate's registry
	// lookup can find the record this login just wrote.
	cookie := findCookie(rec.Result().Cookies(), "scrim_session")
	if cookie == nil {
		t.Fatal("HandleCallback minted no session cookie")
	}
	verify := httptest.NewRequest(http.MethodGet, "/", nil)
	verify.AddCookie(cookie)
	sess, ok := auth.SessionFromRequest(verify)
	if !ok {
		t.Fatal("SessionFromRequest with the minted cookie = not ok, want ok")
	}
	if sess.ID != gotSession.ID {
		t.Errorf("cookie session id = %q, registry got %q -- they must be the same session", sess.ID, gotSession.ID)
	}
}

// TestCallbackFailsClosedWhenRegistrationFails pins the fail-closed half: if the
// session can't be recorded, no cookie is issued at all. Issuing one anyway
// would mint a session the gate rejects on the next request -- a "successful"
// login that bounces straight back to the login page.
func TestCallbackFailsClosedWhenRegistrationFails(t *testing.T) {
	idp := oidctest.New(t)
	auth, err := oidc.New(context.Background(), oidc.Config{
		IssuerURL:     idp.Issuer(),
		ClientID:      idp.ClientID(),
		ClientSecret:  idp.ClientSecret(),
		RedirectURL:   testRedirectURL,
		SessionSecret: []byte("deterministic-test-session-secret"),
		RegisterSession: func(oidc.Session, string, time.Time) error {
			return errors.New("registry unavailable")
		},
	})
	if err != nil {
		t.Fatalf("oidc.New error = %v", err)
	}

	flow, query := idp.CallbackLocation(t, auth)
	req := httptest.NewRequest(http.MethodGet, oidc.CallbackPath+"?"+query, nil)
	req.AddCookie(flow)
	rec := httptest.NewRecorder()
	auth.HandleCallback(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("HandleCallback with a failing registry status = %d, want 500", rec.Code)
	}
	if c := findCookie(rec.Result().Cookies(), "scrim_session"); c != nil {
		t.Error("HandleCallback set a session cookie despite the registration failing, want none")
	}
}

// TestLogoutEndsSessionServerSide proves logout revokes the record too, not
// just the cookie: a cookie copied elsewhere must stop working the moment its
// owner signs out, which is the entire reason the registry exists.
func TestLogoutEndsSessionServerSide(t *testing.T) {
	idp := oidctest.New(t)
	registered := ""
	ended := ""
	auth, err := oidc.New(context.Background(), oidc.Config{
		IssuerURL:     idp.Issuer(),
		ClientID:      idp.ClientID(),
		ClientSecret:  idp.ClientSecret(),
		RedirectURL:   testRedirectURL,
		SessionSecret: []byte("deterministic-test-session-secret"),
		RegisterSession: func(sess oidc.Session, _ string, _ time.Time) error {
			registered = sess.ID
			return nil
		},
		EndSession: func(id string) { ended = id },
	})
	if err != nil {
		t.Fatalf("oidc.New error = %v", err)
	}

	_, all := idp.LoginCookies(t, auth, "")
	logout(t, auth, all...)

	if registered == "" {
		t.Fatal("login registered no session id")
	}
	if ended != registered {
		t.Errorf("logout ended session %q, want the logged-in session %q", ended, registered)
	}
}

// TestLogoutWithoutSessionEndsNothing pins that a session-less logout (what a
// forged cross-site POST produces, since SameSite=Lax withholds the cookie)
// revokes nobody -- the registry is not a scrim-wide sign-out lever for an
// unauthenticated caller.
func TestLogoutWithoutSessionEndsNothing(t *testing.T) {
	idp := oidctest.New(t)
	called := false
	auth, err := oidc.New(context.Background(), oidc.Config{
		IssuerURL:     idp.Issuer(),
		ClientID:      idp.ClientID(),
		ClientSecret:  idp.ClientSecret(),
		RedirectURL:   testRedirectURL,
		SessionSecret: []byte("deterministic-test-session-secret"),
		EndSession:    func(string) { called = true },
	})
	if err != nil {
		t.Fatalf("oidc.New error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, oidc.LogoutPath, nil)
	auth.HandleLogout(httptest.NewRecorder(), req)

	if called {
		t.Error("EndSession was called for a session-less logout, want no revocation")
	}
}

// TestSessionPolicyDefaults pins the resolved policy -- the idle window and
// the absolute cap -- since both are now behavior an operator configures and
// two packages (the Authenticator and the hub's session registry) must read
// identically.
func TestSessionPolicyDefaults(t *testing.T) {
	tests := []struct {
		name      string
		cfg       oidc.Config
		wantIdle  time.Duration
		wantMax   time.Duration
		wantUncap bool
	}{
		{
			name:     "unset takes both defaults",
			cfg:      oidc.Config{},
			wantIdle: 7 * 24 * time.Hour,
			wantMax:  30 * 24 * time.Hour,
		},
		{
			name:     "explicit values win",
			cfg:      oidc.Config{SessionTTL: 2 * time.Hour, SessionMaxLifetime: 9 * time.Hour},
			wantIdle: 2 * time.Hour,
			wantMax:  9 * time.Hour,
		},
		{
			name:     "a zero idle window still takes the default",
			cfg:      oidc.Config{SessionMaxLifetime: 9 * time.Hour},
			wantIdle: 7 * 24 * time.Hour,
			wantMax:  9 * time.Hour,
		},
		{
			name:      "a negative cap means uncapped",
			cfg:       oidc.Config{SessionTTL: time.Hour, SessionMaxLifetime: -1},
			wantIdle:  time.Hour,
			wantUncap: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			idle, maxLife := tc.cfg.SessionPolicy()
			if idle != tc.wantIdle {
				t.Errorf("idle = %v, want %v", idle, tc.wantIdle)
			}
			if tc.wantUncap {
				if maxLife > 0 {
					t.Errorf("max = %v, want a non-positive (uncapped) value", maxLife)
				}
				return
			}
			if maxLife != tc.wantMax {
				t.Errorf("max = %v, want %v", maxLife, tc.wantMax)
			}
		})
	}
}

// TestDefaultSessionTTLIsAnIdleWindow guards the semantic change itself: the
// default is a week, not the old 12-hour absolute lifetime. A revert to a
// short default would silently reintroduce the mid-use sign-out this replaced.
func TestDefaultSessionTTLIsAnIdleWindow(t *testing.T) {
	if oidc.DefaultSessionTTL != 7*24*time.Hour {
		t.Errorf("DefaultSessionTTL = %v, want 7d", oidc.DefaultSessionTTL)
	}
	if oidc.DefaultSessionMaxLifetime != 30*24*time.Hour {
		t.Errorf("DefaultSessionMaxLifetime = %v, want 30d", oidc.DefaultSessionMaxLifetime)
	}
	if oidc.DefaultSessionMaxLifetime <= oidc.DefaultSessionTTL {
		t.Error("the absolute cap must be longer than the idle window, else no session ever slides")
	}
}

// TestRenewSessionReissuesBothCookies pins the cookie half of renewal: the
// session cookie is re-signed with the later expiry and still verifies with
// the same claims, and the retained id_token cookie slides with it so a
// long-lived session does not silently lose its logout hint.
func TestRenewSessionReissuesBothCookies(t *testing.T) {
	auth, idp := newAuth(t)
	sessionCookie, all := idp.LoginCookies(t, auth, "")

	probe := httptest.NewRequest(http.MethodGet, "/", nil)
	probe.AddCookie(sessionCookie)
	sess, ok := auth.SessionFromRequest(probe)
	if !ok {
		t.Fatal("the minted session cookie does not verify")
	}

	renewReq := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range all {
		if c != nil {
			renewReq.AddCookie(c)
		}
	}
	newExpiry := time.Now().Add(14 * 24 * time.Hour).Truncate(time.Second)
	rec := httptest.NewRecorder()
	auth.RenewSession(rec, renewReq, sess, newExpiry)

	issued := findCookie(rec.Result().Cookies(), "scrim_session")
	if issued == nil {
		t.Fatal("RenewSession set no session cookie")
	}
	if issued.MaxAge <= 0 {
		t.Errorf("re-issued session cookie MaxAge = %d, want a positive lifetime", issued.MaxAge)
	}
	verify := httptest.NewRequest(http.MethodGet, "/", nil)
	verify.AddCookie(issued)
	got, ok := auth.SessionFromRequest(verify)
	if !ok {
		t.Fatal("the re-issued session cookie does not verify")
	}
	if got.ID != sess.ID || got.Subject != sess.Subject || got.Email != sess.Email {
		t.Errorf("re-issued session = %+v, want the same identity as %+v", got, sess)
	}
	if got.Expiry != newExpiry.Unix() {
		t.Errorf("re-issued session expiry = %d, want %d", got.Expiry, newExpiry.Unix())
	}

	// The id_token cookie slid too -- proven where it matters, at logout:
	// carrying the renewed pair still yields an id_token_hint.
	renewedIDT := findCookie(rec.Result().Cookies(), "scrim_oidc_idt")
	if renewedIDT == nil {
		t.Fatal("RenewSession did not re-issue the retained id_token cookie")
	}
	_, loc := logout(t, auth, issued, renewedIDT)
	if hint := loc.Query().Get("id_token_hint"); hint == "" {
		t.Error("logout after renewal has no id_token_hint, want the slid ID token")
	}
}

// TestRenewSessionWithNoRetainedIDTokenIsHarmless: a session whose id_token
// cookie is gone (dropped, expired, oversized at login) still renews -- the
// hint is best-effort and its absence must never block the extension.
func TestRenewSessionWithNoRetainedIDTokenIsHarmless(t *testing.T) {
	auth, idp := newAuth(t)
	sessionCookie := idp.Login(t, auth, "")

	probe := httptest.NewRequest(http.MethodGet, "/", nil)
	probe.AddCookie(sessionCookie)
	sess, ok := auth.SessionFromRequest(probe)
	if !ok {
		t.Fatal("the minted session cookie does not verify")
	}

	// Only the session cookie is presented.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(sessionCookie)
	rec := httptest.NewRecorder()
	auth.RenewSession(rec, req, sess, time.Now().Add(48*time.Hour))

	if findCookie(rec.Result().Cookies(), "scrim_session") == nil {
		t.Error("RenewSession set no session cookie when the id_token cookie was absent")
	}
	if c := findCookie(rec.Result().Cookies(), "scrim_oidc_idt"); c != nil {
		t.Errorf("RenewSession invented an id_token cookie (%+v), want none -- there was nothing to slide", c)
	}
}

// TestRenewSessionRefusesAPastExpiry: renewal never issues an already-dead
// cookie, which would read to a browser as an instruction to delete it.
func TestRenewSessionRefusesAPastExpiry(t *testing.T) {
	auth, idp := newAuth(t)
	sessionCookie := idp.Login(t, auth, "")

	probe := httptest.NewRequest(http.MethodGet, "/", nil)
	probe.AddCookie(sessionCookie)
	sess, _ := auth.SessionFromRequest(probe)

	rec := httptest.NewRecorder()
	auth.RenewSession(rec, probe, sess, time.Now().Add(-time.Minute))
	if got := rec.Result().Cookies(); len(got) != 0 {
		t.Errorf("RenewSession with a past expiry set cookies %v, want none", got)
	}
}
