package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/jedwards1230/scrim/internal/session"
	"github.com/jedwards1230/scrim/internal/usertoken"
)

// authedRead does an ordinary authenticated read (GET /api/canvases, a
// non-canvas read any authenticated principal may reach) and returns the status
// code -- 200 when the cookie authenticates, 401 when it no longer does. It is
// the "does this session still work?" probe the revocation tests turn on.
func authedRead(t *testing.T, s *Server, cookie *http.Cookie) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/canvases", nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	return rec.Code
}

// listSessions does GET /api/sessions with the given cookie and returns the
// decoded response plus the status code.
func listSessions(t *testing.T, s *Server, cookie *http.Cookie) ([]sessionResponse, int) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return nil, rec.Code
	}
	var out []sessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding GET /api/sessions: %v (body %q)", err, rec.Body.String())
	}
	return out, rec.Code
}

// deleteSession does DELETE /api/sessions/{id} with the given cookie.
func deleteSession(t *testing.T, s *Server, cookie *http.Cookie, id string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/api/sessions/"+id, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	return rec.Code
}

// TestSessionsListedForTheirOwnPrincipal covers the happy path: a login shows
// up on its owner's list, marked as the current browser, carrying the recorded
// user agent -- and nobody else's.
func TestSessionsListedForTheirOwnPrincipal(t *testing.T) {
	s, auth, idp := newOIDCHub(t)
	alice := sessionFor(t, auth, idp, "sub-alice", "alice@example.com", nil)
	bob := sessionFor(t, auth, idp, "sub-bob", "bob@example.com", nil)

	got, code := listSessions(t, s, alice)
	if code != http.StatusOK {
		t.Fatalf("GET /api/sessions = %d, want 200", code)
	}
	if len(got) != 1 {
		t.Fatalf("alice sees %d sessions, want exactly her own", len(got))
	}
	if !got[0].Current {
		t.Error("alice's own session is not marked current, want the page to say 'This browser'")
	}
	if got[0].UserAgent == "" {
		t.Error("session response carries no user agent, want the recorded one")
	}
	if got[0].CreatedAt.IsZero() || got[0].LastSeen.IsZero() || got[0].ExpiresAt.IsZero() {
		t.Errorf("session response timestamps = %+v, want all three populated", got[0])
	}

	bobs, _ := listSessions(t, s, bob)
	if len(bobs) != 1 || bobs[0].ID == got[0].ID {
		t.Errorf("bob sees %d sessions (%v), want only his own", len(bobs), bobs)
	}
}

// TestRevokedSessionIsRejectedOnItsNextRequest is the property the whole
// feature exists for: revoking a session ends it server-side, so the cookie
// that was working a moment ago authenticates nothing -- without waiting for
// its TTL.
func TestRevokedSessionIsRejectedOnItsNextRequest(t *testing.T) {
	s, auth, idp := newOIDCHub(t)
	laptop := sessionFor(t, auth, idp, "sub-alice", "alice@example.com", nil)
	phone := sessionFor(t, auth, idp, "sub-alice", "alice@example.com", nil)

	// The laptop cookie works before the revocation.
	if code := authedRead(t, s, laptop); code != http.StatusOK {
		t.Fatalf("read with a live session = %d, want 200", code)
	}

	// From the phone, find and end the laptop's session.
	got, _ := listSessions(t, s, phone)
	var laptopID string
	for _, sess := range got {
		if !sess.Current {
			laptopID = sess.ID
		}
	}
	if laptopID == "" {
		t.Fatalf("no other session found to revoke (got %d)", len(got))
	}
	if code := deleteSession(t, s, phone, laptopID); code != http.StatusNoContent {
		t.Fatalf("DELETE /api/sessions/{id} = %d, want 204", code)
	}

	// The laptop's very next request is no longer authenticated.
	if code := authedRead(t, s, laptop); code == http.StatusOK {
		t.Error("a revoked session still authenticates a read, want it rejected on the next request")
	}
	// The phone's own session is untouched.
	if code := authedRead(t, s, phone); code != http.StatusOK {
		t.Errorf("read with the surviving session = %d, want 200", code)
	}
}

