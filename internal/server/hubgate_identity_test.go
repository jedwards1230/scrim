package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/jedwards1230/scrim/internal/apiclient"
	"github.com/jedwards1230/scrim/internal/canvas"
	"github.com/jedwards1230/scrim/internal/oidc"
	"github.com/jedwards1230/scrim/internal/oidc/oidctest"
)

// ownedCanvas creates a canvas owned by owner with a served index.html, so a
// permitted GET /c/<id>/ returns 200 (not the 404 an empty canvas dir yields).
func ownedCanvas(t *testing.T, s *Server, id, owner string) {
	t.Helper()
	if _, err := canvas.Create(s.canvasesDir, s.metaDir, id, "", "", "", owner); err != nil {
		t.Fatalf("Create(%s): %v", id, err)
	}
	if err := os.WriteFile(filepath.Join(canvas.Dir(s.canvasesDir, id), "index.html"),
		[]byte("<!doctype html><title>"+id+"</title>hi"), 0o644); err != nil {
		t.Fatalf("write index for %s: %v", id, err)
	}
}

// sessionFor mints a real OIDC session cookie for the given identity by driving
// a full login against the fake IdP, so the hub gate verifies it exactly as it
// would a browser's.
func sessionFor(t *testing.T, auth *oidc.Authenticator, idp *oidctest.IdP, subject, email string, groups []string) *http.Cookie {
	t.Helper()
	idp.Subject = subject
	idp.Email = email
	idp.Groups = groups
	return idp.Login(t, auth, "/")
}

// listCanvasIDs does GET /api/canvases with the given cookie/query and returns
// the ids the hub reports visible to that caller.
func listCanvasIDs(t *testing.T, s *Server, cookie *http.Cookie, rawQuery string) []string {
	t.Helper()
	url := "/api/canvases"
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/canvases status = %d, want 200 (body: %q)", rec.Code, rec.Body.String())
	}
	var got []apiclient.CanvasResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding canvas list: %v", err)
	}
	ids := make([]string, 0, len(got))
	for _, c := range got {
		ids = append(ids, c.ID)
	}
	return ids
}

func canvasGET(t *testing.T, s *Server, id string, cookie *http.Cookie, rawQuery string) int {
	t.Helper()
	url := "/c/" + id + "/"
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Accept", "text/html")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	return rec.Code
}

// TestPrivateByDefaultUnauthenticated pins the private-by-default gate for a
// caller with no session: the API and SSE get a 401 they can act on, a browser
// navigation is redirected into the login flow.
func TestPrivateByDefaultUnauthenticated(t *testing.T) {
	s, _, _ := newOIDCHub(t)
	ownedCanvas(t, s, "secret", "alice@example.com")

	// Unauthenticated API read → 401 (non-browser).
	apiReq := httptest.NewRequest(http.MethodGet, "/api/canvases", nil)
	apiRec := httptest.NewRecorder()
	s.routes().ServeHTTP(apiRec, apiReq)
	if apiRec.Code != http.StatusUnauthorized {
		t.Errorf("unauth GET /api/canvases = %d, want 401", apiRec.Code)
	}

	// Unauthenticated SSE → 401.
	sseReq := httptest.NewRequest(http.MethodGet, "/c/secret/__events", nil)
	sseReq.Header.Set("Accept", "text/event-stream")
	sseRec := httptest.NewRecorder()
	s.routes().ServeHTTP(sseRec, sseReq)
	if sseRec.Code != http.StatusUnauthorized {
		t.Errorf("unauth SSE = %d, want 401", sseRec.Code)
	}

	// Unauthenticated browser navigation → 302 to login.
	if code := canvasGET(t, s, "secret", nil, ""); code != http.StatusFound {
		t.Errorf("unauth browser GET /c/secret/ = %d, want 302 login", code)
	}
}

