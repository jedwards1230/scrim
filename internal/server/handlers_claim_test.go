package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jedwards1230/scrim/internal/canvas"
	"github.com/jedwards1230/scrim/internal/config"
	"github.com/jedwards1230/scrim/internal/usertoken"
)

// TestClaimTransfersOwnership proves #55 core: a user-token principal claims a
// legacy (admin-owned) canvas and becomes its owner; a second principal then
// gets 409, and a missing canvas is 404.
func TestClaimTransfersOwnership(t *testing.T) {
	s, _ := newHubTestServer(t, []string{"127.0.0.0/8"}, "")

	// Admin creates a legacy canvas (owner=admin).
	if rec := do(t, s, adminReq(http.MethodPost, "/api/canvases", []byte(`{"id":"legacy"}`))); rec.Code != http.StatusCreated {
		t.Fatalf("admin create = %d", rec.Code)
	}

	aliceTok, _, err := s.tokens.Mint("alice", "alice@example.com", nil, usertoken.Allowance{})
	if err != nil {
		t.Fatal(err)
	}
	bobTok, _, err := s.tokens.Mint("bob", "bob@example.com", nil, usertoken.Allowance{})
	if err != nil {
		t.Fatal(err)
	}

	claim := func(id, raw string) int {
		r := httptest.NewRequest(http.MethodPost, "/api/canvases/"+id+"/claim", nil)
		r.Header.Set("Authorization", "Bearer "+raw)
		return do(t, s, r).Code
	}

	// Alice claims the admin-owned canvas → 200, now hers.
	if code := claim("legacy", aliceTok); code != http.StatusOK {
		t.Fatalf("alice claim = %d, want 200", code)
	}
	owner, _, err := canvas.GetOwnerGrants(s.metaDir, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if owner != "alice@example.com" {
		t.Errorf("owner after claim = %q, want alice@example.com", owner)
	}

	// Alice re-claims → still 200 (idempotent).
	if code := claim("legacy", aliceTok); code != http.StatusOK {
		t.Errorf("alice re-claim = %d, want 200 (idempotent)", code)
	}

	// Bob claims a canvas already owned by alice → 409.
	if code := claim("legacy", bobTok); code != http.StatusConflict {
		t.Errorf("bob claim of alice's canvas = %d, want 409", code)
	}

	// Claim of a non-existent canvas → 404.
	if code := claim("ghost", aliceTok); code != http.StatusNotFound {
		t.Errorf("claim missing canvas = %d, want 404", code)
	}

	// An anonymous claim (no credential) → 401.
	anon := httptest.NewRequest(http.MethodPost, "/api/canvases/legacy/claim", nil)
	if rec := do(t, s, anon); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous claim = %d, want 401", rec.Code)
	}
}

// TestStartupMigratesLegacyOwners proves the NewHub sweep stamps owner="admin"
// on a canvas whose on-disk meta predates ownership.
func TestStartupMigratesLegacyOwners(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{Dir: dir, Host: "127.0.0.1", Port: 0, IdleTimeout: time.Hour, NoAuth: true}

	// Plant a legacy canvas directory with content but NO meta file (no owner).
	canvasDir := filepath.Join(cfg.CanvasesDir(), "legacy")
	if err := os.MkdirAll(canvasDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canvasDir, "index.html"), []byte("<h1>legacy</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Constructing the hub runs the one-time ownership sweep.
	s, err := NewHub(cfg, HubOptions{PushToken: "test-push-token"})
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	owner, _, err := canvas.GetOwnerGrants(s.metaDir, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if owner != "admin" {
		t.Errorf("legacy owner after migration = %q, want admin", owner)
	}
}
