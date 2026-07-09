package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jedwards1230/scrim/internal/canvas"
	"github.com/jedwards1230/scrim/internal/usertoken"
)

// sessionReq builds a grant-mutation request carrying a session cookie (and a
// JSON body for POST). It models exactly what the browser share dialog sends:
// a same-site fetch riding the SameSite=Lax session cookie, no bearer.
func sessionReq(method, path string, cookie *http.Cookie, body []byte) *http.Request {
	var r *http.Request
	if body != nil {
		r = httptest.NewRequest(method, path, bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	return r
}

// TestSessionOwnerMayMutateGrants pins the PR2 gate widening: a browser session
// that OWNS a canvas may add and remove its grants natively (no user token),
// while a non-owner session is refused and an anonymous caller is unauthorized.
// It also proves the pre-existing user-token grant path is unchanged.
func TestSessionOwnerMayMutateGrants(t *testing.T) {
	s, auth, idp := newOIDCHub(t)
	ownedCanvas(t, s, "alices", "alice@example.com")

	alice := sessionFor(t, auth, idp, "sub-alice", "alice@example.com", nil)
	bob := sessionFor(t, auth, idp, "sub-bob", "bob@example.com", nil)

	t.Run("session owner POST grant -> 201", func(t *testing.T) {
		rec := do(t, s, sessionReq(http.MethodPost, "/api/canvases/alices/grants", alice,
			[]byte(`{"kind":"user","target":"bob@example.com"}`)))
		if rec.Code != http.StatusCreated {
			t.Fatalf("session-owner POST grant = %d, want 201 (body %q)", rec.Code, rec.Body.String())
		}
	})

	t.Run("session owner DELETE grant -> 204", func(t *testing.T) {
		rec := do(t, s, sessionReq(http.MethodDelete, "/api/canvases/alices/grants/bob@example.com", alice, nil))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("session-owner DELETE grant = %d, want 204 (body %q)", rec.Code, rec.Body.String())
		}
	})

	t.Run("session non-owner POST grant -> 403", func(t *testing.T) {
		rec := do(t, s, sessionReq(http.MethodPost, "/api/canvases/alices/grants", bob,
			[]byte(`{"kind":"user","target":"carol@example.com"}`)))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("session-non-owner POST grant = %d, want 403", rec.Code)
		}
	})

	t.Run("session non-owner DELETE grant -> 403", func(t *testing.T) {
		rec := do(t, s, sessionReq(http.MethodDelete, "/api/canvases/alices/grants/everyone", bob, nil))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("session-non-owner DELETE grant = %d, want 403", rec.Code)
		}
	})

	t.Run("anonymous POST grant -> 401", func(t *testing.T) {
		rec := do(t, s, sessionReq(http.MethodPost, "/api/canvases/alices/grants", nil,
			[]byte(`{"kind":"everyone"}`)))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous POST grant = %d, want 401", rec.Code)
		}
	})
}

// TestUserTokenGrantPathUnchanged proves the PR2 session widening left the
// pre-existing user-token grant path intact: a token whose owner owns the
// canvas and whose allowance permits the target still adds a grant (201), and
// an out-of-allowance target is still refused by the handler (403).
func TestUserTokenGrantPathUnchanged(t *testing.T) {
	s, _, _ := newOIDCHub(t)
	ownedCanvas(t, s, "alices", "alice@example.com")

	raw, _, err := s.tokens.Mint("alice-cli", "alice@example.com", nil,
		usertoken.Allowance{Emails: []string{"carol@example.com"}})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	// Allowed target -> 201.
	req := httptest.NewRequest(http.MethodPost, "/api/canvases/alices/grants",
		bytes.NewReader([]byte(`{"kind":"user","target":"carol@example.com"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+raw)
	if rec := do(t, s, req); rec.Code != http.StatusCreated {
		t.Fatalf("user-token allowed grant = %d, want 201 (body %q)", rec.Code, rec.Body.String())
	}

	// Out-of-allowance target -> 403 (handler-enforced allowance, unchanged).
	req2 := httptest.NewRequest(http.MethodPost, "/api/canvases/alices/grants",
		bytes.NewReader([]byte(`{"kind":"user","target":"mallory@example.com"}`)))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer "+raw)
	if rec := do(t, s, req2); rec.Code != http.StatusForbidden {
		t.Fatalf("user-token out-of-allowance grant = %d, want 403", rec.Code)
	}

	// Sanity: the allowed grant really landed.
	_, grants, err := canvas.GetOwnerGrants(s.metaDir, "alices")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, g := range grants {
		if g.Kind == canvas.GrantUser && g.Target == "carol@example.com" {
			found = true
		}
	}
	if !found {
		t.Errorf("carol grant not recorded; grants = %+v", grants)
	}
}

// TestIsGrantMutationPath is a table-driven unit check on the path classifier
// the gate uses to route a session write to the owner-authorization branch.
func TestIsGrantMutationPath(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		want   bool
	}{
		{"post grants", http.MethodPost, "/api/canvases/c1/grants", true},
		{"delete grant by ref", http.MethodDelete, "/api/canvases/c1/grants/bob@example.com", true},
		{"delete everyone", http.MethodDelete, "/api/canvases/c1/grants/everyone", true},
		{"get grants is not a mutation", http.MethodGet, "/api/canvases/c1/grants", false},
		{"post without grants suffix", http.MethodPost, "/api/canvases/c1/claim", false},
		{"delete grants with no ref", http.MethodDelete, "/api/canvases/c1/grants", false},
		{"delete grants trailing slash only", http.MethodDelete, "/api/canvases/c1/grants/", false},
		{"post grant on wrong prefix", http.MethodPost, "/api/tokens", false},
		{"post to canvas root", http.MethodPost, "/api/canvases", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isGrantMutationPath(tt.method, tt.path); got != tt.want {
				t.Errorf("isGrantMutationPath(%q, %q) = %v, want %v", tt.method, tt.path, got, tt.want)
			}
		})
	}
}
