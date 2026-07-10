package mcpserver

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/scrim/internal/config"
)

// fakeAS is a minimal OAuth authorization server for tests: it serves an OIDC
// discovery document and a JWKS, and mints RS256 JWTs with arbitrary
// iss/aud/exp/scope so every accept and reject path can be exercised. It mirrors
// oidctest's signing technique but is tailored to access-token JWTs (custom aud
// + a `scope` claim), which oidctest's ID-token flow does not expose.
type fakeAS struct {
	server *httptest.Server
	key    *rsa.PrivateKey
	kid    string
}

func newFakeAS(t *testing.T) *fakeAS {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}
	as := &fakeAS{key: key, kid: "test-key-1"}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                                as.server.URL,
			"authorization_endpoint":                as.server.URL + "/authorize",
			"token_endpoint":                        as.server.URL + "/token",
			"jwks_uri":                              as.server.URL + "/jwks",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, _ *http.Request) {
		pub := as.key.Public().(*rsa.PublicKey)
		writeJSON(w, map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA", "use": "sig", "alg": "RS256", "kid": as.kid,
				"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
			}},
		})
	})
	as.server = httptest.NewServer(mux)
	t.Cleanup(as.server.Close)
	return as
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// mintOpts controls a token fakeAS mints; the zero value plus issuer/aud yields
// a valid, unexpired token with no scopes.
type mintOpts struct {
	iss   string // defaults to the AS issuer when empty
	aud   string
	scope string
	sub   string
	exp   time.Time      // defaults to now+1h when zero
	kid   string         // defaults to the AS kid when empty
	extra map[string]any // additional claims (e.g. email, groups) merged verbatim
}

