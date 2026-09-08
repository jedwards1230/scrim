package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// accessPageHTML fetches GET /tokens (the "devices & access" page) for a
// session principal.
func accessPageHTML(t *testing.T, s *Server, cookie *http.Cookie) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/tokens", nil)
	req.Header.Set("Accept", "text/html")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /tokens = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

// aliceAccessPage is the common fixture: an OIDC hub, a logged-in principal,
// and that principal's rendered access page.
func aliceAccessPage(t *testing.T) string {
	t.Helper()
	s, auth, idp := newOIDCHub(t)
	alice := sessionFor(t, auth, idp, "sub-alice", "alice@example.com", nil)
	return accessPageHTML(t, s, alice)
}

// TestAccessPageLeadsWithTheAccessList pins the page's reframing: it is an
// audit view first and a minting utility second. The list of what currently has
// access must come BEFORE the mint form in the document, because the order the
// markup is written in is the order a reader (and a screen reader) meets it.
func TestAccessPageLeadsWithTheAccessList(t *testing.T) {
	body := aliceAccessPage(t)

	heading := strings.Index(body, "<h1>Devices &amp; access</h1>")
	list := strings.Index(body, `<div id="token-list">`)
	mint := strings.Index(body, `<section id="mint">`)
	if heading < 0 || list < 0 || mint < 0 {
		t.Fatalf("access page missing heading/list/mint section (heading=%d list=%d mint=%d)", heading, list, mint)
	}
	if list > mint {
		t.Errorf("mint form precedes the access list (list=%d, mint=%d): the page must lead with what has access", list, mint)
	}
	if heading > list {
		t.Errorf("heading appears after the access list (heading=%d, list=%d)", heading, list)
	}
}

// TestAccessPageDisclosesSessionsAreNotListed pins the honesty note. The page
// covers tokens only: OIDC sessions are stateless and non-revocable
// (docs/PRD.md §12), so the page must say so rather than let a reader assume
// "devices & access" covers their browser sign-ins too.
func TestAccessPageDisclosesSessionsAreNotListed(t *testing.T) {
	body := aliceAccessPage(t)

	for _, want := range []string{
		`id="session-note"`,
		"<strong>tokens only</strong>",
		"Browser sign-ins are not listed here",
		"sessions are stateless",
		"rotate the server's session",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("access page's honesty note missing %q", want)
		}
	}
}

// TestAccessPageCollapsesRevoked pins the revoked disclosure: revoked tokens
// are history, not access, so they live behind a real button wired with
// aria-expanded/aria-controls rather than cluttering the live list.
func TestAccessPageCollapsesRevoked(t *testing.T) {
	body := aliceAccessPage(t)

	for _, want := range []string{
		`<button id="revoked-toggle" class="btn" type="button" aria-expanded="false" aria-controls="revoked-list">`,
		`<div id="revoked-list" hidden>`,
		"Show revoked",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("revoked disclosure missing %q", want)
		}
	}
	// The whole disclosure starts hidden and is only revealed when there IS
	// something revoked to show.
	if !strings.Contains(body, `<div class="disclosure" id="revoked-wrap" hidden>`) {
		t.Error("revoked disclosure wrapper is not hidden by default")
	}
}

// TestAccessPageLeadsEachEntryWithRecency pins the per-entry framing: recency
// leads, a never-used token says so outright (the old page simply omitted the
// bit, making "never" and "long ago" indistinguishable), and the list is sorted
// client-side by recency rather than by the API's creation order.
func TestAccessPageLeadsEachEntryWithRecency(t *testing.T) {
	body := aliceAccessPage(t)

	for _, want := range []string{
		`lead.textContent = "Last used " + relative(t.last_used);`,
		`lead.textContent = "Never used";`,
		"function byRecency(",
		"function staleLabel(",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("access page script missing %q", want)
		}
	}
}

// TestTimeFormatHelpersAreShared pins the single implementation of the
// relative/absolute timestamp pair: the canvas shell's version history and the
// access page's "last used" line render the same way because they run the same
// partial, not two copies that can drift.
func TestTimeFormatHelpersAreShared(t *testing.T) {
	s, auth, idp := newOIDCHub(t)
	ownedCanvas(t, s, "alices", "alice@example.com")
	alice := sessionFor(t, auth, idp, "sub-alice", "alice@example.com", nil)

	shell, code := shellGET(t, s, "alices", alice)
	if code != http.StatusOK {
		t.Fatalf("GET /c/alices/ = %d, want 200", code)
	}
	access := accessPageHTML(t, s, alice)

	for _, page := range []struct {
		name string
		body string
	}{{"shell", shell}, {"access", access}} {
		if n := strings.Count(page.body, "function relative(iso) {"); n != 1 {
			t.Errorf("%s page defines relative() %d times, want exactly 1 (the shared partial)", page.name, n)
		}
		if n := strings.Count(page.body, "function absolute(iso) {"); n != 1 {
			t.Errorf("%s page defines absolute() %d times, want exactly 1 (the shared partial)", page.name, n)
		}
	}
}
