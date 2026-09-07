package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jedwards1230/scrim/internal/canvas"
)

// accountMenuMarkers are the substrings that prove the shared account menu
// rendered: a real menu button (not the old inert chip), the popover it
// controls, and the two account actions inside it -- Tokens as a link and Log
// out as a real POST form (never a GET).
var accountMenuMarkers = []string{
	`id="account-btn"`,
	`aria-controls="account-menu"`,
	`id="account-menu"`,
	`href="/tokens"`,
	`method="POST" action="/auth/logout"`,
}

// TestAccountMenuOnGalleryAndShell proves the account menu -- the viewer's
// identity plus Tokens and Log out -- renders identically on the gallery AND
// on the canvas shell. The shell is the load-bearing half: a collaborator who
// arrives on a share link lands there, and before this it had no logout and no
// route to /tokens at all.
func TestAccountMenuOnGalleryAndShell(t *testing.T) {
	s, auth, idp := newOIDCHub(t)
	ownedCanvas(t, s, "alices", "alice@example.com")
	alice := sessionFor(t, auth, idp, "sub-alice", "alice@example.com", nil)

	gallery, code := getIndexHTML(t, s, alice)
	if code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", code)
	}
	shell, shellCode := shellGET(t, s, "alices", alice)
	if shellCode != http.StatusOK {
		t.Fatalf("GET /c/alices/ = %d, want 200", shellCode)
	}

	for _, page := range []struct {
		name string
		body string
	}{{"gallery", gallery}, {"shell", shell}} {
		t.Run(page.name, func(t *testing.T) {
			for _, want := range append(accountMenuMarkers, "alice@example.com") {
				if !strings.Contains(page.body, want) {
					t.Errorf("%s account menu missing %q", page.name, want)
				}
			}
			// The old top-level Tokens/Log out buttons are gone: both actions
			// now live inside the menu, so neither competes with canvas-level
			// chrome in the header.
			if strings.Contains(page.body, `<a class="btn" href="/tokens">`) {
				t.Errorf("%s still renders a top-level Tokens button", page.name)
			}
		})
	}
}

// TestAccountMenuShowsFullIdentity proves the menu shows the display name AND
// the email in full (the button label alone is truncated), so a viewer can read
// the whole identity the session carries.
func TestAccountMenuShowsFullIdentity(t *testing.T) {
	s, auth, idp := newOIDCHub(t)
	idp.Name = "Alice Example"
	alice := sessionFor(t, auth, idp, "sub-alice", "alice@example.com", nil)

	body, code := getIndexHTML(t, s, alice)
	if code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", code)
	}
	for _, want := range []string{
		`<div class="who">Alice Example</div>`,
		`<div class="when">alice@example.com</div>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("account menu missing %q:\n%s", want, body)
		}
	}
}

// TestAccountMenuAbsentWithoutOIDC pins the same rule the identity chrome has
// always followed (see TestIndexNoIdentityChromeWithoutOIDC): with no OIDC
// there is no principal, so neither the gallery nor the shell emits an account
// menu -- and in particular no logout form on a server that has no sessions.
func TestAccountMenuAbsentWithoutOIDC(t *testing.T) {
	s, _ := newHubTestServer(t, []string{"127.0.0.0/8"}, "")
	if _, err := canvas.Create(s.canvasesDir, s.metaDir, "plain", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	writeCanvas(t, s, "plain", "<html><body>x</body></html>")

	gallery, code := getIndexHTML(t, s, nil)
	if code != http.StatusOK {
		t.Fatalf("GET / (non-OIDC hub) = %d, want 200", code)
	}
	req := httptest.NewRequest(http.MethodGet, "/c/plain/", nil)
	req.Header.Set("Accept", "text/html")
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /c/plain/ (non-OIDC hub) = %d, want 200", rec.Code)
	}

	for _, page := range []struct {
		name string
		body string
	}{{"gallery", gallery}, {"shell", rec.Body.String()}} {
		for _, unwanted := range accountMenuMarkers {
			if strings.Contains(page.body, unwanted) {
				t.Errorf("non-OIDC %s unexpectedly contains %q", page.name, unwanted)
			}
		}
	}
}

// TestMenusAreNotAriaMenus pins the item-3 decision: the header popovers are
// plain button/link popovers, NOT ARIA menus. role="menu" promises Up/Down/
// Home/End roving-tabindex navigation that this markup does not implement, so
// claiming it would tell assistive tech one thing and behave as another. Tab
// already traverses these correctly.
func TestMenusAreNotAriaMenus(t *testing.T) {
	s, auth, idp := newOIDCHub(t)
	ownedCanvas(t, s, "alices", "alice@example.com")
	alice := sessionFor(t, auth, idp, "sub-alice", "alice@example.com", nil)

	body, code := shellGET(t, s, "alices", alice)
	if code != http.StatusOK {
		t.Fatalf("GET /c/alices/ = %d, want 200", code)
	}
	for _, forbidden := range []string{`role="menu"`, `role="menuitem"`, `aria-haspopup=`} {
		if strings.Contains(body, forbidden) {
			t.Errorf("shell still claims the ARIA menu pattern (%q) without implementing it", forbidden)
		}
	}
	// ...while the state the popover DOES implement is still announced.
	for _, want := range []string{`aria-expanded="false"`, `aria-controls="canvas-menu"`} {
		if !strings.Contains(body, want) {
			t.Errorf("shell menu button missing %q", want)
		}
	}
}
