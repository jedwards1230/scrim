// Package oidctest provides a faithful in-process fake OpenID Connect
// identity provider for exercising scrim's OIDC login without any real
// network, browser, or IdP. It serves discovery, JWKS, an authorization
// endpoint, and a token endpoint, signs real RS256 ID tokens with an
// ephemeral RSA key, and enforces PKCE -- so a test drives the same code
// paths a real IdP would.
//
// It is a test-support package: nothing in the production build imports it,
// only _test.go files do (the same relationship net/http has with
// net/http/httptest).
package oidctest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

// DefaultClientID / DefaultClientSecret are the credentials the fake IdP
// expects, for a test that doesn't care to customize them.
const (
	DefaultClientID     = "scrim-hub-test"
	DefaultClientSecret = "test-client-secret"
)

// IdP is a running fake identity provider. Construct it with New; read Issuer
// (and the client credentials) to build an oidc.Config against it. The knob
// fields alter the tokens it issues, to drive failure paths.
type IdP struct {
	server *httptest.Server
	key    *rsa.PrivateKey
	kid    string

	clientID     string
	clientSecret string

	// Subject is the `sub` claim placed in issued ID tokens. Defaults to
	// "user-abc"; set it to test a different identity.
	Subject string
	// Email, Name, and Groups, when set, are emitted as the corresponding ID
	// token claims -- so a test can drive the hub's email-keyed ownership and
	// group-based grants. Email defaults to "user@example.com" (see
	// signIDToken); Name and Groups are omitted from the token when empty.
	Email  string
	Name   string
	Groups []string
	// ForceNonce, when non-empty, overrides the nonce echoed into the ID
	// token, so a test can drive the callback's nonce-mismatch rejection.
	ForceNonce string
	// OmitIDToken, when true, makes the token endpoint return no id_token, so
	// a test can drive the callback's missing-id_token rejection.
	OmitIDToken bool

	mu    sync.Mutex
	codes map[string]codeData
}

type codeData struct {
	nonce     string
	challenge string
}

// New starts a fake IdP and registers its shutdown with t.Cleanup.
func New(t *testing.T) *IdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("oidctest: generating RSA key: %v", err)
	}
	i := &IdP{
		key:          key,
		kid:          "test-key-1",
		clientID:     DefaultClientID,
		clientSecret: DefaultClientSecret,
		Subject:      "user-abc",
		codes:        make(map[string]codeData),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", i.handleDiscovery)
	mux.HandleFunc("GET /jwks", i.handleJWKS)
	mux.HandleFunc("GET /authorize", i.handleAuthorize)
	mux.HandleFunc("POST /token", i.handleToken)
	i.server = httptest.NewServer(mux)
	t.Cleanup(i.server.Close)
	return i
}

// Issuer is the IdP's issuer URL (its base URL); use it as oidc.Config.IssuerURL.
func (i *IdP) Issuer() string { return i.server.URL }

// ClientID / ClientSecret are the credentials the fake IdP expects.
func (i *IdP) ClientID() string     { return i.clientID }
func (i *IdP) ClientSecret() string { return i.clientSecret }

func (i *IdP) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"issuer":                                i.server.URL,
		"authorization_endpoint":                i.server.URL + "/authorize",
		"token_endpoint":                        i.server.URL + "/token",
		"jwks_uri":                              i.server.URL + "/jwks",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"scopes_supported":                      []string{"openid", "profile", "email"},
	})
}

func (i *IdP) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	pub := i.key.Public().(*rsa.PublicKey)
	eBytes := big.NewInt(int64(pub.E)).Bytes()
	writeJSON(w, map[string]any{
		"keys": []map[string]any{{
			"kty": "RSA",
			"use": "sig",
			"alg": "RS256",
			"kid": i.kid,
			"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(eBytes),
		}},
	})
}

// handleAuthorize mimics the IdP's authorization endpoint: it records the
// nonce and PKCE challenge against a freshly minted code, then redirects the
// browser back to the client's redirect_uri with that code and the original
// state -- exactly as a real IdP does after the user authenticates.
func (i *IdP) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirectURI := q.Get("redirect_uri")
	state := q.Get("state")
	code := randString()

	i.mu.Lock()
	i.codes[code] = codeData{nonce: q.Get("nonce"), challenge: q.Get("code_challenge")}
	i.mu.Unlock()

	u, _ := url.Parse(redirectURI)
	rq := u.Query()
	rq.Set("code", code)
	rq.Set("state", state)
	u.RawQuery = rq.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// handleToken mimics the token endpoint: it verifies the PKCE verifier against
// the stored challenge, then issues a signed ID token embedding the nonce it
// recorded at /authorize (or the ForceNonce override).
func (i *IdP) handleToken(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	code := r.PostForm.Get("code")

	i.mu.Lock()
	data, ok := i.codes[code]
	delete(i.codes, code)
	i.mu.Unlock()
	if !ok {
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
		return
	}

	// Enforce PKCE: S256(code_verifier) must equal the stored challenge.
	verifier := r.PostForm.Get("code_verifier")
	sum := sha256.Sum256([]byte(verifier))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != data.challenge {
		http.Error(w, `{"error":"invalid_grant","error_description":"pkce"}`, http.StatusBadRequest)
		return
	}

	resp := map[string]any{
		"access_token": "test-access-token",
		"token_type":   "Bearer",
		"expires_in":   3600,
	}
	if !i.OmitIDToken {
		nonce := data.nonce
		if i.ForceNonce != "" {
			nonce = i.ForceNonce
		}
		resp["id_token"] = i.signIDToken(nonce)
	}
	writeJSON(w, resp)
}

// signIDToken builds and RS256-signs an ID token for i.Subject with the given
// nonce, valid for an hour.
func (i *IdP) signIDToken(nonce string) string {
	now := time.Now()
	header := map[string]any{"alg": "RS256", "typ": "JWT", "kid": i.kid}
	email := i.Email
	if email == "" {
		email = "user@example.com"
	}
	claims := map[string]any{
		"iss":   i.server.URL,
		"sub":   i.Subject,
		"aud":   i.clientID,
		"exp":   now.Add(time.Hour).Unix(),
		"iat":   now.Unix(),
		"nonce": nonce,
		// A default Authentik mapping returns email_verified=false; include it
		// to prove scrim accepts the login regardless (identity keys on sub).
		"email":          email,
		"email_verified": false,
	}
	// name/groups are optional profile claims -- emitted only when the test set
	// them, so the "IdP omits them" path (empty session fields, still a valid
	// login) stays exercised by default.
	if i.Name != "" {
		claims["name"] = i.Name
	}
	if len(i.Groups) > 0 {
		claims["groups"] = i.Groups
	}
	hb, _ := json.Marshal(header)
	cb, _ := json.Marshal(claims)
	signingInput := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(cb)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, i.key, crypto.SHA256, digest[:])
	if err != nil {
		panic("oidctest: signing id_token: " + err.Error())
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func randString() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
