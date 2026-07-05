// Package oidc implements generic OpenID Connect login for the scrim hub's
// read access: an authorization-code flow (with state, nonce, and PKCE)
// against any discovery-compliant IdP, a signed session cookie minted after a
// verified ID token, and a stateless per-attempt flow cookie binding the
// callback to the browser that started it.
//
// It is deliberately IdP-agnostic: everything is driven by the issuer's
// /.well-known/openid-configuration document, with nothing specific to any
// one provider in the code. Identity is keyed on the standard `sub` claim;
// any user the IdP authenticates is accepted on first login (auto-
// registration -- there is no local user database to pre-seed). The optional
// email/email_verified claims are not consulted for the access decision, so
// an IdP that returns email_verified=false (e.g. Authentik's default mapping)
// does not lock anyone out.
package oidc

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	coreoidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	// SessionCookieName is the cookie carrying a signed, authenticated session
	// after a completed login. The hub read gate authenticates a request by
	// verifying this cookie (see SessionFromRequest); the SSE endpoint, an
	// ordinary GET, authenticates by the very same cookie.
	SessionCookieName = "scrim_session"
	// flowCookieName carries the signed per-attempt login state between
	// /auth/login and /auth/callback. It is short-lived and cleared the moment
	// the callback consumes it.
	flowCookieName = "scrim_oidc_flow"

	// LoginPath initiates the auth-code flow; CallbackPath is the fixed
	// redirect URI the IdP returns to (it must match the redirect URL
	// registered with the IdP); LogoutPath clears the session cookie. The hub
	// read gate exempts exactly these three paths so an unauthenticated
	// browser can actually reach the login flow.
	LoginPath    = "/auth/login"
	CallbackPath = "/auth/callback"
	LogoutPath   = "/auth/logout"

	// flowTTL bounds how long a started login may sit before its callback: long
	// enough for a human to complete an IdP login (including MFA), short enough
	// that a leaked flow cookie is useless soon after.
	flowTTL = 10 * time.Minute
)

// Config is the resolved OIDC configuration for a hub. It is populated from
// the hub's flags/env (see internal/cli) and validated by New.
type Config struct {
	// IssuerURL is the OIDC issuer; its /.well-known/openid-configuration is
	// fetched at startup to discover the authorization, token, and JWKS
	// endpoints. Required.
	IssuerURL string
	// ClientID and ClientSecret are the confidential client's credentials as
	// registered with the IdP. Both required.
	ClientID     string
	ClientSecret string
	// RedirectURL is the full external URL of CallbackPath (e.g.
	// https://scrim.example.com/auth/callback). It must be provided explicitly
	// -- behind a TLS-terminating proxy the hub cannot reliably derive its own
	// external scheme/host -- and must exactly match what is registered with
	// the IdP. Required.
	RedirectURL string
	// Scopes requested at authorization. "openid" is always included by New
	// even if absent here. Defaults to {"openid","profile","email"} upstream.
	Scopes []string
	// SessionSecret is the HMAC key for the session and flow cookies. If empty,
	// New generates a random one -- sessions then do not survive a hub restart
	// (a fresh key invalidates old cookies), which is a safe default, just not
	// a persistent one. Provide a stable secret to persist sessions across
	// restarts / replicas.
	SessionSecret []byte
	// SessionTTL is how long a minted session cookie stays valid. Defaults to
	// DefaultSessionTTL.
	SessionTTL time.Duration
	// SecureCookies sets the Secure attribute on issued cookies. It should be
	// true in production (the hub is served over TLS by its proxy); it exists
	// as a knob only so a plain-HTTP local test deployment can turn it off.
	SecureCookies bool
	// LogAuthFailure, if non-nil, is called with a COARSE, static reason on an
	// authentication failure (e.g. "callback: state mismatch"). Reasons never
	// contain tokens, URLs, claim values, or any request-derived text -- the
	// caller wires this to the hub's scrubbed logging surface.
	LogAuthFailure func(reason string)
}

// DefaultSessionTTL is the session lifetime used when Config.SessionTTL is
// zero.
const DefaultSessionTTL = 12 * time.Hour

// minSessionSecretLen is the smallest operator-supplied SessionSecret New
// accepts; it matches the size of the key New generates when none is given.
const minSessionSecretLen = 32

// Authenticator holds the discovered OIDC endpoints, the OAuth2 client
// config, the ID-token verifier, and the cookie signer. It is safe for
// concurrent use by multiple request handlers.
type Authenticator struct {
	oauth2   oauth2.Config
	verifier *coreoidc.IDTokenVerifier
	signer   signer

	sessionTTL time.Duration
	secure     bool
	logFailure func(reason string)

	// now is time.Now in production; overridable in tests for deterministic
	// expiry.
	now func() time.Time
}