// TestRevokeAnotherPrincipalsSessionIs404 mirrors handleRevokeToken exactly: a
// session belonging to someone else is a 404, never a 403 -- the response must
// not reveal that an id it can't touch exists.
func TestRevokeAnotherPrincipalsSessionIs404(t *testing.T) {
	s, auth, idp := newOIDCHub(t)
	alice := sessionFor(t, auth, idp, "sub-alice", "alice@example.com", nil)
	bob := sessionFor(t, auth, idp, "sub-bob", "bob@example.com", nil)

	bobs, _ := listSessions(t, s, bob)
	if len(bobs) != 1 {
		t.Fatalf("bob has %d sessions, want 1", len(bobs))
	}

	if code := deleteSession(t, s, alice, bobs[0].ID); code != http.StatusNotFound {
		t.Errorf("DELETE of another principal's session = %d, want 404 (not 403 -- existence must not leak)", code)
	}
	// An id that doesn't exist at all answers identically.
	if code := deleteSession(t, s, alice, "no-such-session"); code != http.StatusNotFound {
		t.Errorf("DELETE of an unknown session = %d, want 404", code)
	}
	// And bob is still signed in.
	if code := authedRead(t, s, bob); code != http.StatusOK {
		t.Errorf("bob's session was ended by alice's DELETE (read = %d), want it untouched", code)
	}
}

// TestLogoutEndsTheSessionServerSide pins that logout is a real revocation, not
// just a cookie clear: a copy of the cookie taken elsewhere stops working too.
func TestLogoutEndsTheSessionServerSide(t *testing.T) {
	s, auth, idp := newOIDCHub(t)
	cookie := sessionFor(t, auth, idp, "sub-alice", "alice@example.com", nil)

	if code := authedRead(t, s, cookie); code != http.StatusOK {
		t.Fatalf("read before logout = %d, want 200", code)
	}

	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("POST /auth/logout = %d, want 302", rec.Code)
	}

	// The same cookie value, replayed (as a stolen copy would be), is dead.
	if code := authedRead(t, s, cookie); code == http.StatusOK {
		t.Error("a logged-out session cookie still authenticates, want the record revoked server-side")
	}
}

// TestSessionEndpointsAreSessionOnly pins the plane: a user-token bearer and
// the admin push token are not accounts with devices. The user token gets the
// same 403 /api/tokens gives it; the admin token reaches the handler (it
// bypasses the gate by design) but has no subject, so it gets an empty list and
// a 404 -- never someone else's sessions.
func TestSessionEndpointsAreSessionOnly(t *testing.T) {
	s, auth, idp := newOIDCHub(t)
	alice := sessionFor(t, auth, idp, "sub-alice", "alice@example.com", nil)
	alices, _ := listSessions(t, s, alice)
	if len(alices) != 1 {
		t.Fatalf("alice has %d sessions, want 1", len(alices))
	}

	raw, _, err := s.tokens.Mint("cli", "alice@example.com", nil, usertoken.Allowance{})
	if err != nil {
		t.Fatalf("minting a user token: %v", err)
	}

	// A user token may not end its owner's browser sessions.
	req := httptest.NewRequest(http.MethodDelete, "/api/sessions/"+alices[0].ID, nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("DELETE /api/sessions with a user-token bearer = %d, want 403", rec.Code)
	}

	// The admin push token is the machine plane: no subject, so no devices.
	adminList := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	adminList.Header.Set("Authorization", "Bearer test-push-token")
	adminRec := httptest.NewRecorder()
	s.routes().ServeHTTP(adminRec, adminList)
	if adminRec.Code != http.StatusOK {
		t.Fatalf("GET /api/sessions with the admin token = %d, want 200", adminRec.Code)
	}
	var adminGot []sessionResponse
	if err := json.Unmarshal(adminRec.Body.Bytes(), &adminGot); err != nil {
		t.Fatal(err)
	}
	if len(adminGot) != 0 {
		t.Errorf("admin token listed %d sessions, want none -- it has no browser sign-ins of its own", len(adminGot))
	}

	adminDel := httptest.NewRequest(http.MethodDelete, "/api/sessions/"+alices[0].ID, nil)
	adminDel.Header.Set("Authorization", "Bearer test-push-token")
	adminDelRec := httptest.NewRecorder()
	s.routes().ServeHTTP(adminDelRec, adminDel)
	if adminDelRec.Code != http.StatusNotFound {
		t.Errorf("DELETE of a user's session with the admin token = %d, want 404", adminDelRec.Code)
	}
	if code := authedRead(t, s, alice); code != http.StatusOK {
		t.Errorf("alice's session was ended by the admin token (read = %d), want it untouched", code)
	}

	// An anonymous caller gets 401, not 403.
	anon := httptest.NewRequest(http.MethodDelete, "/api/sessions/"+alices[0].ID, nil)
	anonRec := httptest.NewRecorder()
	s.routes().ServeHTTP(anonRec, anon)
	if anonRec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous DELETE /api/sessions = %d, want 401", anonRec.Code)
	}
}

