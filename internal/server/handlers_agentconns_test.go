package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/jedwards1230/scrim/internal/agentconn"
	"github.com/jedwards1230/scrim/internal/usertoken"
)

// agentRead performs an ordinary authenticated read AS a forwarded agent: the
// admin push-token bearer plus the verified X-Scrim-Actor-* headers scrim mcp
// attaches, including the OAuth client id and the token's iat. It returns the
// status code -- 200 while the connection is live, 403 once it is revoked.
func agentRead(t *testing.T, s *Server, subject, clientID string, issuedAt time.Time) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/canvases", nil)
	req.Header.Set("Authorization", "Bearer test-push-token")
	req.Header.Set("X-Scrim-Actor-Id", subject)
	req.Header.Set("X-Scrim-Actor-Email", subject+"@example.com")
	if clientID != "" {
		req.Header.Set("X-Scrim-Actor-Client-Id", clientID)
	}
	if !issuedAt.IsZero() {
		req.Header.Set("X-Scrim-Actor-Token-Issued-At", strconv.FormatInt(issuedAt.Unix(), 10))
	}
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	return rec.Code
}

// agentReadRawIssuedAt is agentRead with the issued-at header set to an
// arbitrary raw string, so a malformed value can be exercised the way a
// misbehaving or hostile forwarder would send it.
func agentReadRawIssuedAt(t *testing.T, s *Server, subject, clientID, issuedAt string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/canvases", nil)
	req.Header.Set("Authorization", "Bearer test-push-token")
	req.Header.Set("X-Scrim-Actor-Id", subject)
	req.Header.Set("X-Scrim-Actor-Email", subject+"@example.com")
	req.Header.Set("X-Scrim-Actor-Client-Id", clientID)
	req.Header.Set("X-Scrim-Actor-Token-Issued-At", issuedAt)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	return rec.Code
}

// listAgentConns does GET /api/agent-connections with the given cookie and
// returns the decoded response plus the status code.
func listAgentConns(t *testing.T, s *Server, cookie *http.Cookie) ([]agentConnResponse, int) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/agent-connections", nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return nil, rec.Code
	}
	var out []agentConnResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding GET /api/agent-connections: %v (body %q)", err, rec.Body.String())
	}
	return out, rec.Code
}

// deleteAgentConn does DELETE /api/agent-connections/{id} with the given cookie.
func deleteAgentConn(t *testing.T, s *Server, cookie *http.Cookie, id string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/api/agent-connections/"+id, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	return rec.Code
}

// TestAgentConnectionListedForItsPrincipal covers the gap this feature closes:
// an OAuth-authenticated MCP client reaching the hub as a forwarded actor now
// shows up on the page whose whole job is "what has access" -- named by its
// OAuth client id, on its principal's list and nobody else's.
func TestAgentConnectionListedForItsPrincipal(t *testing.T) {
	s, auth, idp := newOIDCHub(t)
	alice := sessionFor(t, auth, idp, "sub-alice", "alice@example.com", nil)
	bob := sessionFor(t, auth, idp, "sub-bob", "bob@example.com", nil)

	if code := agentRead(t, s, "sub-alice", "claude-desktop", time.Now()); code != http.StatusOK {
		t.Fatalf("forwarded-actor read = %d, want 200", code)
	}

	got, code := listAgentConns(t, s, alice)
	if code != http.StatusOK {
		t.Fatalf("GET /api/agent-connections = %d, want 200", code)
	}
	if len(got) != 1 {
		t.Fatalf("alice sees %d agent connections, want the one that just called", len(got))
	}
	if got[0].ClientID != "claude-desktop" {
		t.Errorf("client id = %q, want claude-desktop (the token's azp, forwarded by scrim mcp)", got[0].ClientID)
	}
	if got[0].FirstSeen.IsZero() || got[0].LastSeen.IsZero() {
		t.Errorf("agent connection timestamps = %+v, want both populated", got[0])
	}
	if got[0].RevokedAt != nil {
		t.Error("a live connection reports a revoked_at, want none")
	}

	if bobs, _ := listAgentConns(t, s, bob); len(bobs) != 0 {
		t.Errorf("bob sees %d of alice's agent connections, want none", len(bobs))
	}
}

