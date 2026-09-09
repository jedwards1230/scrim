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

// TestAccessPageListsBrowserSessionsFirst pins the reversal in #145: browser
// sign-ins ARE listed now, above the tokens, each individually signable-out.
// The old copy claiming they can't be listed must be gone -- keeping it would
// be an outright false statement about what the page in front of the reader
// does.
func TestAccessPageListsBrowserSessionsFirst(t *testing.T) {
	body := aliceAccessPage(t)

	for _, want := range []string{
		`<section id="sessions">`,
		`<div id="session-list">`,
		"Browsers you're signed in from",
		"scrim records no IP addresses",
		`fetch("/api/sessions"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("access page's sessions section missing %q", want)
		}
	}

	// The stale, now-false claims must not survive anywhere on the page.
	for _, gone := range []string{
		"Browser sign-ins are not listed here",
		"sessions are stateless",
		"rotate the server's session",
	} {
		if strings.Contains(body, gone) {
			t.Errorf("access page still claims %q, which is false now that sessions are listed and revocable", gone)
		}
	}

	sessions := strings.Index(body, `<div id="session-list">`)
	tokens := strings.Index(body, `<div id="token-list">`)
	if sessions < 0 || tokens < 0 {
		t.Fatalf("access page missing a list (sessions=%d tokens=%d)", sessions, tokens)
	}
	if sessions > tokens {
		t.Errorf("token list precedes the session list (sessions=%d tokens=%d): sign-ins lead the page", sessions, tokens)
	}
}

// TestAccessPageMarksAndGuardsTheCurrentSession pins the two properties that
// keep the sign-out control safe to press: the session making the request is
// labeled, and ending THAT one confirms first because it signs you out here.
func TestAccessPageMarksAndGuardsTheCurrentSession(t *testing.T) {
	body := aliceAccessPage(t)

	for _, want := range []string{
		`badge.textContent = "This browser";`,
		`btn.textContent = s.current ? "Sign out here" : "Sign out";`,
		"window.confirm(",
		"function deviceLabel(ua) {",
		// The honest fallback: an unrecognised user agent is shown raw rather
		// than labeled with a guess. (Asserted as the whole if/return pair --
		// html/template strips JS comments, so the surrounding prose isn't in
		// the rendered page to match on.)
		`if (browser) return browser;`,
		`if (os) return os;`,
		`    return ua;`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("access page script missing %q", want)
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

// TestSessionExpiryUsesAForwardFormatter pins the fix for a bug that only
// showed up in a browser: the expiry line was rendered with relative(), which
// clamps a negative age to zero, so a session with ten hours left on it read
// "expires just now" -- on a devices page that reads as "you are about to be
// signed out". A future instant needs until(), relative()'s forward-looking
// twin.
func TestSessionExpiryUsesAForwardFormatter(t *testing.T) {
	body := aliceAccessPage(t)

	if !strings.Contains(body, "until(s.expires_at)") {
		t.Error("session expiry is not rendered with the forward-looking until() formatter")
	}
	if strings.Contains(body, "relative(s.expires_at)") {
		t.Error("session expiry uses relative(), which renders every future instant as just now")
	}
	if !strings.Contains(body, "function until(iso)") {
		t.Error("the shared time helpers do not define until()")
	}
}

// TestSignOutHereUsesTheLogoutFlow pins a fix for a bug that shipped: signing
// the CURRENT browser out revoked the registry record and reloaded the page.
// That looks correct and is not -- the reload is a browser navigation, so the
// read gate sends it into /auth/login, where the IdP's own session cookie
// (which scrim never touched) authenticates it again and mints a fresh
// session. The user saw a page that never signed them out.
//
// Ending this browser's session has to go through POST /auth/logout, which
// clears scrim's cookies, drops the registry entry, AND redirects to the
// IdP's end_session_endpoint. It is the same trap RP-initiated logout exists
// to avoid.
func TestSignOutHereUsesTheLogoutFlow(t *testing.T) {
	body := aliceAccessPage(t)

	if !strings.Contains(body, `form.action = "/auth/logout"`) {
		t.Error("signing out the current browser does not go through the logout flow")
	}
	if !strings.Contains(body, "if (s.current) { signOutHere(); return; }") {
		t.Error("the current session is not routed to the logout flow before the DELETE")
	}
	if strings.Contains(body, "window.location.reload()") {
		t.Error("the current session is still ended with a reload, which the IdP silently re-authenticates")
	}
}
