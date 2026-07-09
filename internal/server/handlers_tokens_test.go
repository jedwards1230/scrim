package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jedwards1230/scrim/internal/apiclient"
	"github.com/jedwards1230/scrim/internal/canvas"
)

// mintTokenViaSession mints a token for the given session principal through the
// HTTP API (POST /api/tokens) and returns the raw secret.
func mintTokenViaSession(t *testing.T, s *Server, cookie *http.Cookie, body string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/tokens", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/tokens = %d, want 201 (body: %q)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Token string `json:"token"`
		Meta  struct {
			OwnerEmail string `json:"owner_email"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Token == "" {
		t.Fatal("mint returned an empty raw token")
	}
	return resp.Token
}

// createCanvasWithToken creates a canvas via POST /api/canvases using a user
// bearer token, returning the response status.
func createCanvasWithToken(t *testing.T, s *Server, rawToken, id string) int {
	t.Helper()
	body := `{"id":"` + id + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/canvases", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+rawToken)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	return rec.Code
}

// TestUserTokenActsAsOwner proves a canvas created with a user token is owned
// by the token's owner, and that a second principal's token can neither see nor
// write it.
func TestUserTokenActsAsOwner(t *testing.T) {
	s, auth, idp := newOIDCHub(t)
	alice := sessionFor(t, auth, idp, "sub-alice", "alice@example.com", nil)
	bob := sessionFor(t, auth, idp, "sub-bob", "bob@example.com", nil)

	aliceToken := mintTokenViaSession(t, s, alice, `{"name":"alice-laptop"}`)
	bobToken := mintTokenViaSession(t, s, bob, `{"name":"bob-laptop"}`)

	// Alice's token creates a canvas -> owned by alice.
	if code := createCanvasWithToken(t, s, aliceToken, "acanvas"); code != http.StatusCreated {
		t.Fatalf("create with alice's token = %d, want 201", code)
	}
	owner, _, err := canvas.GetOwnerGrants(s.metaDir, "acanvas")
	if err != nil {
		t.Fatal(err)
	}
	if owner != "alice@example.com" {
		t.Errorf("canvas owner = %q, want alice@example.com", owner)
	}

	// Bob's token cannot see it in the list.
	req := httptest.NewRequest(http.MethodGet, "/api/canvases", nil)
	req.Header.Set("Authorization", "Bearer "+bobToken)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("bob token list = %d, want 200", rec.Code)
	}
	var list []apiclient.CanvasResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Errorf("bob's token sees %d canvases, want 0 (alice's is private)", len(list))
	}

	// Bob's token cannot WRITE alice's canvas (PUT a file) -> 403.
	putReq := httptest.NewRequest(http.MethodPut, "/api/canvases/acanvas/files/x.html", bytes.NewReader([]byte("hi")))
	putReq.Header.Set("Authorization", "Bearer "+bobToken)
	putRec := httptest.NewRecorder()
	s.routes().ServeHTTP(putRec, putReq)
	if putRec.Code != http.StatusForbidden {
		t.Errorf("bob token PUT to alice's canvas = %d, want 403", putRec.Code)
	}

	// Alice's token CAN write her own canvas.
	putReq2 := httptest.NewRequest(http.MethodPut, "/api/canvases/acanvas/files/x.html", bytes.NewReader([]byte("hi")))
	putReq2.Header.Set("Authorization", "Bearer "+aliceToken)
	putRec2 := httptest.NewRecorder()
	s.routes().ServeHTTP(putRec2, putReq2)
	if putRec2.Code != http.StatusNoContent {
		t.Errorf("alice token PUT to her canvas = %d, want 204", putRec2.Code)
	}
}

// TestUserTokenAutoShareAppliedOnCreate proves a token minted with an
// auto_share policy applies its grants when the token creates a canvas -- so a
// grantee immediately sees it.
func TestUserTokenAutoShareAppliedOnCreate(t *testing.T) {
	s, auth, idp := newOIDCHub(t)
	alice := sessionFor(t, auth, idp, "sub-alice", "alice@example.com", nil)
	// A token that auto-shares everything it creates with bob.
	aliceToken := mintTokenViaSession(t, s, alice,
		`{"name":"agent","auto_share":[{"kind":"user","target":"bob@example.com"}]}`)

	if code := createCanvasWithToken(t, s, aliceToken, "shared"); code != http.StatusCreated {
		t.Fatalf("create = %d, want 201", code)
	}

	_, grants, err := canvas.GetOwnerGrants(s.metaDir, "shared")
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 || grants[0].Kind != canvas.GrantUser || grants[0].Target != "bob@example.com" {
		t.Fatalf("auto-share grants = %+v, want a single bob user grant", grants)
	}
	if grants[0].CreatedBy != "alice@example.com" {
		t.Errorf("grant CreatedBy = %q, want alice@example.com", grants[0].CreatedBy)
	}

	// Bob (session) now sees the auto-shared canvas.
	bob := sessionFor(t, auth, idp, "sub-bob", "bob@example.com", nil)
	if ids := listCanvasIDs(t, s, bob, ""); !contains(ids, "shared") {
		t.Errorf("bob's gallery = %v, want it to include the auto-shared canvas", ids)
	}
}