func (as *fakeAS) mint(t *testing.T, o mintOpts) string {
	t.Helper()
	iss := o.iss
	if iss == "" {
		iss = as.server.URL
	}
	exp := o.exp
	if exp.IsZero() {
		exp = time.Now().Add(time.Hour)
	}
	kid := o.kid
	if kid == "" {
		kid = as.kid
	}
	header := map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid}
	claims := map[string]any{
		"iss": iss,
		"aud": o.aud,
		"sub": o.sub,
		"exp": exp.Unix(),
		"iat": time.Now().Unix(),
	}
	if o.scope != "" {
		claims["scope"] = o.scope
	}
	for k, v := range o.extra {
		claims[k] = v
	}
	hb, _ := json.Marshal(header)
	cb, _ := json.Marshal(claims)
	signingInput := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(cb)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, as.key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("signing token: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

const testAudience = "scrim-mcp"

// newTestValidator builds an oauthValidator against the fake AS with a fixed
// audience and configured resource (so metadata/WWW-Authenticate URLs are
// deterministic).
func newTestValidator(t *testing.T, as *fakeAS, resource string) *oauthValidator {
	t.Helper()
	v, err := newOAuthValidator(context.Background(), OAuthConfig{
		Issuer:   as.server.URL,
		Audience: testAudience,
		Resource: resource,
	})
	if err != nil {
		t.Fatalf("newOAuthValidator: %v", err)
	}
	return v
}

func TestOAuthConfigValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     OAuthConfig
		wantErr bool
	}{
		{name: "disabled is valid", cfg: OAuthConfig{}},
		{name: "issuer without audience fails", cfg: OAuthConfig{Issuer: "https://as.example"}, wantErr: true},
		{name: "issuer with audience ok", cfg: OAuthConfig{Issuer: "https://as.example", Audience: "scrim"}},
		{name: "bad resource url fails", cfg: OAuthConfig{Issuer: "https://as.example", Audience: "scrim", Resource: "not-absolute"}, wantErr: true},
		{name: "good resource url ok", cfg: OAuthConfig{Issuer: "https://as.example", Audience: "scrim", Resource: "https://scrim.example"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.wantErr != (err != nil) {
				t.Fatalf("Validate() err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// TestProtectedResourceMetadata proves the RFC 9728 document is served
// UNAUTHENTICATED (200) with the correct shape by newHTTPHandler.
func TestProtectedResourceMetadata(t *testing.T) {
	as := newFakeAS(t)
	v := newTestValidator(t, as, "https://scrim-mcp.example")
	cfg := config.Config{Dir: t.TempDir(), Host: "127.0.0.1", Port: 7799}
	ts := httptest.NewServer(newHTTPHandler(cfg, "test", nil, v))
	defer ts.Close()

	resp, err := http.Get(ts.URL + protectedResourceMetadataPath)
	if err != nil {
		t.Fatalf("GET metadata: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (must be reachable unauthenticated)", resp.StatusCode)
	}
	var meta protectedResourceMetadata
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if meta.Resource != "https://scrim-mcp.example" {
		t.Errorf("resource = %q, want https://scrim-mcp.example", meta.Resource)
	}
	if len(meta.AuthorizationServers) != 1 || meta.AuthorizationServers[0] != as.server.URL {
		t.Errorf("authorization_servers = %v, want [%q]", meta.AuthorizationServers, as.server.URL)
	}
	if strings.Join(meta.ScopesSupported, " ") != scopeRead+" "+scopeWrite {
		t.Errorf("scopes_supported = %v, want [%q %q]", meta.ScopesSupported, scopeRead, scopeWrite)
	}
	if len(meta.BearerMethodsSupported) != 1 || meta.BearerMethodsSupported[0] != "header" {
		t.Errorf("bearer_methods_supported = %v, want [header]", meta.BearerMethodsSupported)
	}
}

// rpcBody builds a JSON-RPC tools/call request body for the named tool.
func rpcBody(tool string) string {
	return `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tool + `","arguments":{}}}`
}

// callMCP posts a JSON-RPC body to the OAuth-wrapped /mcp handler with the given
// bearer (empty = none) and returns the response. The wrapped handler is the
// middleware around a stub that records whether the request reached it.
func callMCP(t *testing.T, v *oauthValidator, bearer, body string) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	var reached bool
	stub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		// Prove the body was restored intact for the downstream handler.
		got, _ := io.ReadAll(r.Body)
		if string(got) != body {
			t.Errorf("downstream body = %q, want %q (peek must restore it)", got, body)
		}
		w.WriteHeader(http.StatusOK)
	})
	h := v.middleware(stub)

	req := httptest.NewRequest(http.MethodPost, "https://scrim-mcp.example"+mcpPath, strings.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, reached
}

// TestOAuthMiddleware is the table of accept/reject paths through the bearer +
// scope gate.
func TestOAuthMiddleware(t *testing.T) {
	as := newFakeAS(t)
	v := newTestValidator(t, as, "https://scrim-mcp.example")
	wantMeta := "https://scrim-mcp.example" + protectedResourceMetadataPath

	valid := func(scope string) string {
		return as.mint(t, mintOpts{aud: testAudience, sub: "u-1", scope: scope})
	}

	t.Run("read scope allows a read tool", func(t *testing.T) {
		rec, reached := callMCP(t, v, valid(scopeRead), rpcBody("list"))
		if !reached || rec.Code != http.StatusOK {
			t.Fatalf("code = %d reached = %v, want 200 + reached", rec.Code, reached)
		}
	})

	t.Run("write scope allows a read tool (write dominates read)", func(t *testing.T) {
		rec, reached := callMCP(t, v, valid(scopeWrite), rpcBody("list"))
		if !reached || rec.Code != http.StatusOK {
			t.Fatalf("code = %d reached = %v, want 200 + reached", rec.Code, reached)
		}
	})

	t.Run("write scope allows a write tool", func(t *testing.T) {
		rec, reached := callMCP(t, v, valid(scopeWrite), rpcBody("write_file"))
		if !reached || rec.Code != http.StatusOK {
			t.Fatalf("code = %d reached = %v, want 200 + reached", rec.Code, reached)
		}
	})

	t.Run("read scope on a write tool is 403 insufficient_scope", func(t *testing.T) {
		rec, reached := callMCP(t, v, valid(scopeRead), rpcBody("write_file"))
		if reached {
			t.Fatal("write tool must not reach the handler without scrim:write")
		}
		if rec.Code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403", rec.Code)
		}
		assertChallenge(t, rec, "insufficient_scope", scopeWrite, wantMeta)
	})

	t.Run("no scopes on a read tool is 403", func(t *testing.T) {
		rec, reached := callMCP(t, v, valid(""), rpcBody("list"))
		if reached || rec.Code != http.StatusForbidden {
			t.Fatalf("code = %d reached = %v, want 403", rec.Code, reached)
		}
		assertChallenge(t, rec, "insufficient_scope", scopeRead, wantMeta)
	})

	t.Run("absent token is 401", func(t *testing.T) {
		rec, reached := callMCP(t, v, "", rpcBody("list"))
		if reached || rec.Code != http.StatusUnauthorized {
			t.Fatalf("code = %d reached = %v, want 401", rec.Code, reached)
		}
		// No credentials: bare challenge, resource_metadata only, no error code.
		wa := rec.Header().Get("WWW-Authenticate")
		if !strings.Contains(wa, "resource_metadata="+quote(wantMeta)) || strings.Contains(wa, "error=") {
			t.Errorf("WWW-Authenticate = %q, want a bare resource_metadata challenge", wa)
		}
	})

	t.Run("malformed token is 401 invalid_token", func(t *testing.T) {
		rec, reached := callMCP(t, v, "not-a-jwt", rpcBody("list"))
		if reached || rec.Code != http.StatusUnauthorized {
			t.Fatalf("code = %d reached = %v, want 401", rec.Code, reached)
		}
		assertChallenge(t, rec, "invalid_token", "", wantMeta)
	})

	t.Run("expired token is 401", func(t *testing.T) {
		tok := as.mint(t, mintOpts{aud: testAudience, sub: "u-1", scope: scopeRead, exp: time.Now().Add(-time.Minute)})
		rec, reached := callMCP(t, v, tok, rpcBody("list"))
		if reached || rec.Code != http.StatusUnauthorized {
			t.Fatalf("code = %d reached = %v, want 401", rec.Code, reached)
		}
		assertChallenge(t, rec, "invalid_token", "", wantMeta)
	})

	t.Run("wrong audience is 401", func(t *testing.T) {
		tok := as.mint(t, mintOpts{aud: "some-other-resource", sub: "u-1", scope: scopeRead})
		rec, reached := callMCP(t, v, tok, rpcBody("list"))
		if reached || rec.Code != http.StatusUnauthorized {
			t.Fatalf("code = %d reached = %v, want 401", rec.Code, reached)
		}
		assertChallenge(t, rec, "invalid_token", "", wantMeta)
	})

	t.Run("wrong issuer is 401", func(t *testing.T) {
		tok := as.mint(t, mintOpts{iss: "https://evil.example", aud: testAudience, sub: "u-1", scope: scopeRead})
		rec, reached := callMCP(t, v, tok, rpcBody("list"))
		if reached || rec.Code != http.StatusUnauthorized {
			t.Fatalf("code = %d reached = %v, want 401", rec.Code, reached)
		}
		assertChallenge(t, rec, "invalid_token", "", wantMeta)
	})

	t.Run("non-tools-call request needs only a valid token (no scope)", func(t *testing.T) {
		// A tools/list carries no tool name; a valid token with NO scopes passes.
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
		rec, reached := callMCP(t, v, valid(""), body)
		if !reached || rec.Code != http.StatusOK {
			t.Fatalf("code = %d reached = %v, want 200 + reached", rec.Code, reached)
		}
	})

	t.Run("batch-wrapped write tool with a read token is 403 (no fail-open)", func(t *testing.T) {
		// A one-element JSON-RPC array wrapping a write tool must NOT bypass the
		// scope gate: the batch requires scrim:write.
		body := `[{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write_file","arguments":{}}}]`
		rec, reached := callMCP(t, v, valid(scopeRead), body)
		if reached {
			t.Fatal("array-wrapped write tool must not reach the handler without scrim:write")
		}
		if rec.Code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403", rec.Code)
		}
		assertChallenge(t, rec, "insufficient_scope", scopeWrite, wantMeta)
	})

	t.Run("batch of two reads with a read token is allowed", func(t *testing.T) {
		body := `[{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list"}},` +
			`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"read_file"}}]`
		rec, reached := callMCP(t, v, valid(scopeRead), body)
		if !reached || rec.Code != http.StatusOK {
			t.Fatalf("code = %d reached = %v, want 200 + reached", rec.Code, reached)
		}
	})

	t.Run("batch mixing a read and a write with a read token is 403", func(t *testing.T) {
		body := `[{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list"}},` +
			`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"share_canvas"}}]`
		rec, reached := callMCP(t, v, valid(scopeRead), body)
		if reached || rec.Code != http.StatusForbidden {
			t.Fatalf("code = %d reached = %v, want 403", rec.Code, reached)
		}
		assertChallenge(t, rec, "insufficient_scope", scopeWrite, wantMeta)
	})

	t.Run("garbage body that is neither object nor array fails closed (403)", func(t *testing.T) {
		rec, reached := callMCP(t, v, valid(scopeRead), `not json at all`)
		if reached || rec.Code != http.StatusForbidden {
			t.Fatalf("code = %d reached = %v, want 403 (fail closed)", rec.Code, reached)
		}
		assertChallenge(t, rec, "insufficient_scope", scopeWrite, wantMeta)
	})

	t.Run("oversized body is 413", func(t *testing.T) {
		big := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write_file","arguments":{"content":"` +
			strings.Repeat("A", maxRPCPeekBytes+1) + `"}}}`
		rec, reached := callMCP(t, v, valid(scopeWrite), big)
		if reached || rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("code = %d reached = %v, want 413", rec.Code, reached)
		}
	})
}

