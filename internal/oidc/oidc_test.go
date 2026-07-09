package oidc_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

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