// TestTokenMintingRestrictedToSessions proves the token-management endpoints
// reject a user-token principal (no privilege escalation) and an anonymous
// caller, while a session succeeds.
func TestTokenMintingRestrictedToSessions(t *testing.T) {
	s, auth, idp := newOIDCHub(t)
	alice := sessionFor(t, auth, idp, "sub-alice", "alice@example.com", nil)
	aliceToken := mintTokenViaSession(t, s, alice, `{"name":"laptop"}`)

	// A user-token principal trying to mint another token -> 403 (no escalation).
	req := httptest.NewRequest(http.MethodPost, "/api/tokens", bytes.NewReader([]byte(`{"name":"escalate"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+aliceToken)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("user-token mint = %d, want 403 (no escalation)", rec.Code)
	}

	// Anonymous mint -> 401.
	anonReq := httptest.NewRequest(http.MethodPost, "/api/tokens", bytes.NewReader([]byte(`{"name":"x"}`)))
	anonReq.Header.Set("Content-Type", "application/json")
	anonRec := httptest.NewRecorder()
	s.routes().ServeHTTP(anonRec, anonReq)
	if anonRec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous mint = %d, want 401", anonRec.Code)
	}
}

// TestTokenListAndRevoke proves a session lists and revokes its own tokens, and
// that a revoked token stops authorizing.
func TestTokenListAndRevoke(t *testing.T) {
	s, auth, idp := newOIDCHub(t)
	alice := sessionFor(t, auth, idp, "sub-alice", "alice@example.com", nil)
	raw := mintTokenViaSession(t, s, alice, `{"name":"laptop"}`)

	// List returns the token, with no hash/secret.
	listReq := httptest.NewRequest(http.MethodGet, "/api/tokens", nil)
	listReq.AddCookie(alice)
	listRec := httptest.NewRecorder()
	s.routes().ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("GET /api/tokens = %d, want 200", listRec.Code)
	}
	if bytes.Contains(listRec.Body.Bytes(), []byte("hash")) || bytes.Contains(listRec.Body.Bytes(), []byte(raw)) {
		t.Error("token list leaked a hash or raw secret")
	}
	var listed []tokenResponse
	if err := json.Unmarshal(listRec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("listed %d tokens, want 1", len(listed))
	}
	id := listed[0].ID

	// The token authorizes a create before revocation.
	if code := createCanvasWithToken(t, s, raw, "before"); code != http.StatusCreated {
		t.Fatalf("create before revoke = %d, want 201", code)
	}

	// Revoke it.
	delReq := httptest.NewRequest(http.MethodDelete, "/api/tokens/"+id, nil)
	delReq.AddCookie(alice)
	delRec := httptest.NewRecorder()
	s.routes().ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusNoContent {
		t.Fatalf("DELETE /api/tokens/%s = %d, want 204", id, delRec.Code)
	}

	// After revocation the token no longer authorizes a write -> 401 (not a
	// valid credential anymore, and no session on the request).
	if code := createCanvasWithToken(t, s, raw, "after"); code != http.StatusUnauthorized {
		t.Errorf("create after revoke = %d, want 401 (token revoked)", code)
	}
}

// TestAdminPushTokenStillWrites is the no-regression guard: the global push
// token remains a full machine credential (writes any canvas), now as the
// admin/bootstrap credential.
func TestAdminPushTokenStillWrites(t *testing.T) {
	s, _, _ := newOIDCHub(t)

	// Admin creates a canvas -> owned by "admin".
	req := httptest.NewRequest(http.MethodPost, "/api/canvases", bytes.NewReader([]byte(`{"id":"legacy"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-push-token")
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("admin create = %d, want 201", rec.Code)
	}
	owner, _, err := canvas.GetOwnerGrants(s.metaDir, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if owner != "admin" {
		t.Errorf("admin-created canvas owner = %q, want admin", owner)
	}

	// Admin can write any canvas (superuser) -- including one it doesn't "own".
	if err := canvas.SetOwner(s.metaDir, "legacy", "someone@else.com"); err != nil {
		t.Fatal(err)
	}
	putReq := httptest.NewRequest(http.MethodPut, "/api/canvases/legacy/files/x.html", bytes.NewReader([]byte("hi")))
	putReq.Header.Set("Authorization", "Bearer test-push-token")
	putRec := httptest.NewRecorder()
	s.routes().ServeHTTP(putRec, putReq)
	if putRec.Code != http.StatusNoContent {
		t.Errorf("admin PUT to another's canvas = %d, want 204 (superuser)", putRec.Code)
	}
}