// TestRevokedAgentConnectionIsBlocked is the property the whole feature exists
// for: Revoke actually bites, on the agent's very next request, and cuts off
// only that connection.
func TestRevokedAgentConnectionIsBlocked(t *testing.T) {
	s, auth, idp := newOIDCHub(t)
	alice := sessionFor(t, auth, idp, "sub-alice", "alice@example.com", nil)

	issued := time.Now().Add(-time.Hour)
	if code := agentRead(t, s, "sub-alice", "claude-desktop", issued); code != http.StatusOK {
		t.Fatalf("read before revocation = %d, want 200", code)
	}
	if code := agentRead(t, s, "sub-alice", "other-agent", issued); code != http.StatusOK {
		t.Fatalf("second agent read = %d, want 200", code)
	}

	got, _ := listAgentConns(t, s, alice)
	var id string
	for _, conn := range got {
		if conn.ClientID == "claude-desktop" {
			id = conn.ID
		}
	}
	if id == "" {
		t.Fatalf("no claude-desktop connection to revoke (got %+v)", got)
	}
	if code := deleteAgentConn(t, s, alice, id); code != http.StatusNoContent {
		t.Fatalf("DELETE /api/agent-connections/{id} = %d, want 204", code)
	}

	if code := agentRead(t, s, "sub-alice", "claude-desktop", issued); code != http.StatusForbidden {
		t.Errorf("read from a revoked agent = %d, want 403 on its very next request", code)
	}
	if code := agentRead(t, s, "sub-alice", "other-agent", issued); code != http.StatusOK {
		t.Errorf("alice's other agent = %d, want 200 -- the revocation is per connection", code)
	}

	// The revoked connection stays listed, marked, so the page shows history
	// rather than pretending the agent was never there.
	got, _ = listAgentConns(t, s, alice)
	for _, conn := range got {
		if conn.ID == id && conn.RevokedAt == nil {
			t.Error("the revoked connection is listed as live, want revoked_at set")
		}
	}
}

// TestReauthorizedAgentIsAdmitted pins the `iat` rule end-to-end: revoking
// blocks the client at scrim, it does NOT delete the grant at the IdP, and
// authorizing again -- which mints a token issued after the revocation -- gets
// the agent back in. The devices page promises exactly this and no more.
func TestReauthorizedAgentIsAdmitted(t *testing.T) {
	s, auth, idp := newOIDCHub(t)
	alice := sessionFor(t, auth, idp, "sub-alice", "alice@example.com", nil)

	old := time.Now().Add(-time.Hour)
	if code := agentRead(t, s, "sub-alice", "claude-desktop", old); code != http.StatusOK {
		t.Fatalf("first read = %d, want 200", code)
	}
	got, _ := listAgentConns(t, s, alice)
	if code := deleteAgentConn(t, s, alice, got[0].ID); code != http.StatusNoContent {
		t.Fatalf("revoke = %d, want 204", code)
	}
	if code := agentRead(t, s, "sub-alice", "claude-desktop", old); code != http.StatusForbidden {
		t.Fatalf("the old token = %d, want 403", code)
	}

	// A token minted after the revocation: re-authorization.
	if code := agentRead(t, s, "sub-alice", "claude-desktop", time.Now().Add(time.Minute)); code != http.StatusOK {
		t.Errorf("a token issued after the revocation = %d, want 200 (re-authorizing must work)", code)
	}
	// ...and the page stops calling a working connection revoked.
	got, _ = listAgentConns(t, s, alice)
	if len(got) != 1 || got[0].RevokedAt != nil {
		t.Errorf("after re-authorization the connection reads %+v, want it listed as live", got)
	}
}

