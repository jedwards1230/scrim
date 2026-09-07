package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jedwards1230/scrim/internal/canvas"
	"github.com/jedwards1230/scrim/internal/usertoken"
)

// TestSessionOwnerMayCopyCanvas pins the gate widening the shell's Duplicate
// menu item needs (#126), modelled on the grant-mutation precedent: a browser
// session that may WRITE the SOURCE canvas may duplicate it, a non-owner
// session may not, and an anonymous caller is unauthorized. The source is the
// only thing this branch authorizes -- the shell never sends "overwrite", so a
// name collision comes back as a 409 rather than clobbering anything.
func TestSessionOwnerMayCopyCanvas(t *testing.T) {
	s, auth, idp := newOIDCHub(t)
	ownedCanvas(t, s, "alices", "alice@example.com")

	alice := sessionFor(t, auth, idp, "sub-alice", "alice@example.com", nil)
	bob := sessionFor(t, auth, idp, "sub-bob", "bob@example.com", nil)

	t.Run("session owner POST copy -> 200", func(t *testing.T) {
		rec := do(t, s, sessionReq(http.MethodPost, "/api/canvases/alices/copy", alice,
			[]byte(`{"to":"alices-copy"}`)))
		if rec.Code != http.StatusOK {
			t.Fatalf("session-owner POST copy = %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
		if !canvas.Exists(s.canvasesDir, "alices-copy") {
			t.Error("the duplicate canvas was not created")
		}
	})

	t.Run("session owner POST copy onto an existing target -> 409", func(t *testing.T) {
		// No "overwrite" in the body -- the shell never sends one, so a
		// collision must be a refusal, never a silent replace.
		rec := do(t, s, sessionReq(http.MethodPost, "/api/canvases/alices/copy", alice,
			[]byte(`{"to":"alices-copy"}`)))
		if rec.Code != http.StatusConflict {
			t.Fatalf("session-owner POST copy onto existing = %d, want 409", rec.Code)
		}
	})

	t.Run("session non-owner POST copy -> 403", func(t *testing.T) {
		rec := do(t, s, sessionReq(http.MethodPost, "/api/canvases/alices/copy", bob,
			[]byte(`{"to":"bobs-copy"}`)))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("session-non-owner POST copy = %d, want 403", rec.Code)
		}
		if canvas.Exists(s.canvasesDir, "bobs-copy") {
			t.Error("a refused copy must not have created the target")
		}
	})

	t.Run("anonymous POST copy -> 401", func(t *testing.T) {
		rec := do(t, s, sessionReq(http.MethodPost, "/api/canvases/alices/copy", nil,
			[]byte(`{"to":"anon-copy"}`)))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous POST copy = %d, want 401", rec.Code)
		}
	})
}

// TestCopyBearerPathsUnchanged proves the session branch left the two
// pre-existing machine-plane paths exactly as they were: the admin push token
// still copies anything, and a user token still copies only what its owner
// owns.
func TestCopyBearerPathsUnchanged(t *testing.T) {
	s, _, _ := newOIDCHub(t)
	ownedCanvas(t, s, "alices", "alice@example.com")
	ownedCanvas(t, s, "carols", "carol@example.com")

	// Admin bearer: unrestricted.
	if rec := do(t, s, adminReq(http.MethodPost, "/api/canvases/alices/copy",
		[]byte(`{"to":"admin-copy"}`))); rec.Code != http.StatusOK {
		t.Fatalf("admin POST copy = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}

	raw, _, err := s.tokens.Mint("alice-cli", "alice@example.com", nil, usertoken.Allowance{})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	// A user token whose owner owns the source: allowed.
	req := httptest.NewRequest(http.MethodPost, "/api/canvases/alices/copy",
		bytes.NewReader([]byte(`{"to":"token-copy"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+raw)
	if rec := do(t, s, req); rec.Code != http.StatusOK {
		t.Fatalf("user-token POST copy of own canvas = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}

	// ...and one whose owner does not: still refused, exactly as before.
	req2 := httptest.NewRequest(http.MethodPost, "/api/canvases/carols/copy",
		bytes.NewReader([]byte(`{"to":"stolen-copy"}`)))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer "+raw)
	if rec := do(t, s, req2); rec.Code != http.StatusForbidden {
		t.Fatalf("user-token POST copy of another's canvas = %d, want 403", rec.Code)
	}
}

// TestIsCopyPath is a table-driven unit check on the path classifier the gate
// uses to route a session write to the source-ownership branch.
func TestIsCopyPath(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		want   bool
	}{
		{"post copy", http.MethodPost, "/api/canvases/c1/copy", true},
		{"get copy is not a mutation", http.MethodGet, "/api/canvases/c1/copy", false},
		{"delete copy", http.MethodDelete, "/api/canvases/c1/copy", false},
		{"post grants is not copy", http.MethodPost, "/api/canvases/c1/grants", false},
		{"deeper path is not copy", http.MethodPost, "/api/canvases/c1/copy/extra", false},
		{"no sub-path", http.MethodPost, "/api/canvases/c1", false},
		{"wrong prefix", http.MethodPost, "/api/tokens/copy", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isCopyPath(tt.method, tt.path); got != tt.want {
				t.Errorf("isCopyPath(%q, %q) = %v, want %v", tt.method, tt.path, got, tt.want)
			}
		})
	}
}