// TestOwnerOnlyVisibility proves a canvas created by A is invisible to B until
// shared: A sees it (gallery + direct GET), B sees an empty gallery and a 404.
func TestOwnerOnlyVisibility(t *testing.T) {
	s, auth, idp := newOIDCHub(t)
	ownedCanvas(t, s, "alices", "alice@example.com")

	alice := sessionFor(t, auth, idp, "sub-alice", "alice@example.com", nil)
	bob := sessionFor(t, auth, idp, "sub-bob", "bob@example.com", nil)

	if ids := listCanvasIDs(t, s, alice, ""); len(ids) != 1 || ids[0] != "alices" {
		t.Errorf("alice's gallery = %v, want [alices]", ids)
	}
	if ids := listCanvasIDs(t, s, bob, ""); len(ids) != 0 {
		t.Errorf("bob's gallery = %v, want empty (not shared)", ids)
	}
	if code := canvasGET(t, s, "alices", alice, ""); code != http.StatusOK {
		t.Errorf("alice GET her canvas = %d, want 200", code)
	}
	// B is authenticated-but-not-permitted → 404 (never 403, never reveal it).
	if code := canvasGET(t, s, "alices", bob, ""); code != http.StatusNotFound {
		t.Errorf("bob GET alice's canvas = %d, want 404", code)
	}
}

// TestShareGrantsHonored exercises each grant kind end-to-end through the gate:
// user, group, everyone, and link (?k=).
func TestShareGrantsHonored(t *testing.T) {
	s, auth, idp := newOIDCHub(t)
	bob := sessionFor(t, auth, idp, "sub-bob", "bob@example.com", []string{"eng"})

	t.Run("user grant", func(t *testing.T) {
		ownedCanvas(t, s, "cu", "alice@example.com")
		if err := canvas.AddGrant(s.metaDir, "cu", canvas.Grant{Kind: canvas.GrantUser, Target: "bob@example.com"}); err != nil {
			t.Fatal(err)
		}
		if code := canvasGET(t, s, "cu", bob, ""); code != http.StatusOK {
			t.Errorf("bob GET user-granted canvas = %d, want 200", code)
		}
		if ids := listCanvasIDs(t, s, bob, ""); !contains(ids, "cu") {
			t.Errorf("bob's gallery = %v, want it to include cu", ids)
		}
	})

	t.Run("group grant", func(t *testing.T) {
		ownedCanvas(t, s, "cg", "alice@example.com")
		if err := canvas.AddGrant(s.metaDir, "cg", canvas.Grant{Kind: canvas.GrantGroup, Target: "eng"}); err != nil {
			t.Fatal(err)
		}
		if code := canvasGET(t, s, "cg", bob, ""); code != http.StatusOK {
			t.Errorf("bob (group eng) GET group-granted canvas = %d, want 200", code)
		}
	})

	t.Run("everyone grant", func(t *testing.T) {
		ownedCanvas(t, s, "ce", "alice@example.com")
		if err := canvas.AddGrant(s.metaDir, "ce", canvas.Grant{Kind: canvas.GrantEveryone}); err != nil {
			t.Fatal(err)
		}
		// A different authenticated principal (carol, no groups) still sees it.
		carol := sessionFor(t, auth, idp, "sub-carol", "carol@example.com", nil)
		if code := canvasGET(t, s, "ce", carol, ""); code != http.StatusOK {
			t.Errorf("carol GET everyone-granted canvas = %d, want 200", code)
		}
	})

	t.Run("link grant honored via ?k=", func(t *testing.T) {
		const secret = "share-link-secret-value"
		ownedCanvas(t, s, "cl", "alice@example.com")
		if err := canvas.AddGrant(s.metaDir, "cl", canvas.Grant{
			Kind:           canvas.GrantLink,
			LinkID:         "l1",
			LinkSecretHash: canvas.HashLinkSecret(secret),
		}); err != nil {
			t.Fatal(err)
		}
		// Anonymous with the correct secret → 200.
		if code := canvasGET(t, s, "cl", nil, "k="+secret); code != http.StatusOK {
			t.Errorf("anon GET link canvas with correct ?k= = %d, want 200", code)
		}
		// Anonymous with a wrong secret → 302 login (unauthenticated, no valid link).
		if code := canvasGET(t, s, "cl", nil, "k=wrong"); code != http.StatusFound {
			t.Errorf("anon GET link canvas with wrong ?k= = %d, want 302 login", code)
		}
		// Anonymous with no secret → 302 login.
		if code := canvasGET(t, s, "cl", nil, ""); code != http.StatusFound {
			t.Errorf("anon GET link canvas with no ?k= = %d, want 302 login", code)
		}
	})
}