// TestResourceBaseDerivation proves the resource URL is derived from the request
// (honoring X-Forwarded-Proto) when no Resource is configured.
func TestResourceBaseDerivation(t *testing.T) {
	as := newFakeAS(t)
	v := newTestValidator(t, as, "") // no configured resource -> derive per request

	req := httptest.NewRequest(http.MethodGet, "http://scrim-mcp.internal"+protectedResourceMetadataPath, nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	if got := v.resourceBase(req); got != "https://scrim-mcp.internal" {
		t.Errorf("resourceBase = %q, want https://scrim-mcp.internal (X-Forwarded-Proto honored)", got)
	}
}

func TestRequiredScope(t *testing.T) {
	cases := map[string]string{
		"list": scopeRead, "read_file": scopeRead, "list_grants": scopeRead, "path": scopeRead,
		"add": scopeWrite, "write_file": scopeWrite, "share_canvas": scopeWrite, "push": scopeWrite,
		"some_future_tool": scopeWrite, // unmapped -> fail closed to write
	}
	for tool, want := range cases {
		t.Run(tool, func(t *testing.T) {
			if got := requiredScope(tool); got != want {
				t.Errorf("requiredScope(%q) = %q, want %q", tool, got, want)
			}
		})
	}
}

// assertChallenge checks a WWW-Authenticate Bearer challenge carries the
// expected error, scope (when non-empty), and resource_metadata pointer.
func assertChallenge(t *testing.T, rec *httptest.ResponseRecorder, wantErr, wantScope, wantMeta string) {
	t.Helper()
	wa := rec.Header().Get("WWW-Authenticate")
	if !strings.HasPrefix(wa, "Bearer ") {
		t.Fatalf("WWW-Authenticate = %q, want a Bearer challenge", wa)
	}
	if wantErr != "" && !strings.Contains(wa, "error="+quote(wantErr)) {
		t.Errorf("WWW-Authenticate = %q, want error=%q", wa, wantErr)
	}
	if wantScope != "" && !strings.Contains(wa, "scope="+quote(wantScope)) {
		t.Errorf("WWW-Authenticate = %q, want scope=%q", wa, wantScope)
	}
	if !strings.Contains(wa, "resource_metadata="+quote(wantMeta)) {
		t.Errorf("WWW-Authenticate = %q, want resource_metadata=%q", wa, wantMeta)
	}
}

func quote(s string) string { return `"` + s + `"` }
