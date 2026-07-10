package mcpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jedwards1230/scrim/internal/config"
)

// verifyToken runs a minted token through the validator's real coreoidc verifier
// so the returned *coreoidc.IDToken is exactly what the middleware feeds
// actorFromToken -- the derivation is tested against a genuinely-verified token,
// never a hand-built one.
func verifyToken(t *testing.T, v *oauthValidator, token string) actor {
	t.Helper()
	idt, err := v.verifier.Verify(context.Background(), token)
	if err != nil {
		t.Fatalf("verifying token: %v", err)
	}
	return actorFromToken(idt)
}

// TestActorFromToken maps a validated JWT's sub/email/groups onto an actor.
func TestActorFromToken(t *testing.T) {
	as := newFakeAS(t)
	v := newTestValidator(t, as, "https://scrim-mcp.example")

	cases := []struct {
		name string
		opts mintOpts
		want actor
	}{
		{
			name: "sub email and groups array",
			opts: mintOpts{aud: testAudience, sub: "u-1", extra: map[string]any{
				"email":  "alice@example.com",
				"groups": []string{"eng", "sre"},
			}},
			want: actor{ID: "u-1", Email: "alice@example.com", Groups: []string{"eng", "sre"}},
		},
		{
			name: "sub only yields id with empty email and nil groups",
			opts: mintOpts{aud: testAudience, sub: "u-2"},
			want: actor{ID: "u-2"},
		},
		{
			name: "email present groups absent leaves groups nil",
			opts: mintOpts{aud: testAudience, sub: "u-3", extra: map[string]any{
				"email": "bob@example.com",
			}},
			want: actor{ID: "u-3", Email: "bob@example.com"},
		},
		{
			name: "empty groups array leaves groups nil",
			opts: mintOpts{aud: testAudience, sub: "u-4", extra: map[string]any{
				"email":  "carol@example.com",
				"groups": []string{},
			}},
			want: actor{ID: "u-4", Email: "carol@example.com"},
		},
		{
			name: "defensive: groups as a comma-delimited string is split",
			opts: mintOpts{aud: testAudience, sub: "u-5", extra: map[string]any{
				"email":  "dave@example.com",
				"groups": "eng, sre ,ops",
			}},
			want: actor{ID: "u-5", Email: "dave@example.com", Groups: []string{"eng", "sre", "ops"}},
		},
		{
			name: "exotic groups shape does not cost the email",
			opts: mintOpts{aud: testAudience, sub: "u-6", extra: map[string]any{
				"email":  "erin@example.com",
				"groups": []int{1, 2}, // neither a string array nor a string
			}},
			want: actor{ID: "u-6", Email: "erin@example.com"}, // email recovered, groups nil
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := verifyToken(t, v, as.mint(t, tc.opts))
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("actorFromToken = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestActorContextOAuthPrecedence proves the JWT-derived OAuth actor is
// authoritative over the HMAC header plane, that the HMAC plane still works when
// no OAuth actor is present (OAuth-off behaviour), and that neither present
// leaves the request anonymous.
func TestActorContextOAuthPrecedence(t *testing.T) {
	const secret = "s3cr3t"
	s := &server{identitySecret: secret}

	// A request carrying validly-signed HMAC X-Forwarded-User-* headers for a
	// DIFFERENT principal than the OAuth actor, so precedence is observable.
	hmacReq := func() *mcp.CallToolRequest {
		return &mcp.CallToolRequest{Extra: &mcp.RequestExtra{
			Header: signedHeaders("hmac-user", "hmac@example.com", "hmac-group", secret),
		}}
	}

	t.Run("OAuth actor wins over a valid HMAC actor", func(t *testing.T) {
		ctx := ctxWithOAuthActor(context.Background(), actor{ID: "jwt-user", Email: "jwt@example.com", Groups: []string{"jwt-group"}})
		ctx = s.actorContext(ctx, hmacReq())
		a, ok := actorFromContext(ctx)
		if !ok {
			t.Fatal("expected an actor in context")
		}
		want := actor{ID: "jwt-user", Email: "jwt@example.com", Groups: []string{"jwt-group"}}
		if !reflect.DeepEqual(a, want) {
			t.Errorf("actor = %+v, want the JWT actor %+v (HMAC must be ignored)", a, want)
		}
	})

	t.Run("no OAuth actor falls back to the HMAC plane", func(t *testing.T) {
		ctx := s.actorContext(context.Background(), hmacReq())
		a, ok := actorFromContext(ctx)
		if !ok {
			t.Fatal("expected the HMAC actor in context")
		}
		if a.ID != "hmac-user" || a.Email != "hmac@example.com" {
			t.Errorf("actor = %+v, want the HMAC-derived actor", a)
		}
	})

	t.Run("neither OAuth nor HMAC leaves ctx anonymous", func(t *testing.T) {
		req := &mcp.CallToolRequest{Extra: &mcp.RequestExtra{Header: http.Header{}}}
		ctx := s.actorContext(context.Background(), req)
		if _, ok := actorFromContext(ctx); ok {
			t.Error("no identity at all must stay anonymous")
		}
	})
}

// bearerRoundTripper adds an Authorization bearer (and any extra headers) to
// every outbound request, so the MCP streamable-HTTP client presents the
// minted OAuth token (and optionally spoofed HMAC identity headers) the way a
// real client would.
type bearerRoundTripper struct {
	base   http.RoundTripper
	bearer string
	extra  http.Header
}

func (rt bearerRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Header.Set("Authorization", "Bearer "+rt.bearer)
	for k, vs := range rt.extra {
		for _, v := range vs {
			r.Header.Set(k, v)
		}
	}
	return rt.base.RoundTrip(r)
}

// capturedActor is the X-Scrim-Actor-* attribution the recording fake hub saw
// on a hubBackend request, plus the bearer it rode.
type capturedActor struct {
	auth, id, email, groups string
}

// runListOverHTTP stands up the full streamable-HTTP stack (newHTTPHandler with
// a real oauthValidator, hub mode pointed at a recording fake hub), drives a
// real MCP `list` tools/call over HTTP presenting the given bearer token and
// extra headers (e.g. spoofed HMAC identity), and returns the attribution the
// hub recorded. The HMAC plane is armed with hmacSecret so precedence is
// exercised end-to-end. This proves empirically that the go-sdk's
// StreamableHTTPHandler derives the tool-call handler ctx from the inbound HTTP
// r.Context() -- the whole design hinges on it.
func runListOverHTTP(t *testing.T, as *fakeAS, v *oauthValidator, hmacSecret, token string, extra http.Header) capturedActor {
	t.Helper()
	t.Setenv(IdentitySecretEnv, hmacSecret) // arm the HMAC plane so precedence is meaningful

	var mu sync.Mutex
	var got capturedActor
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = capturedActor{
			auth:   r.Header.Get("Authorization"),
			id:     r.Header.Get(hdrActorID),
			email:  r.Header.Get(hdrActorEmail),
			groups: r.Header.Get(hdrActorGroups),
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer hub.Close()

	cfg := config.Config{Dir: t.TempDir(), Host: "127.0.0.1", Port: 7799}
	handler := newHTTPHandler(cfg, "test", &HubTarget{BaseURL: hub.URL, Token: "admin-token"}, v)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	transport := &mcp.StreamableClientTransport{
		Endpoint: ts.URL + mcpPath,
		HTTPClient: &http.Client{Transport: bearerRoundTripper{
			base:   http.DefaultTransport,
			bearer: token,
			extra:  extra,
		}},
		DisableStandaloneSSE: true,
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0"}, nil)
	session, err := client.Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatalf("client Connect over streamable-HTTP: %v", err)
	}
	defer func() { _ = session.Close() }()

	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "list"}); err != nil {
		t.Fatalf("CallTool(list): %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	return got
}

// TestOAuthAttributionEndToEnd is the LOAD-BEARING integration test: it drives a
// real MCP `list` tools/call over the full streamable-HTTP stack and asserts the
// JWT-derived actor propagated all the way to the hub's X-Scrim-Actor-* headers.
// The client additionally presents valid HMAC X-Forwarded-User-* headers for a
// different principal, so precedence (JWT wins) is exercised end-to-end.
func TestOAuthAttributionEndToEnd(t *testing.T) {
	const hmacSecret = "integration-hmac-secret"
	as := newFakeAS(t)
	v := newTestValidator(t, as, "https://scrim-mcp.example")

	token := as.mint(t, mintOpts{aud: testAudience, sub: "jwt-user", scope: scopeRead, extra: map[string]any{
		"email":  "jwt@example.com",
		"groups": []string{"eng", "sre"},
	}})
	// Validly-signed HMAC headers for a DIFFERENT principal; the JWT actor must win.
	spoof := signedHeaders("hmac-user", "hmac@example.com", "hmac-group", hmacSecret)

	got := runListOverHTTP(t, as, v, hmacSecret, token, spoof)

	if got.auth != "Bearer admin-token" {
		t.Errorf("hub Authorization = %q, want Bearer admin-token", got.auth)
	}
	// If this fails with an empty actor id, the SDK did NOT propagate r.Context()
	// into the handler ctx -- the design's core assumption is broken.
	if got.id != "jwt-user" {
		t.Fatalf("X-Scrim-Actor-Id = %q, want jwt-user (SDK must propagate r.Context() into the tool-call ctx)", got.id)
	}
	if got.email != "jwt@example.com" {
		t.Errorf("X-Scrim-Actor-Email = %q, want jwt@example.com (JWT actor must win over HMAC)", got.email)
	}
	if got.groups != "eng,sre" {
		t.Errorf("X-Scrim-Actor-Groups = %q, want eng,sre", got.groups)
	}
}

// TestOAuthEmptySubDoesNotShadowHMAC proves the sub-guard: a valid token whose
// `sub` is empty must NOT stash an (empty-ID) OAuth actor, since precedence is
// absolute and doing so would shadow a valid HMAC actor with blank
// X-Scrim-Actor-* headers -- an attribution downgrade. With a sub-less token
// present AND valid HMAC headers, the HMAC actor must win.
func TestOAuthEmptySubDoesNotShadowHMAC(t *testing.T) {
	const hmacSecret = "integration-hmac-secret"
	as := newFakeAS(t)
	v := newTestValidator(t, as, "https://scrim-mcp.example")

	// A valid token (sig/iss/aud/exp all check out) but with NO subject.
	token := as.mint(t, mintOpts{aud: testAudience, sub: "", scope: scopeRead})
	hmac := signedHeaders("hmac-user", "hmac@example.com", "hmac-group", hmacSecret)

	got := runListOverHTTP(t, as, v, hmacSecret, token, hmac)

	if got.id != "hmac-user" || got.email != "hmac@example.com" {
		t.Fatalf("actor = {id:%q email:%q}, want the HMAC actor (empty-sub OAuth token must not shadow it)", got.id, got.email)
	}
	if got.groups != "hmac-group" {
		t.Errorf("X-Scrim-Actor-Groups = %q, want hmac-group", got.groups)
	}
}
