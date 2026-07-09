package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jedwards1230/scrim/internal/canvas"
)

// getIndexHTML fetches the gallery at "/" as a browser would (Accept: text/html),
// optionally carrying a session cookie, and returns the body + status.
func getIndexHTML(t *testing.T, s *Server, cookie *http.Cookie) (string, int) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept", "text/html")
	// Loopback RemoteAddr so a non-OIDC hub's CIDR gate (127.0.0.0/8) admits the
	// read; an OIDC hub ignores it and gates on the cookie instead.
	req.RemoteAddr = "127.0.0.1:12345"
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	return rec.Body.String(), rec.Code
}

// TestIndexIdentityChromeUnderOIDC proves the gallery gains the identity chip,
// logout form, tokens link, per-card badges, and a Share control for the
// owner -- all rendered server-side from the session's claims.
func TestIndexIdentityChromeUnderOIDC(t *testing.T) {
	s, auth, idp := newOIDCHub(t)
	ownedCanvas(t, s, "alices", "alice@example.com")

	alice := sessionFor(t, auth, idp, "sub-alice", "alice@example.com", []string{"eng"})
	body, code := getIndexHTML(t, s, alice)
	if code != http.StatusOK {
		t.Fatalf("GET / (alice) = %d, want 200", code)
	}

	wants := []string{
		`class="chip"`,          // identity chip present
		`action="/auth/logout"`, // POST logout form
		`href="/tokens"`,        // link to the my-tokens page
		`data-share="alices"`,   // Share control on her owned canvas
		`>Owned<`,               // owned badge
		`>Private<`,             // visibility qualifier (no broad grant yet)
	}
	for _, w := range wants {
		if !strings.Contains(body, w) {
			t.Errorf("OIDC index missing %q", w)
		}
	}
	// The owner's own card must not carry a Claim control (nothing to claim).
	if strings.Contains(body, `data-claim="alices"`) {
		t.Error("owner card should not offer a Claim control")
	}
}

// TestIndexSharedBadgeAndClaim proves a non-owner viewer sees a "Shared with
// you" badge (never the ACL) and a legacy admin-owned canvas offers a Claim.
func TestIndexSharedBadgeAndClaim(t *testing.T) {
	s, auth, idp := newOIDCHub(t)
	ownedCanvas(t, s, "alices", "alice@example.com")
	if err := canvas.AddGrant(s.metaDir, "alices", canvas.Grant{Kind: canvas.GrantUser, Target: "bob@example.com"}); err != nil {
		t.Fatal(err)
	}
	// A legacy admin-owned canvas, shared to everyone so bob can see it (private
	// by default it would be invisible, and an unclaimable-because-unseen canvas
	// is a no-op). Still admin-owned, so bob may claim it.
	ownedCanvas(t, s, "legacy", "admin")
	if err := canvas.AddGrant(s.metaDir, "legacy", canvas.Grant{Kind: canvas.GrantEveryone}); err != nil {
		t.Fatal(err)
	}

	bob := sessionFor(t, auth, idp, "sub-bob", "bob@example.com", nil)
	body, code := getIndexHTML(t, s, bob)
	if code != http.StatusOK {
		t.Fatalf("GET / (bob) = %d, want 200", code)
	}
	if !strings.Contains(body, `>Shared with you<`) {
		t.Error("shared-with recipient should see a 'Shared with you' badge")
	}
	// Bob is not the owner, so no Share control on alice's canvas.
	if strings.Contains(body, `data-share="alices"`) {
		t.Error("non-owner must not get a Share control")
	}
	// The legacy admin-owned canvas offers a Claim control to a logged-in viewer.
	if !strings.Contains(body, `data-claim="legacy"`) {
		t.Error("a claimable admin-owned canvas should offer a Claim control")
	}
}

// TestIndexNoIdentityChromeWithoutOIDC proves a non-OIDC hub renders no new
// identity chrome -- no chip, no logout, no tokens link, no share/claim
// controls -- while keeping the theme toggle it always had.
func TestIndexNoIdentityChromeWithoutOIDC(t *testing.T) {
	s, _ := newHubTestServer(t, []string{"127.0.0.0/8"}, "")
	if _, err := canvas.Create(s.canvasesDir, s.metaDir, "c1", "", "", "", ""); err != nil {
		t.Fatal(err)
	}

	body, code := getIndexHTML(t, s, nil)
	if code != http.StatusOK {
		t.Fatalf("GET / (non-OIDC hub) = %d, want 200", code)
	}
	if !strings.Contains(body, `id="theme-toggle"`) {
		t.Error("theme toggle must remain on a non-OIDC hub")
	}
	for _, unwanted := range []string{`class="chip"`, `action="/auth/logout"`, `href="/tokens"`, `data-share=`, `data-claim=`, `id="share-dialog"`} {
		if strings.Contains(body, unwanted) {
			t.Errorf("non-OIDC index unexpectedly contains %q", unwanted)
		}
	}
}

// TestTokensPageHubOnlyAndRendered proves GET /tokens is hub-only and renders
// the management page for a session.
func TestTokensPageHubOnlyAndRendered(t *testing.T) {
	// Default daemon: no such route.
	def, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/tokens", nil)
	rec := httptest.NewRecorder()
	def.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /tokens on default daemon = %d, want 404 (hub-only)", rec.Code)
	}

	// OIDC hub with a session: the page renders.
	s, auth, idp := newOIDCHub(t)
	alice := sessionFor(t, auth, idp, "sub-alice", "alice@example.com", nil)
	pReq := httptest.NewRequest(http.MethodGet, "/tokens", nil)
	pReq.Header.Set("Accept", "text/html")
	pReq.AddCookie(alice)
	pRec := httptest.NewRecorder()
	s.routes().ServeHTTP(pRec, pReq)
	if pRec.Code != http.StatusOK {
		t.Fatalf("GET /tokens (alice) = %d, want 200", pRec.Code)
	}
	body := pRec.Body.String()
	for _, w := range []string{"Your tokens", `id="mint-btn"`, `id="token-list"`} {
		if !strings.Contains(body, w) {
			t.Errorf("tokens page missing %q", w)
		}
	}
}
