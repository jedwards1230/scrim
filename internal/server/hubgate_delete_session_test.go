package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jedwards1230/scrim/internal/canvas"
	"github.com/jedwards1230/scrim/internal/usertoken"
)

// TestSessionOwnerMayDeleteCanvas pins the gate widening the shell's Delete
// menu item needs: a browser session that may WRITE a canvas may delete it, a
// viewer with only a view grant may not, and an anonymous caller is
// unauthorized. Modelled on the duplication branch (TestSessionOwnerMayCopyCanvas),
// which this shares its authorization check with.
func TestSessionOwnerMayDeleteCanvas(t *testing.T) {
	s, auth, idp := newOIDCHub(t)
	ownedCanvas(t, s, "alices", "alice@example.com")
	ownedCanvas(t, s, "shared", "alice@example.com")
	if err := canvas.AddGrant(s.metaDir, "shared", canvas.Grant{Kind: canvas.GrantUser, Target: "bob@example.com"}); err != nil {
		t.Fatal(err)
	}

	alice := sessionFor(t, auth, idp, "sub-alice", "alice@example.com", nil)
	bob := sessionFor(t, auth, idp, "sub-bob", "bob@example.com", nil)

	t.Run("view-only session DELETE -> 403", func(t *testing.T) {
		rec := do(t, s, sessionReq(http.MethodDelete, "/api/canvases/shared", bob, nil))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("view-only session DELETE = %d, want 403", rec.Code)
		}
		if !canvas.Exists(s.canvasesDir, "shared") {
			t.Error("a refused delete must not have removed the canvas")
		}
	})

	t.Run("anonymous DELETE -> 401", func(t *testing.T) {
		rec := do(t, s, sessionReq(http.MethodDelete, "/api/canvases/alices", nil, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous DELETE = %d, want 401", rec.Code)
		}
		if !canvas.Exists(s.canvasesDir, "alices") {
			t.Error("an unauthorized delete must not have removed the canvas")
		}
	})

	t.Run("session owner DELETE -> 204", func(t *testing.T) {
		rec := do(t, s, sessionReq(http.MethodDelete, "/api/canvases/alices", alice, nil))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("session-owner DELETE = %d, want 204 (body %q)", rec.Code, rec.Body.String())
		}
		if canvas.Exists(s.canvasesDir, "alices") {
			t.Error("the canvas survived its owner's delete")
		}
	})
}

// TestDeleteBearerPathsUnchanged proves the session branch left the machine
// plane exactly as it was: the admin push token still deletes anything, and a
// user token still deletes only what its owner owns.
func TestDeleteBearerPathsUnchanged(t *testing.T) {
	s, _, _ := newOIDCHub(t)
	ownedCanvas(t, s, "alices", "alice@example.com")
	ownedCanvas(t, s, "carols", "carol@example.com")
	ownedCanvas(t, s, "adminy", "admin")

	if rec := do(t, s, adminReq(http.MethodDelete, "/api/canvases/adminy", nil)); rec.Code != http.StatusNoContent {
		t.Fatalf("admin DELETE = %d, want 204 (body %q)", rec.Code, rec.Body.String())
	}

	raw, _, err := s.tokens.Mint("alice-cli", "alice@example.com", nil, usertoken.Allowance{})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	tokenReq := func(path string) *http.Request {
		r := httptest.NewRequest(http.MethodDelete, path, bytes.NewReader(nil))
		r.Header.Set("Authorization", "Bearer "+raw)
		return r
	}
	if rec := do(t, s, tokenReq("/api/canvases/carols")); rec.Code != http.StatusForbidden {
		t.Fatalf("user-token DELETE of another's canvas = %d, want 403", rec.Code)
	}
	if rec := do(t, s, tokenReq("/api/canvases/alices")); rec.Code != http.StatusNoContent {
		t.Fatalf("user-token DELETE of own canvas = %d, want 204 (body %q)", rec.Code, rec.Body.String())
	}
}

// TestIsCanvasDeletePath is a table check on the path classifier the gate uses
// to route a session DELETE to the ownership branch: only the EXACT canvas
// path qualifies, never a grant delete or anything deeper.
func TestIsCanvasDeletePath(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		want   bool
	}{
		{"delete canvas", http.MethodDelete, "/api/canvases/c1", true},
		{"get canvas is not a mutation", http.MethodGet, "/api/canvases/c1", false},
		{"post canvases is a create", http.MethodPost, "/api/canvases", false},
		{"delete grant", http.MethodDelete, "/api/canvases/c1/grants/bob@example.com", false},
		{"delete a file", http.MethodDelete, "/api/canvases/c1/files/a.html", false},
		{"trailing slash", http.MethodDelete, "/api/canvases/c1/", false},
		{"no id", http.MethodDelete, "/api/canvases/", false},
		{"wrong prefix", http.MethodDelete, "/api/tokens/t1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isCanvasDeletePath(tt.method, tt.path); got != tt.want {
				t.Errorf("isCanvasDeletePath(%q, %q) = %v, want %v", tt.method, tt.path, got, tt.want)
			}
		})
	}
}