// TestAgentRevocationDoesNotTouchTheAdminPushToken is the recovery invariant: a
// BARE admin push token -- no actor headers, the credential CI pushes and
// `scrim push` use -- is unaffected by any revocation in the registry, and by a
// registry that cannot be read at all.
func TestAgentRevocationDoesNotTouchTheAdminPushToken(t *testing.T) {
	s, auth, idp := newOIDCHub(t)
	alice := sessionFor(t, auth, idp, "sub-alice", "alice@example.com", nil)

	if code := agentRead(t, s, "sub-alice", "claude-desktop", time.Now().Add(-time.Hour)); code != http.StatusOK {
		t.Fatalf("agent read = %d, want 200", code)
	}
	got, _ := listAgentConns(t, s, alice)
	if code := deleteAgentConn(t, s, alice, got[0].ID); code != http.StatusNoContent {
		t.Fatalf("revoke = %d, want 204", code)
	}

	adminRead := func() int {
		req := httptest.NewRequest(http.MethodGet, "/api/canvases", nil)
		req.Header.Set("Authorization", "Bearer test-push-token")
		rec := httptest.NewRecorder()
		s.routes().ServeHTTP(rec, req)
		return rec.Code
	}
	adminPush := func() int {
		req := httptest.NewRequest(http.MethodPost, "/api/push/recovery", http.NoBody)
		req.Header.Set("Authorization", "Bearer test-push-token")
		rec := httptest.NewRecorder()
		s.routes().ServeHTTP(rec, req)
		return rec.Code
	}

	if code := adminRead(); code != http.StatusOK {
		t.Errorf("bare admin read after a revocation = %d, want 200 -- it is the machine/recovery credential", code)
	}
	if code := adminPush(); code == http.StatusUnauthorized || code == http.StatusForbidden {
		t.Errorf("bare admin push after a revocation = %d, want it ungated", code)
	}

	// Now break the registry entirely, as a restart against a corrupt file
	// would: the forwarded-actor plane fails closed, the bare admin token does
	// not consult the store at all and keeps working.
	if err := os.WriteFile(filepath.Join(s.metaDir, "agent-connections.json"), []byte("{ not the registry"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.agents = agentconn.New(s.metaDir)

	if code := agentRead(t, s, "sub-alice", "other-agent", time.Now()); code == http.StatusOK {
		t.Error("a forwarded agent was admitted against a corrupt registry, want it refused (fail closed)")
	}
	if code := adminRead(); code != http.StatusOK {
		t.Errorf("bare admin read against a corrupt registry = %d, want 200", code)
	}
	if code := adminPush(); code == http.StatusUnauthorized || code == http.StatusForbidden {
		t.Errorf("bare admin push against a corrupt registry = %d, want it ungated", code)
	}
	if _, code := listAgentConns(t, s, alice); code == http.StatusOK {
		t.Errorf("GET /api/agent-connections against a corrupt registry = %d, want a failure rather than an empty list", code)
	}
}

// TestRevokeAnotherPrincipalsAgentConnIs404 mirrors the session and token
// endpoints exactly: someone else's connection is a 404, never a 403.
func TestRevokeAnotherPrincipalsAgentConnIs404(t *testing.T) {
	s, auth, idp := newOIDCHub(t)
	alice := sessionFor(t, auth, idp, "sub-alice", "alice@example.com", nil)
	bob := sessionFor(t, auth, idp, "sub-bob", "bob@example.com", nil)

	issued := time.Now().Add(-time.Hour)
	if code := agentRead(t, s, "sub-bob", "claude-desktop", issued); code != http.StatusOK {
		t.Fatalf("bob's agent read = %d, want 200", code)
	}
	bobs, _ := listAgentConns(t, s, bob)
	if len(bobs) != 1 {
		t.Fatalf("bob has %d agent connections, want 1", len(bobs))
	}

	if code := deleteAgentConn(t, s, alice, bobs[0].ID); code != http.StatusNotFound {
		t.Errorf("DELETE of another principal's agent connection = %d, want 404 (not 403 -- existence must not leak)", code)
	}
	if code := deleteAgentConn(t, s, alice, "no-such-connection"); code != http.StatusNotFound {
		t.Errorf("DELETE of an unknown agent connection = %d, want 404", code)
	}
	// And bob's agent still works.
	if code := agentRead(t, s, "sub-bob", "claude-desktop", issued); code != http.StatusOK {
		t.Errorf("bob's agent was cut off by alice's DELETE (read = %d), want it untouched", code)
	}
}

// TestAgentConnEndpointsAreSessionOnly pins the plane, exactly as
// /api/sessions* does: a user token is 403, the machine plane (an agent
// revoking its own revocation, or someone else's) is 403, an anonymous caller
// is 401, and the admin push token -- having no agents of its own -- lists
// nothing and gets a 404.
func TestAgentConnEndpointsAreSessionOnly(t *testing.T) {
	s, auth, idp := newOIDCHub(t)
	alice := sessionFor(t, auth, idp, "sub-alice", "alice@example.com", nil)
	if code := agentRead(t, s, "sub-alice", "claude-desktop", time.Now()); code != http.StatusOK {
		t.Fatalf("agent read = %d, want 200", code)
	}
	alices, _ := listAgentConns(t, s, alice)
	if len(alices) != 1 {
		t.Fatalf("alice has %d agent connections, want 1", len(alices))
	}

	raw, _, err := s.tokens.Mint("cli", "alice@example.com", nil, usertoken.Allowance{})
	if err != nil {
		t.Fatalf("minting a user token: %v", err)
	}
	req := httptest.NewRequest(http.MethodDelete, "/api/agent-connections/"+alices[0].ID, nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("DELETE /api/agent-connections with a user-token bearer = %d, want 403", rec.Code)
	}

	// The forwarded agent itself may not manage connections.
	agentDel := httptest.NewRequest(http.MethodDelete, "/api/agent-connections/"+alices[0].ID, nil)
	agentDel.Header.Set("Authorization", "Bearer test-push-token")
	agentDel.Header.Set("X-Scrim-Actor-Id", "sub-alice")
	agentDel.Header.Set("X-Scrim-Actor-Email", "alice@example.com")
	agentDel.Header.Set("X-Scrim-Actor-Client-Id", "claude-desktop")
	agentRec := httptest.NewRecorder()
	s.routes().ServeHTTP(agentRec, agentDel)
	if agentRec.Code != http.StatusForbidden {
		t.Errorf("DELETE /api/agent-connections as the forwarded agent = %d, want 403", agentRec.Code)
	}

	// The admin push token is the machine plane: no subject, so no agents.
	adminList := httptest.NewRequest(http.MethodGet, "/api/agent-connections", nil)
	adminList.Header.Set("Authorization", "Bearer test-push-token")
	adminListRec := httptest.NewRecorder()
	s.routes().ServeHTTP(adminListRec, adminList)
	if adminListRec.Code != http.StatusOK {
		t.Fatalf("GET /api/agent-connections with the admin token = %d, want 200", adminListRec.Code)
	}
	var adminGot []agentConnResponse
	if err := json.Unmarshal(adminListRec.Body.Bytes(), &adminGot); err != nil {
		t.Fatal(err)
	}
	if len(adminGot) != 0 {
		t.Errorf("admin token listed %d agent connections, want none -- it has none of its own", len(adminGot))
	}

	adminDel := httptest.NewRequest(http.MethodDelete, "/api/agent-connections/"+alices[0].ID, nil)
	adminDel.Header.Set("Authorization", "Bearer test-push-token")
	adminDelRec := httptest.NewRecorder()
	s.routes().ServeHTTP(adminDelRec, adminDel)
	if adminDelRec.Code != http.StatusNotFound {
		t.Errorf("DELETE of a user's agent connection with the admin token = %d, want 404", adminDelRec.Code)
	}

	anon := httptest.NewRequest(http.MethodDelete, "/api/agent-connections/"+alices[0].ID, nil)
	anonRec := httptest.NewRecorder()
	s.routes().ServeHTTP(anonRec, anon)
	if anonRec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous DELETE /api/agent-connections = %d, want 401", anonRec.Code)
	}

	// After all of that, alice's agent is still admitted.
	if code := agentRead(t, s, "sub-alice", "claude-desktop", time.Now()); code != http.StatusOK {
		t.Errorf("alice's agent = %d after the rejected management attempts, want 200", code)
	}
}

// TestMalformedIssuedAtCannotLiftARevocation pins the parser's fail-closed
// direction: a forwarded issued-at that is garbage, negative, or a far-future
// bluff must not buy a revoked client its access back. Anything unparseable
// reads as "no proof of age", which keeps the revocation in force -- the only
// value that lifts it is a real timestamp after the revocation, and a caller
// that can mint one has re-authorized for real.
func TestMalformedIssuedAtCannotLiftARevocation(t *testing.T) {
	s, auth, idp := newOIDCHub(t)
	alice := sessionFor(t, auth, idp, "sub-alice", "alice@example.com", nil)

	if code := agentRead(t, s, "sub-alice", "claude-desktop", time.Now().Add(-time.Hour)); code != http.StatusOK {
		t.Fatalf("first read = %d, want 200", code)
	}
	got, _ := listAgentConns(t, s, alice)
	if code := deleteAgentConn(t, s, alice, got[0].ID); code != http.StatusNoContent {
		t.Fatalf("revoke = %d, want 204", code)
	}

	for _, raw := range []string{"not-a-number", "", "-1", "9999999999999999999999", "  "} {
		if code := agentReadRawIssuedAt(t, s, "sub-alice", "claude-desktop", raw); code != http.StatusForbidden {
			t.Errorf("revoked agent with issued-at %q = %d, want 403", raw, code)
		}
	}
}

// TestAgentRevocationHoldsAgainstTheHMACPlane covers the credential that
// carries no JWT: a gateway-forwarded (HMAC) actor presents no client id and no
// iat, so it keys on the principal's empty-client row -- and a revocation of
// THAT row blocks it, since nothing it presents can prove it postdates the
// revocation. A revocation must never be silently unenforceable.
func TestAgentRevocationHoldsAgainstTheHMACPlane(t *testing.T) {
	s, auth, idp := newOIDCHub(t)
	alice := sessionFor(t, auth, idp, "sub-alice", "alice@example.com", nil)

	if code := agentRead(t, s, "sub-alice", "", time.Time{}); code != http.StatusOK {
		t.Fatalf("HMAC-plane actor read = %d, want 200", code)
	}
	got, _ := listAgentConns(t, s, alice)
	if len(got) != 1 || got[0].ClientID != "" {
		t.Fatalf("connections = %+v, want one unnamed (client-id-less) connection", got)
	}
	if code := deleteAgentConn(t, s, alice, got[0].ID); code != http.StatusNoContent {
		t.Fatalf("revoke = %d, want 204", code)
	}

	if code := agentRead(t, s, "sub-alice", "", time.Time{}); code != http.StatusForbidden {
		t.Errorf("HMAC-plane actor after revocation = %d, want 403", code)
	}
	// Not even a freshly-dated request lifts it: with no client id there is
	// nothing tying that timestamp to this connection.
	if code := agentRead(t, s, "sub-alice", "", time.Now().Add(time.Hour)); code != http.StatusForbidden {
		t.Errorf("HMAC-plane actor with a fresh iat = %d, want 403 (unprovable, so fail closed)", code)
	}
}