// TestCorruptSessionStoreFailsClosedButAdminStillWorks is the recovery
// invariant. A registry that cannot be parsed must reject every
// session-authenticated request (fail closed) -- and must NOT touch the admin
// push token, which is the credential an operator recovers the hub with and
// which never consults the store at all.
func TestCorruptSessionStoreFailsClosedButAdminStillWorks(t *testing.T) {
	s, auth, idp := newOIDCHub(t)
	cookie := sessionFor(t, auth, idp, "sub-alice", "alice@example.com", nil)
	if code := authedRead(t, s, cookie); code != http.StatusOK {
		t.Fatalf("read with a live session = %d, want 200", code)
	}

	// Corrupt the registry on disk and reload the hub's store from it, exactly
	// as a restart would.
	path := filepath.Join(s.metaDir, "sessions.json")
	if err := os.WriteFile(path, []byte("{ this is not the registry"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.sessions = session.New(s.metaDir)

	if code := authedRead(t, s, cookie); code == http.StatusOK {
		t.Error("a session cookie authenticated against a corrupt registry, want it rejected (fail closed)")
	}
	if _, code := listSessions(t, s, cookie); code == http.StatusOK {
		t.Errorf("GET /api/sessions against a corrupt registry = %d, want a failure rather than an empty list", code)
	}

	// The admin push token is untouched: it resolves before the session branch
	// and never reads the store.
	req := httptest.NewRequest(http.MethodGet, "/api/canvases", nil)
	req.Header.Set("Authorization", "Bearer test-push-token")
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("admin push-token read against a corrupt session registry = %d, want 200 -- it is the recovery path", rec.Code)
	}

	push := httptest.NewRequest(http.MethodPost, "/api/push/recovery", http.NoBody)
	push.Header.Set("Authorization", "Bearer test-push-token")
	pushRec := httptest.NewRecorder()
	s.routes().ServeHTTP(pushRec, push)
	if pushRec.Code == http.StatusUnauthorized || pushRec.Code == http.StatusForbidden {
		t.Errorf("admin push against a corrupt session registry = %d, want it ungated", pushRec.Code)
	}
}

// TestPreRegistrySessionCookieIsRejected pins decision 3 end-to-end: a cookie
// in the old, id-less format -- every cookie minted before this change -- no
// longer authenticates. Everyone logs in once on deploy, which is the intended
// cost of making sessions revocable at all.
func TestPreRegistrySessionCookieIsRejected(t *testing.T) {
	s, auth, idp := newOIDCHub(t)
	cookie := sessionFor(t, auth, idp, "sub-alice", "alice@example.com", nil)

	// Delete the record but keep the (still validly signed) cookie: that is
	// exactly the state a pre-registry cookie is in -- signed, unexpired, and
	// backed by nothing.
	got, _ := listSessions(t, s, cookie)
	if len(got) != 1 {
		t.Fatalf("expected 1 session, got %d", len(got))
	}
	if err := s.sessions.End(got[0].ID); err != nil {
		t.Fatal(err)
	}

	if code := authedRead(t, s, cookie); code == http.StatusOK {
		t.Error("a session cookie with no registry record authenticated a read, want it rejected")
	}
}
