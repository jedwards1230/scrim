package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jedwards1230/scrim/internal/principal"
)

// getPrincipals does GET /api/principals?q=<q> against the hub and returns the
// decoded suggestions plus the HTTP status.
func getPrincipals(t *testing.T, s *Server, q string) ([]principalSuggestion, int) {
	t.Helper()
	url := "/api/principals"
	if q != "" {
		url += "?q=" + q
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.RemoteAddr = "127.0.0.1:12345" // pass the non-OIDC hub's loopback CIDR gate
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return nil, rec.Code
	}
	var got []principalSuggestion
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding principals: %v (body %q)", err, rec.Body.String())
	}
	return got, rec.Code
}

func TestHandlePrincipalsFilters(t *testing.T) {
	// A non-OIDC hub allowing loopback: the read gate lets a 127.0.0.1 request
	// through, so the handler itself is what's under test.
	s, _ := newHubTestServer(t, []string{"127.0.0.0/8"}, "")
	seed := []struct {
		email, name string
		groups      []string
	}{
		{"alice@example.com", "Alice Anderson", []string{"eng"}},
		{"bob@example.com", "Bob Barker", nil},
		{"carol@example.com", "Carol", []string{"design"}},
	}
	for _, p := range seed {
		if err := s.principals.Observe(p.email, p.name, p.groups, principal.SourceLogin); err != nil {
			t.Fatalf("Observe(%s): %v", p.email, err)
		}
	}

	t.Run("empty q returns all, email-sorted", func(t *testing.T) {
		got, code := getPrincipals(t, s, "")
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
		if len(got) != 3 {
			t.Fatalf("got %d principals, want 3", len(got))
		}
		if got[0].Email != "alice@example.com" {
			t.Errorf("first = %q, want alice@example.com (email-sorted)", got[0].Email)
		}
		if got[0].DisplayName != "Alice Anderson" || len(got[0].GroupsSeen) != 1 {
			t.Errorf("alice suggestion = %+v, want name + one group", got[0])
		}
	})

	t.Run("prefix on email is case-insensitive", func(t *testing.T) {
		got, _ := getPrincipals(t, s, "BOB")
		if len(got) != 1 || got[0].Email != "bob@example.com" {
			t.Errorf("q=BOB got %+v, want just bob@example.com", got)
		}
	})

	t.Run("prefix matches display name too", func(t *testing.T) {
		got, _ := getPrincipals(t, s, "Carol")
		if len(got) != 1 || got[0].Email != "carol@example.com" {
			t.Errorf("q=Carol got %+v, want carol@example.com (display-name match)", got)
		}
	})

	t.Run("non-matching prefix yields an empty array, not null", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/principals?q=zzz", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		rec := httptest.NewRecorder()
		s.routes().ServeHTTP(rec, req)
		if body := rec.Body.String(); body != "[]\n" {
			t.Errorf("q=zzz body = %q, want %q (empty JSON array)", body, "[]\n")
		}
	})
}

func TestHandlePrincipalsCaps(t *testing.T) {
	s, _ := newHubTestServer(t, []string{"127.0.0.0/8"}, "")
	// Seed more than the cap; every email shares the prefix "user".
	for i := 0; i < maxPrincipalSuggestions+5; i++ {
		email := "user" + string(rune('a'+i%26)) + string(rune('0'+i/26)) + "@example.com"
		if err := s.principals.Observe(email, "", nil, principal.SourceLogin); err != nil {
			t.Fatalf("Observe: %v", err)
		}
	}
	got, _ := getPrincipals(t, s, "user")
	if len(got) != maxPrincipalSuggestions {
		t.Errorf("got %d suggestions, want the cap of %d", len(got), maxPrincipalSuggestions)
	}
}

// TestPrincipalsRouteHubOnly proves the autocomplete route is hub-only: the
// default daemon never registers it (a 404, not a gate rejection).
func TestPrincipalsRouteHubOnly(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/principals", nil)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /api/principals on default daemon = %d, want 404 (hub-only route)", rec.Code)
	}
}
