package mcpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// signedHeaders builds a header set carrying a validly-signed forwarded identity
// for the given secret.
func signedHeaders(id, email, groups, secret string) http.Header {
	h := http.Header{}
	h.Set(hdrFwdUserID, id)
	h.Set(hdrFwdUserEmail, email)
	h.Set(hdrFwdUserGroups, groups)
	h.Set(hdrFwdUserSig, signForwardedIdentity(id, email, groups, secret))
	return h
}

func TestVerifyForwardedIdentity(t *testing.T) {
	const secret = "shared-hmac-secret"

	t.Run("valid signature is trusted", func(t *testing.T) {
		h := signedHeaders("u-1", "alice@example.com", "eng, sre", secret)
		a, ok := verifyForwardedIdentity(h, secret)
		if !ok {
			t.Fatal("valid signature not trusted")
		}
		want := actor{ID: "u-1", Email: "alice@example.com", Groups: []string{"eng", "sre"}}
		if !reflect.DeepEqual(a, want) {
			t.Errorf("actor = %+v, want %+v", a, want)
		}
	})

	t.Run("empty secret disables trust", func(t *testing.T) {
		h := signedHeaders("u-1", "alice@example.com", "", secret)
		if _, ok := verifyForwardedIdentity(h, ""); ok {
			t.Error("empty secret must not trust any identity")
		}
	})

	t.Run("nil headers not trusted", func(t *testing.T) {
		if _, ok := verifyForwardedIdentity(nil, secret); ok {
			t.Error("nil headers must not be trusted")
		}
	})

	t.Run("missing signature not trusted", func(t *testing.T) {
		h := http.Header{}
		h.Set(hdrFwdUserEmail, "alice@example.com")
		if _, ok := verifyForwardedIdentity(h, secret); ok {
			t.Error("a request with no signature must not be trusted")
		}
	})

	t.Run("malformed base64 signature not trusted", func(t *testing.T) {
		h := signedHeaders("u-1", "alice@example.com", "", secret)
		h.Set(hdrFwdUserSig, "!!!not base64!!!")
		if _, ok := verifyForwardedIdentity(h, secret); ok {
			t.Error("a malformed signature must not be trusted")
		}
	})

	t.Run("tampered email fails verification", func(t *testing.T) {
		h := signedHeaders("u-1", "alice@example.com", "", secret)
		// Swap the email after signing: the signature no longer covers it.
		h.Set(hdrFwdUserEmail, "attacker@example.com")
		if _, ok := verifyForwardedIdentity(h, secret); ok {
			t.Error("a tampered email must fail verification")
		}
	})

	t.Run("wrong secret fails verification", func(t *testing.T) {
		h := signedHeaders("u-1", "alice@example.com", "", secret)
		if _, ok := verifyForwardedIdentity(h, "a-different-secret"); ok {
			t.Error("a signature under a different secret must fail")
		}
	})
}

func TestActorContext(t *testing.T) {
	const secret = "s3cr3t"
	s := &server{identitySecret: secret}

	t.Run("verified identity threads an actor", func(t *testing.T) {
		req := &mcp.CallToolRequest{Extra: &mcp.RequestExtra{Header: signedHeaders("u-9", "bob@example.com", "team", secret)}}
		ctx := s.actorContext(context.Background(), req)
		a, ok := actorFromContext(ctx)
		if !ok {
			t.Fatal("expected an actor in context")
		}
		if a.Email != "bob@example.com" {
			t.Errorf("actor email = %q, want bob@example.com", a.Email)
		}
	})

	t.Run("unsigned request leaves ctx anonymous", func(t *testing.T) {
		req := &mcp.CallToolRequest{Extra: &mcp.RequestExtra{Header: http.Header{}}}
		ctx := s.actorContext(context.Background(), req)
		if _, ok := actorFromContext(ctx); ok {
			t.Error("an unsigned request must not thread an actor")
		}
	})

	t.Run("nil request (stdio) is anonymous", func(t *testing.T) {
		ctx := s.actorContext(context.Background(), nil)
		if _, ok := actorFromContext(ctx); ok {
			t.Error("a nil request must not thread an actor")
		}
	})
}

// TestHubBackendAttachesActorHeaders proves the hubBackend re-emits a verified
// actor as X-Scrim-Actor-* on top of the admin bearer, and omits them when the
// call carries no actor.
func TestHubBackendAttachesActorHeaders(t *testing.T) {
	var gotAuth, gotActorEmail, gotActorGroups string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotActorEmail = r.Header.Get(hdrActorEmail)
		gotActorGroups = r.Header.Get(hdrActorGroups)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer ts.Close()
	b := newHubBackend(ts.URL, "admin-token")

	t.Run("actor in ctx is forwarded", func(t *testing.T) {
		ctx := ctxWithActor(context.Background(), actor{ID: "u-1", Email: "alice@example.com", Groups: []string{"eng", "sre"}})
		if _, err := b.List(ctx); err != nil {
			t.Fatalf("List: %v", err)
		}
		if gotAuth != "Bearer admin-token" {
			t.Errorf("Authorization = %q, want Bearer admin-token", gotAuth)
		}
		if gotActorEmail != "alice@example.com" {
			t.Errorf("actor email header = %q, want alice@example.com", gotActorEmail)
		}
		if gotActorGroups != "eng,sre" {
			t.Errorf("actor groups header = %q, want eng,sre", gotActorGroups)
		}
	})

	t.Run("no actor in ctx omits the headers", func(t *testing.T) {
		gotActorEmail, gotActorGroups = "", ""
		if _, err := b.List(context.Background()); err != nil {
			t.Fatalf("List: %v", err)
		}
		if gotActorEmail != "" || gotActorGroups != "" {
			t.Errorf("actor headers leaked without an actor: email=%q groups=%q", gotActorEmail, gotActorGroups)
		}
	})
}