// New validates cfg, performs OIDC discovery against cfg.IssuerURL, and
// returns a ready Authenticator. It FAILS CLOSED: any missing required field,
// or a discovery failure, returns an error so the hub never starts a server
// that is "OIDC configured" but not actually enforcing it. Discovery is
// bounded by a short timeout derived from ctx so a hung IdP cannot hang hub
// startup indefinitely.
func New(ctx context.Context, cfg Config) (*Authenticator, error) {
	if cfg.IssuerURL == "" {
		return nil, errors.New("oidc: issuer URL is required")
	}
	if cfg.ClientID == "" {
		return nil, errors.New("oidc: client ID is required")
	}
	if cfg.ClientSecret == "" {
		return nil, errors.New("oidc: client secret is required")
	}
	if cfg.RedirectURL == "" {
		return nil, errors.New("oidc: redirect URL is required")
	}
	// The redirect URL is what the hub hands the IdP and what the IdP sends the
	// browser back to, so it must be a real absolute URL with a host (e.g.
	// https://scrim.example.com/auth/callback). Reject a relative or
	// host-less value at boot -- failing closed on misconfiguration rather
	// than starting a hub whose login flow can never complete. The path is
	// left unchecked on purpose so a prefix-stripping proxy (external
	// /prefix/auth/callback -> hub /auth/callback) still works.
	if u, err := url.Parse(cfg.RedirectURL); err != nil {
		return nil, fmt.Errorf("oidc: parsing redirect URL: %w", err)
	} else if !u.IsAbs() || u.Host == "" {
		return nil, fmt.Errorf("oidc: redirect URL %q must be absolute with a host (e.g. https://host/auth/callback)", cfg.RedirectURL)
	}

	discoveryCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	provider, err := coreoidc.NewProvider(discoveryCtx, cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("oidc: discovery against issuer failed: %w", err)
	}

	// The session secret keys the HMAC over every session and flow cookie. An
	// operator-supplied one must carry real entropy, so a non-empty value
	// shorter than the generated 32-byte size is rejected at startup rather
	// than quietly signing cookies with a weak key -- fail closed, matching
	// the repo's secure-by-default rule. An empty value is the sanctioned
	// "generate one for me" path (non-persistent across restarts).
	secret := cfg.SessionSecret
	switch {
	case len(secret) == 0:
		secret = make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return nil, fmt.Errorf("oidc: generating session secret: %w", err)
		}
	case len(secret) < minSessionSecretLen:
		return nil, fmt.Errorf("oidc: session secret must be at least %d bytes when provided (got %d)", minSessionSecretLen, len(secret))
	}

	ttl := cfg.SessionTTL
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}

	return &Authenticator{
		oauth2: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Endpoint:     provider.Endpoint(),
			RedirectURL:  cfg.RedirectURL,
			Scopes:       withOpenID(cfg.Scopes),
		},
		verifier:   provider.Verifier(&coreoidc.Config{ClientID: cfg.ClientID}),
		signer:     signer{key: secret},
		sessionTTL: ttl,
		secure:     cfg.SecureCookies,
		logFailure: cfg.LogAuthFailure,
		now:        time.Now,
	}, nil
}

// withOpenID returns scopes with "openid" guaranteed present (it is mandatory
// for an OIDC request) and, when the caller passed nothing, a sensible
// default set.
func withOpenID(scopes []string) []string {
	if len(scopes) == 0 {
		return []string{coreoidc.ScopeOpenID, "profile", "email"}
	}
	for _, s := range scopes {
		if s == coreoidc.ScopeOpenID {
			return scopes
		}
	}
	return append([]string{coreoidc.ScopeOpenID}, scopes...)
}

// SessionFromRequest reports whether r carries a valid, unexpired,
// correctly-signed session cookie, returning the authenticated subject when
// it does. It is the hub read gate's authentication check and never mutates
// r or w.
func (a *Authenticator) SessionFromRequest(r *http.Request) (subject string, ok bool) {
	cookie, err := r.Cookie(SessionCookieName)
	if err != nil {
		return "", false
	}
	subject, err = a.signer.decodeSession(cookie.Value, a.now())
	if err != nil {
		return "", false
	}
	return subject, true
}

// HandleLogin initiates the authorization-code flow: it mints fresh state,
// nonce, and a PKCE verifier, stores them (with the sanitized post-login
// return path) in a signed, short-lived flow cookie, and redirects the
// browser to the IdP's authorization endpoint carrying the state, nonce, and
// S256 PKCE challenge.
func (a *Authenticator) HandleLogin(w http.ResponseWriter, r *http.Request) {
	state, err := randToken()
	if err != nil {
		a.fail(w, r, http.StatusInternalServerError, "login: generating state")
		return
	}
	nonce, err := randToken()
	if err != nil {
		a.fail(w, r, http.StatusInternalServerError, "login: generating nonce")
		return
	}
	verifier := oauth2.GenerateVerifier()
	returnTo := sanitizeReturnTo(r.URL.Query().Get("return_to"))

	fs := flowState{State: state, Nonce: nonce, Verifier: verifier, ReturnTo: returnTo}
	a.setCookie(w, flowCookieName, a.signer.encodeFlow(fs, a.now().Add(flowTTL)), flowTTL)

	authURL := a.oauth2.AuthCodeURL(state,
		coreoidc.Nonce(nonce),
		oauth2.S256ChallengeOption(verifier),
	)
	http.Redirect(w, r, authURL, http.StatusFound)
}

