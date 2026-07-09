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

// createWithActor POSTs a canvas as a CF-forwarded actor: the admin push token
// bearer plus X-Scrim-Actor-* headers. Returns the response recorder.
func createWithActor(t *testing.T, s *Server, id, actorEmail string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/canvases", bytes.NewReader([]byte(`{"id":"`+id+`"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-push-token")
	req.Header.Set("X-Scrim-Actor-Email", actorEmail)
	req.Header.Set("X-Scrim-Actor-Id", "sub-"+actorEmail)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	return rec
}

// TestCFActorOwnsCreatedCanvas proves #51 attribution: a canvas created with the
// admin push token AND verified X-Scrim-Actor-* headers is owned by the actor,
// not by admin.
func TestCFActorOwnsCreatedCanvas(t *testing.T) {
	s, _ := newHubTestServer(t, []string{"127.0.0.0/8"}, "")

	rec := createWithActor(t, s, "actor-canvas", "alice@example.com")
	if rec.Code != http.StatusCreated {
		t.Fatalf("CF-actor create = %d, want 201 (body: %q)", rec.Code, rec.Body.String())
	}
	var got apiclient.CanvasResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Owner != "alice@example.com" {
		t.Errorf("owner in response = %q, want alice@example.com", got.Owner)
	}
	owner, _, err := canvas.GetOwnerGrants(s.metaDir, "actor-canvas")
	if err != nil {
		t.Fatal(err)
	}
	if owner != "alice@example.com" {
		t.Errorf("stored owner = %q, want alice@example.com (not admin)", owner)
	}
}

// TestCFActorCannotWriteOthersCanvas proves the CF actor acts AS itself, not as
// the admin superuser: it cannot write a canvas owned by another principal.
func TestCFActorCannotWriteOthersCanvas(t *testing.T) {
	s, _ := newHubTestServer(t, []string{"127.0.0.0/8"}, "")

	// Alice (via CF actor) creates and owns a canvas.
	if rec := createWithActor(t, s, "alices", "alice@example.com"); rec.Code != http.StatusCreated {
		t.Fatalf("alice create = %d, want 201", rec.Code)
	}

	// Bob (via CF actor) tries to write it → 403 (not his).
	req := httptest.NewRequest(http.MethodPut, "/api/canvases/alices/files/x.html", bytes.NewReader([]byte("nope")))
	req.Header.Set("Authorization", "Bearer test-push-token")
	req.Header.Set("X-Scrim-Actor-Email", "bob@example.com")
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("bob CF-actor write to alice's canvas = %d, want 403", rec.Code)
	}
}

// TestCFActorReadVisibility proves a CF actor's reads are private-by-default on
// an OIDC hub: an actor sees its own canvas in the gallery but not another
// actor's.
func TestCFActorReadVisibility(t *testing.T) {
	s, _, _ := newOIDCHub(t)

	if rec := createWithActor(t, s, "alices", "alice@example.com"); rec.Code != http.StatusCreated {
		t.Fatalf("alice create = %d, want 201", rec.Code)
	}

	listAs := func(actorEmail string) []string {
		req := httptest.NewRequest(http.MethodGet, "/api/canvases", nil)
		req.Header.Set("Authorization", "Bearer test-push-token")
		req.Header.Set("X-Scrim-Actor-Email", actorEmail)
		rec := httptest.NewRecorder()
		s.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("list as %s = %d, want 200", actorEmail, rec.Code)
		}
		var got []apiclient.CanvasResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		ids := make([]string, 0, len(got))
		for _, c := range got {
			ids = append(ids, c.ID)
		}
		return ids
	}

	if ids := listAs("alice@example.com"); len(ids) != 1 || ids[0] != "alices" {
		t.Errorf("alice actor gallery = %v, want [alices]", ids)
	}
	if ids := listAs("bob@example.com"); len(ids) != 0 {
		t.Errorf("bob actor gallery = %v, want empty (private by default)", ids)
	}
}
