package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jedwards1230/scrim/internal/oidc"
	"github.com/jedwards1230/scrim/internal/session"
)

// sessionIDOf decodes the session id out of a minted cookie, by handing it
// back to the authenticator that signed it.
func sessionIDOf(t *testing.T, auth *oidc.Authenticator, cookie *http.Cookie) oidc.Session {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	sess, ok := auth.SessionFromRequest(req)
	if !ok {
		t.Fatal("the minted session cookie does not verify")
	}
	return sess
}

// stale rewrites id's registry record with a LastSeen far enough in the past
// that renewal is due on the next request. The store's clock is real time and
// not injectable from this package, so the RECORD is aged instead of the
// clock -- Create is the supported way to do that, and it is exactly the
// shape a record has after the browser has been away for a while.
func stale(t *testing.T, s *Server, sess oidc.Session, expiresIn time.Duration) session.Record {
	t.Helper()
	now := time.Now()
	rec := session.Record{
		ID:        sess.ID,
		Subject:   sess.Subject,
		Email:     sess.Email,
		CreatedAt: now.Add(-2 * time.Hour),
		LastSeen:  now.Add(-2 * session.TouchInterval),
		ExpiresAt: now.Add(expiresIn),
	}
	if err := s.sessions.Create(rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

// setCookieNamed returns the Set-Cookie the response issues for name, or nil.
func setCookieNamed(res *http.Response, name string) *http.Cookie {
	for _, c := range res.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// readAs performs an ordinary authenticated read carrying cookie.
func readAs(t *testing.T, s *Server, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/canvases", nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	return rec
}

// TestSessionRenewalExtendsAndReissues is the headline behavior: using a
// session slides its registry deadline out by a full idle window AND re-issues
// the cookie to match, so the cookie's own signed expiry never becomes the
// binding constraint.
func TestSessionRenewalExtendsAndReissues(t *testing.T) {
	s, auth, idp := newOIDCHub(t)
	cookie := idp.Login(t, auth, "/")
	sess := sessionIDOf(t, auth, cookie)
	before := stale(t, s, sess, time.Hour)

	rec := readAs(t, s, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated read = %d, want 200", rec.Code)
	}

	// The registry record moved out by an idle window.
	after, ok, err := s.sessions.Lookup(sess.ID)
	if !ok || err != nil {
		t.Fatalf("Lookup after renewal = (%v, %v), want (true, nil)", ok, err)
	}
	if !after.ExpiresAt.After(before.ExpiresAt) {
		t.Errorf("registry ExpiresAt = %v, want it later than %v", after.ExpiresAt, before.ExpiresAt)
	}
	if want := time.Now().Add(oidc.DefaultSessionTTL); after.ExpiresAt.Before(want.Add(-time.Minute)) {
		t.Errorf("registry ExpiresAt = %v, want roughly one idle window out (%v)", after.ExpiresAt, want)
	}

	// The cookie was re-issued, still verifies, and carries the new expiry.
	issued := setCookieNamed(rec.Result(), oidc.SessionCookieName)
	if issued == nil {
		t.Fatal("no session cookie was re-issued, want one carrying the extended expiry")
	}
	renewed := sessionIDOf(t, auth, issued)
	if renewed.ID != sess.ID || renewed.Subject != sess.Subject || renewed.Email != sess.Email {
		t.Errorf("re-issued cookie identity = %+v, want the same session/claims as %+v", renewed, sess)
	}
	if renewed.Expiry != after.ExpiresAt.Unix() {
		t.Errorf("re-issued cookie expiry = %d, want it to match the registry's %d", renewed.Expiry, after.ExpiresAt.Unix())
	}
	if renewed.Expiry <= before.ExpiresAt.Unix() {
		t.Errorf("re-issued cookie expiry = %d, want it later than the pre-renewal %d", renewed.Expiry, before.ExpiresAt.Unix())
	}
	// And the renewed cookie is itself usable.
	if code := readAs(t, s, issued).Code; code != http.StatusOK {
		t.Errorf("read with the re-issued cookie = %d, want 200", code)
	}
}

// TestSessionRenewalIsThrottled pins the cost guarantee at the gate: a session
// used again inside TouchInterval gets no registry write and no Set-Cookie.
func TestSessionRenewalIsThrottled(t *testing.T) {
	s, auth, idp := newOIDCHub(t)
	cookie := idp.Login(t, auth, "/")
	sess := sessionIDOf(t, auth, cookie)

	// A freshly minted session's LastSeen is now, so every one of these reads
	// is inside the throttle window.
	before, ok, err := s.sessions.Lookup(sess.ID)
	if !ok || err != nil {
		t.Fatalf("Lookup = (%v, %v), want (true, nil)", ok, err)
	}
	path := filepath.Join(s.metaDir, "sessions.json")
	stat0, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	for i := range 3 {
		rec := readAs(t, s, cookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("read #%d = %d, want 200", i, rec.Code)
		}
		if c := setCookieNamed(rec.Result(), oidc.SessionCookieName); c != nil {
			t.Errorf("read #%d re-issued the session cookie inside the throttle, want no Set-Cookie", i)
		}
	}

	after, _, _ := s.sessions.Lookup(sess.ID)
	if !after.ExpiresAt.Equal(before.ExpiresAt) || !after.LastSeen.Equal(before.LastSeen) {
		t.Errorf("record moved inside the throttle: %+v -> %+v", before, after)
	}
	stat1, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if stat1.Size() != stat0.Size() || !stat1.ModTime().Equal(stat0.ModTime()) {
		t.Error("the registry file was rewritten by throttled reads, want no disk write")
	}
}

// TestSessionRenewalNeverResurrects: a session revoked (or expired) is gone,
// and the request that presents its cookie must be rejected rather than
// renewing the record back into existence.
func TestSessionRenewalNeverResurrects(t *testing.T) {
	tests := []struct {
		name string
		kill func(t *testing.T, s *Server, sess oidc.Session)
	}{
		{
			name: "revoked from the devices page",
			kill: func(t *testing.T, s *Server, sess oidc.Session) {
				t.Helper()
				if revoked, err := s.sessions.Revoke(sess.ID, sess.Subject); !revoked || err != nil {
					t.Fatalf("Revoke = (%v, %v), want (true, nil)", revoked, err)
				}
			},
		},
		{
			name: "already lapsed",
			kill: func(t *testing.T, s *Server, sess oidc.Session) {
				t.Helper()
				// Age the record past its own expiry; the cookie is still
				// signed-valid, so only the registry stands between it and a
				// renewal.
				now := time.Now()
				if err := s.sessions.Create(session.Record{
					ID:        sess.ID,
					Subject:   sess.Subject,
					CreatedAt: now.Add(-2 * time.Hour),
					LastSeen:  now.Add(-2 * session.TouchInterval),
					ExpiresAt: now.Add(time.Second),
				}); err != nil {
					t.Fatal(err)
				}
				time.Sleep(1100 * time.Millisecond)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, auth, idp := newOIDCHub(t)
			cookie := idp.Login(t, auth, "/")
			sess := sessionIDOf(t, auth, cookie)
			tc.kill(t, s, sess)

			rec := readAs(t, s, cookie)
			if rec.Code == http.StatusOK {
				t.Errorf("read with a dead session = 200, want it rejected")
			}
			if c := setCookieNamed(rec.Result(), oidc.SessionCookieName); c != nil && c.MaxAge > 0 {
				t.Error("a dead session's cookie was re-issued, want no renewal")
			}
			if _, ok, err := s.sessions.Lookup(sess.ID); ok || err != nil {
				t.Errorf("Lookup after the request = (%v, %v), want (false, nil) -- the record came back", ok, err)
			}
		})
	}
}

// TestAdminPushTokenIsNeverRenewed keeps the recovery credential out of the
// renewal path entirely -- including when the session registry is corrupt,
// which is precisely when that credential has to work.
func TestAdminPushTokenIsNeverRenewed(t *testing.T) {
	s, _, _ := newOIDCHub(t)

	pushRead := func(t *testing.T) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/canvases", nil)
		req.Header.Set("Authorization", "Bearer test-push-token")
		rec := httptest.NewRecorder()
		s.routes().ServeHTTP(rec, req)
		return rec
	}

	rec := pushRead(t)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin push read = %d, want 200", rec.Code)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Errorf("admin push read set cookies %v, want none -- it carries no session to renew", rec.Result().Cookies())
	}

	// Corrupt the registry and reload it, as a restart would. The push token
	// still reads, and still sets nothing.
	if err := os.MkdirAll(s.metaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.metaDir, "sessions.json"), []byte("{ not the registry"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.sessions = session.New(s.metaDir, oidc.DefaultSessionTTL, oidc.DefaultSessionMaxLifetime)
	if err := s.sessions.Err(); err == nil {
		t.Fatal("the reloaded registry is not in its fail-closed state, want an error")
	}

	rec = pushRead(t)
	if rec.Code != http.StatusOK {
		t.Errorf("admin push read against a corrupt registry = %d, want 200 -- it is the recovery credential", rec.Code)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Errorf("admin push read set cookies %v, want none", rec.Result().Cookies())
	}
}

// TestDevicesPageShowsTheRenewedExpiry: the devices & access page reads its
// sign-in list from GET /api/sessions, and that request is itself renewed
// before the handler runs -- so the expiry it displays is the fresh one, not
// the value the record carried when the browser arrived.
func TestDevicesPageShowsTheRenewedExpiry(t *testing.T) {
	s, auth, idp := newOIDCHub(t)
	cookie := idp.Login(t, auth, "/")
	sess := sessionIDOf(t, auth, cookie)
	before := stale(t, s, sess, time.Hour)

	got, code := listSessions(t, s, cookie)
	if code != http.StatusOK {
		t.Fatalf("GET /api/sessions = %d, want 200", code)
	}
	var mine *sessionResponse
	for i := range got {
		if got[i].ID == sess.ID {
			mine = &got[i]
		}
	}
	if mine == nil {
		t.Fatalf("GET /api/sessions did not list the current session %q", sess.ID)
	}
	if !mine.ExpiresAt.After(before.ExpiresAt) {
		t.Errorf("listed expires_at = %v, want the renewed value (later than %v)", mine.ExpiresAt, before.ExpiresAt)
	}
	stored, _, _ := s.sessions.Lookup(sess.ID)
	if !mine.ExpiresAt.Equal(stored.ExpiresAt) {
		t.Errorf("listed expires_at = %v, want the registry's %v", mine.ExpiresAt, stored.ExpiresAt)
	}
}

// TestSessionRenewalStopsAtTheAbsoluteCap: a session already sitting at its
// absolute cap keeps working but is not extended -- the gate must not slide
// it past the cap just because it is in use.
func TestSessionRenewalStopsAtTheAbsoluteCap(t *testing.T) {
	s, auth, idp := newOIDCHub(t)
	cookie := idp.Login(t, auth, "/")
	sess := sessionIDOf(t, auth, cookie)

	// Created a full max-lifetime ago bar an hour, so the cap is one hour out
	// and a fresh idle window (7d) would overshoot it wildly.
	now := time.Now()
	created := now.Add(-oidc.DefaultSessionMaxLifetime + time.Hour)
	if err := s.sessions.Create(session.Record{
		ID:        sess.ID,
		Subject:   sess.Subject,
		CreatedAt: created,
		LastSeen:  now.Add(-2 * session.TouchInterval),
		ExpiresAt: now.Add(30 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	if code := readAs(t, s, cookie).Code; code != http.StatusOK {
		t.Fatalf("read = %d, want 200", code)
	}

	after, ok, err := s.sessions.Lookup(sess.ID)
	if !ok || err != nil {
		t.Fatalf("Lookup = (%v, %v), want (true, nil)", ok, err)
	}
	hardStop := created.Add(oidc.DefaultSessionMaxLifetime)
	if after.ExpiresAt.After(hardStop) {
		t.Errorf("ExpiresAt = %v, past the absolute cap at %v", after.ExpiresAt, hardStop)
	}
	if !after.ExpiresAt.Equal(hardStop) {
		t.Errorf("ExpiresAt = %v, want it clamped exactly to the cap %v", after.ExpiresAt, hardStop)
	}
}