// HandleCallback completes the flow: it reads and clears the flow cookie,
// rejects any callback whose state does not match the cookie, exchanges the
// code for tokens (presenting the PKCE verifier), verifies the ID token's
// signature/issuer/audience/expiry via JWKS, checks its nonce against the
// cookie, and -- only then -- mints the session cookie and redirects to the
// stored return path. Every failure branch is fail-closed: no session is
// issued.
func (a *Authenticator) HandleCallback(w http.ResponseWriter, r *http.Request) {
	// Clear the single-use flow cookie unconditionally: whether this callback
	// succeeds or fails, its state/nonce/verifier must not be replayable.
	a.clearCookie(w, flowCookieName)

	cookie, err := r.Cookie(flowCookieName)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, "callback: missing flow cookie")
		return
	}
	fs, err := a.signer.decodeFlow(cookie.Value, a.now())
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, "callback: invalid or expired flow cookie")
		return
	}

	if errParam := r.URL.Query().Get("error"); errParam != "" {
		a.fail(w, r, http.StatusUnauthorized, "callback: identity provider returned an error")
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("state")), []byte(fs.State)) != 1 {
		a.fail(w, r, http.StatusBadRequest, "callback: state mismatch")
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		a.fail(w, r, http.StatusBadRequest, "callback: missing authorization code")
		return
	}

	// Bound the outbound calls to the IdP (token endpoint exchange, then JWKS
	// fetch during Verify) so a slow or hung IdP can't pin this handler open
	// indefinitely; derive from the request context so a client disconnect
	// still cancels promptly.
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	token, err := a.oauth2.Exchange(ctx, code, oauth2.VerifierOption(fs.Verifier))
	if err != nil {
		a.fail(w, r, http.StatusUnauthorized, "callback: token exchange failed")
		return
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		a.fail(w, r, http.StatusUnauthorized, "callback: no id_token in token response")
		return
	}
	idToken, err := a.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		a.fail(w, r, http.StatusUnauthorized, "callback: id_token verification failed")
		return
	}
	if subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(fs.Nonce)) != 1 {
		a.fail(w, r, http.StatusUnauthorized, "callback: nonce mismatch")
		return
	}
	if idToken.Subject == "" {
		a.fail(w, r, http.StatusUnauthorized, "callback: id_token has no subject")
		return
	}

	// Auto-registration: any subject the IdP authenticated is accepted. The
	// session keys on `sub` only; email/email_verified are intentionally not
	// consulted, so an IdP returning email_verified=false does not lock the
	// user out.
	a.setCookie(w, SessionCookieName, a.signer.encodeSession(idToken.Subject, a.now().Add(a.sessionTTL)), a.sessionTTL)
	http.Redirect(w, r, sanitizeReturnTo(fs.ReturnTo), http.StatusFound)
}

// HandleLogout clears the session cookie and redirects to the hub root. It
// clears only local state -- it does not initiate IdP single-logout.
func (a *Authenticator) HandleLogout(w http.ResponseWriter, r *http.Request) {
	a.clearCookie(w, SessionCookieName)
	http.Redirect(w, r, "/", http.StatusFound)
}

// fail logs a coarse reason (if a logger is wired) and writes a plain,
// detail-free HTTP error. The client never sees the internal reason string,
// and the reason itself carries no request-derived data.
func (a *Authenticator) fail(w http.ResponseWriter, _ *http.Request, status int, reason string) {
	if a.logFailure != nil {
		a.logFailure(reason)
	}
	http.Error(w, "authentication failed", status)
}

// setCookie writes a signed cookie scoped to the whole site, HttpOnly and
// SameSite=Lax (so it survives the top-level GET redirect back from the IdP
// yet is withheld from cross-site subrequests), with Secure per configuration.
func (a *Authenticator) setCookie(w http.ResponseWriter, name, value string, maxAge time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   int(maxAge.Seconds()),
		HttpOnly: true,
		Secure:   a.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// clearCookie expires a cookie by name (MaxAge < 0), matching the attributes
// used to set it so the browser reliably removes it.
func (a *Authenticator) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   a.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// randToken returns 32 bytes of cryptographically secure randomness, base64url
// encoded -- used for the anti-CSRF state and the anti-replay nonce.
func randToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("oidc: reading randomness: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// sanitizeReturnTo constrains a post-login redirect target to a local,
// same-origin path so the login endpoint can never be turned into an open
// redirect. Anything that isn't a plain rooted path ("/...", but not "//" or
// "/\" which browsers treat as protocol-relative/host-changing) collapses to
// "/".
func sanitizeReturnTo(raw string) string {
	if raw == "" || raw[0] != '/' {
		return "/"
	}
	if strings.HasPrefix(raw, "//") || strings.HasPrefix(raw, "/\\") {
		return "/"
	}
	// Reject anything with a scheme/host or control characters by round-
	// tripping through url.Parse and keeping only a relative path+query.
	u, err := url.Parse(raw)
	if err != nil || u.IsAbs() || u.Host != "" {
		return "/"
	}
	out := u.EscapedPath()
	if out == "" {
		out = "/"
	}
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	return out
}