// TestSpoofedActorHeaderIgnored proves the #51 trust rule holds already: an
// X-Scrim-Actor-* header on a request WITHOUT the admin push token is ignored,
// so it can never impersonate an owner or elevate a session.
func TestSpoofedActorHeaderIgnored(t *testing.T) {
	s, auth, idp := newOIDCHub(t)
	ownedCanvas(t, s, "alices", "alice@example.com")
	bob := sessionFor(t, auth, idp, "sub-bob", "bob@example.com", nil)

	// Anonymous caller spoofing alice via actor headers, no admin bearer → still
	// anonymous → 401 on the API (the header buys nothing).
	req := httptest.NewRequest(http.MethodGet, "/api/canvases", nil)
	req.Header.Set("X-Scrim-Actor-Email", "alice@example.com")
	req.Header.Set("X-Scrim-Actor-Id", "sub-alice")
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("spoofed-actor anon API = %d, want 401 (actor header ignored)", rec.Code)
	}

	// Bob spoofing alice via actor headers → still bob's claims, so alice's
	// canvas stays invisible to him.
	req2 := httptest.NewRequest(http.MethodGet, "/c/alices/", nil)
	req2.Header.Set("Accept", "text/html")
	req2.Header.Set("X-Scrim-Actor-Email", "alice@example.com")
	req2.AddCookie(bob)
	rec2 := httptest.NewRecorder()
	s.routes().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotFound {
		t.Errorf("bob spoofing alice via actor header = %d, want 404 (header ignored)", rec2.Code)
	}
}

// TestAdminPushTokenSeesEverything confirms the admin push token remains a
// visibility superuser: it lists and reads every canvas regardless of owner.
func TestAdminPushTokenSeesEverything(t *testing.T) {
	s, _, _ := newOIDCHub(t)
	ownedCanvas(t, s, "alices", "alice@example.com")
	ownedCanvas(t, s, "bobs", "bob@example.com")

	req := httptest.NewRequest(http.MethodGet, "/api/canvases", nil)
	req.Header.Set("Authorization", "Bearer test-push-token")
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin GET /api/canvases = %d, want 200", rec.Code)
	}
	var got []apiclient.CanvasResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("admin sees %d canvases, want 2 (superuser)", len(got))
	}
	// The response carries owner (additive field) so a PR2 UI can badge it.
	for _, c := range got {
		if c.Owner == "" {
			t.Errorf("canvas %s has empty owner in response, want it populated", c.ID)
		}
	}
}

// TestLogoutIsPostOnly pins the CSRF hardening: GET /auth/logout is 405, POST
// clears the session.
func TestLogoutIsPostOnly(t *testing.T) {
	s, _, _ := newOIDCHub(t)

	getReq := httptest.NewRequest(http.MethodGet, oidc.LogoutPath, nil)
	getRec := httptest.NewRecorder()
	s.routes().ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /auth/logout = %d, want 405 (POST-only, CSRF hardening)", getRec.Code)
	}

	postReq := httptest.NewRequest(http.MethodPost, oidc.LogoutPath, nil)
	postRec := httptest.NewRecorder()
	s.routes().ServeHTTP(postRec, postReq)
	if postRec.Code != http.StatusFound {
		t.Errorf("POST /auth/logout = %d, want 302", postRec.Code)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
