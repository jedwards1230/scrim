package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jedwards1230/scrim/internal/canvas"
	"github.com/jedwards1230/scrim/internal/usertoken"
)

// adminReq builds a machine-API request carrying the admin push token.
func adminReq(method, path string, body []byte) *http.Request {
	var r *http.Request
	if body != nil {
		r = httptest.NewRequest(method, path, bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.Header.Set("Authorization", "Bearer test-push-token")
	return r
}

func do(t *testing.T, s *Server, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, r)
	return rec
}

// TestGrantRoutesRoundTrip exercises the #52 hub grant routes directly: create a
// canvas, add user + link grants, list them (secret-free), then delete one.
func TestGrantRoutesRoundTrip(t *testing.T) {
	s, _ := newHubTestServer(t, []string{"127.0.0.0/8"}, "")

	if rec := do(t, s, adminReq(http.MethodPost, "/api/canvases", []byte(`{"id":"c1"}`))); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d", rec.Code)
	}

	// User grant.
	if rec := do(t, s, adminReq(http.MethodPost, "/api/canvases/c1/grants", []byte(`{"kind":"user","target":"bob@example.com"}`))); rec.Code != http.StatusCreated {
		t.Fatalf("add user grant = %d (body %q)", rec.Code, rec.Body.String())
	}

	// Link grant returns a secret ONCE.
	rec := do(t, s, adminReq(http.MethodPost, "/api/canvases/c1/grants", []byte(`{"kind":"link"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("add link grant = %d", rec.Code)
	}
	var linkResp struct {
		LinkID     string `json:"link_id"`
		LinkSecret string `json:"link_secret"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &linkResp); err != nil {
		t.Fatal(err)
	}
	if linkResp.LinkSecret == "" || linkResp.LinkID == "" {
		t.Fatalf("link grant response missing id/secret: %s", rec.Body.String())
	}

	// List grants: owner=admin, two grants, and NO secret/hash in the payload.
	listRec := do(t, s, adminReq(http.MethodGet, "/api/canvases/c1/grants", nil))
	if listRec.Code != http.StatusOK {
		t.Fatalf("list grants = %d", listRec.Code)
	}
	body := listRec.Body.String()
	if bytes.Contains(listRec.Body.Bytes(), []byte(linkResp.LinkSecret)) {
		t.Error("list grants leaked the raw link secret")
	}
	if bytes.Contains(listRec.Body.Bytes(), []byte("link_secret_hash")) || bytes.Contains(listRec.Body.Bytes(), []byte(canvas.HashLinkSecret(linkResp.LinkSecret))) {
		t.Error("list grants leaked the link secret hash")
	}
	var listResp struct {
		Owner  string `json:"owner"`
		Grants []struct {
			Kind   string `json:"kind"`
			LinkID string `json:"link_id"`
		} `json:"grants"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &listResp); err != nil {
		t.Fatal(err)
	}
	if listResp.Owner != "admin" {
		t.Errorf("owner = %q, want admin", listResp.Owner)
	}
	if len(listResp.Grants) != 2 {
		t.Fatalf("listed %d grants, want 2 (body %q)", len(listResp.Grants), body)
	}

	// Delete the link grant by its public id.
	if rec := do(t, s, adminReq(http.MethodDelete, "/api/canvases/c1/grants/"+linkResp.LinkID, nil)); rec.Code != http.StatusNoContent {
		t.Fatalf("delete link grant = %d", rec.Code)
	}
	// A second delete of the same ref is 404.
	if rec := do(t, s, adminReq(http.MethodDelete, "/api/canvases/c1/grants/"+linkResp.LinkID, nil)); rec.Code != http.StatusNotFound {
		t.Errorf("delete missing grant = %d, want 404", rec.Code)
	}
}

// TestGrantAllowanceEnforced proves a user token may only share to targets its
// allowance permits; admin is unrestricted.
func TestGrantAllowanceEnforced(t *testing.T) {
	s, _ := newHubTestServer(t, []string{"127.0.0.0/8"}, "")

	// Mint a user token for alice, allowed to share only to bob@example.com.
	raw, _, err := s.tokens.Mint("alice-laptop", "alice@example.com", nil, usertoken.Allowance{
		Emails: []string{"bob@example.com"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Alice's token creates her own canvas.
	createReq := httptest.NewRequest(http.MethodPost, "/api/canvases", bytes.NewReader([]byte(`{"id":"alices"}`)))
	createReq.Header.Set("Content-Type", "application/json")
	createReq.Header.Set("Authorization", "Bearer "+raw)
	if rec := do(t, s, createReq); rec.Code != http.StatusCreated {
		t.Fatalf("alice token create = %d", rec.Code)
	}

	grant := func(payload string) int {
		r := httptest.NewRequest(http.MethodPost, "/api/canvases/alices/grants", bytes.NewReader([]byte(payload)))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+raw)
		return do(t, s, r).Code
	}

	// In-allowance target → 201.
	if code := grant(`{"kind":"user","target":"bob@example.com"}`); code != http.StatusCreated {
		t.Errorf("in-allowance grant = %d, want 201", code)
	}
	// Out-of-allowance target → 403.
	if code := grant(`{"kind":"user","target":"eve@example.com"}`); code != http.StatusForbidden {
		t.Errorf("out-of-allowance grant = %d, want 403", code)
	}
	// Out-of-allowance kind (everyone not permitted) → 403.
	if code := grant(`{"kind":"everyone"}`); code != http.StatusForbidden {
		t.Errorf("out-of-allowance everyone grant = %d, want 403", code)
	}
	// Admin is unrestricted → 201 even for everyone.
	if rec := do(t, s, adminReq(http.MethodPost, "/api/canvases/alices/grants", []byte(`{"kind":"everyone"}`))); rec.Code != http.StatusCreated {
		t.Errorf("admin everyone grant = %d, want 201", rec.Code)
	}
}
