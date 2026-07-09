package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jedwards1230/scrim/internal/authentik"
	"github.com/jedwards1230/scrim/internal/config"
	"github.com/jedwards1230/scrim/internal/principal"
)

// staticLister is a principalLister returning a fixed slice, for exercising
// compositeLister's merge without a live registry or Authentik.
type staticLister []principal.Principal

func (s staticLister) List() []principal.Principal { return []principal.Principal(s) }

func TestCompositeListerMerges(t *testing.T) {
	lazy := staticLister{
		{Email: "alice@example.com", DisplayName: "Alice (observed)", GroupsSeen: []string{"eng"}},
		{Email: "bob@example.com"}, // observed but no display name yet
	}
	authentikSrc := staticLister{
		{Email: "alice@example.com", DisplayName: "Alice Anderson", GroupsSeen: []string{"eng", "leads"}},
		{Email: "bob@example.com", DisplayName: "Bob Barker"},
		{Email: "carol@example.com", DisplayName: "Carol", GroupsSeen: []string{"design"}}, // Authentik-only
		{Email: "", DisplayName: "dropme"},                                                 // empty email is ignored
	}
	c := compositeLister{sources: []principalLister{lazy, authentikSrc}}

	got := c.List()

	if len(got) != 3 {
		t.Fatalf("merged %d principals, want 3 (alice, bob, carol): %+v", len(got), got)
	}
	// Email-sorted.
	if got[0].Email != "alice@example.com" || got[1].Email != "bob@example.com" || got[2].Email != "carol@example.com" {
		t.Fatalf("merged order = %q,%q,%q, want alice,bob,carol", got[0].Email, got[1].Email, got[2].Email)
	}
	// Lazy source wins the display name on conflict (never lost).
	if got[0].DisplayName != "Alice (observed)" {
		t.Errorf("alice DisplayName = %q, want the lazy source's %q", got[0].DisplayName, "Alice (observed)")
	}
	// Groups are unioned across sources.
	if len(got[0].GroupsSeen) != 2 || got[0].GroupsSeen[0] != "eng" || got[0].GroupsSeen[1] != "leads" {
		t.Errorf("alice GroupsSeen = %v, want [eng leads]", got[0].GroupsSeen)
	}
	// A blank display name is filled from the later source.
	if got[1].DisplayName != "Bob Barker" {
		t.Errorf("bob DisplayName = %q, want Authentik's %q (blank was filled)", got[1].DisplayName, "Bob Barker")
	}
	// Authentik-only principal is added.
	if got[2].DisplayName != "Carol" {
		t.Errorf("carol DisplayName = %q, want Carol (Authentik-only add)", got[2].DisplayName)
	}
}

// fakeAuthentikServer stands up an httptest Authentik answering one active user
// and one group, for the NewHub wiring tests.
func fakeAuthentikServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v3/core/users/":
			_, _ = w.Write([]byte(`{"pagination":{"next":0},"results":[{"email":"dave@example.com","name":"Dave Directory","is_active":true,"groups_obj":[{"name":"ops"}]}]}`))
		case "/api/v3/core/groups/":
			_, _ = w.Write([]byte(`{"pagination":{"next":0},"results":[{"name":"ops"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newAuthentikHub(t *testing.T, cfg *authentik.Config) *Server {
	t.Helper()
	s, err := NewHub(config.Config{
		Dir:         t.TempDir(),
		Host:        "127.0.0.1",
		Port:        0,
		IdleTimeout: time.Hour,
		NoAuth:      true,
	}, HubOptions{
		PushToken:  "test-push-token",
		AllowCIDRs: []string{"127.0.0.0/8"},
		Authentik:  cfg,
	})
	if err != nil {
		t.Fatalf("NewHub() error = %v", err)
	}
	return s
}

func TestNewHubComposesAuthentikDirectory(t *testing.T) {
	srv := fakeAuthentikServer(t)
	s := newAuthentikHub(t, &authentik.Config{BaseURL: srv.URL, Token: "t"})

	// A locally-observed principal plus the Authentik pull should both surface
	// through GET /api/principals, de-duped and email-sorted.
	if err := s.principals.Observe("alice@example.com", "Alice", []string{"eng"}, principal.SourceLogin); err != nil {
		t.Fatalf("Observe: %v", err)
	}

	got, code := getPrincipals(t, s, "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	// alice (lazy) + dave (Authentik user) + ops (Authentik group) = 3.
	byEmail := map[string]principalSuggestion{}
	for _, p := range got {
		byEmail[p.Email] = p
	}
	if _, ok := byEmail["alice@example.com"]; !ok {
		t.Error("merged view missing the lazily-observed alice")
	}
	dave, ok := byEmail["dave@example.com"]
	if !ok {
		t.Fatal("merged view missing the Authentik-pulled dave")
	}
	if dave.DisplayName != "Dave Directory" {
		t.Errorf("dave DisplayName = %q, want Dave Directory", dave.DisplayName)
	}
	if _, ok := byEmail["ops"]; !ok {
		t.Error("merged view missing the Authentik group 'ops'")
	}
}

func TestNewHubRejectsMalformedAuthentikURL(t *testing.T) {
	_, err := NewHub(config.Config{Dir: t.TempDir()}, HubOptions{
		PushToken: "tok",
		Authentik: &authentik.Config{BaseURL: "://not a url", Token: "t"},
	})
	if err == nil {
		t.Fatal("NewHub() with a malformed Authentik URL error = nil, want a startup error")
	}
}

func TestNewHubAuthentikUnreachableDegrades(t *testing.T) {
	// A URL that will never connect: the hub still starts, and GET /api/principals
	// returns just the lazily-observed principals (autocomplete degrades cleanly).
	s := newAuthentikHub(t, &authentik.Config{
		BaseURL:    "http://127.0.0.1:1",
		Token:      "t",
		HTTPClient: &http.Client{Timeout: 200 * time.Millisecond},
	})
	if err := s.principals.Observe("alice@example.com", "Alice", nil, principal.SourceLogin); err != nil {
		t.Fatalf("Observe: %v", err)
	}

	got, code := getPrincipals(t, s, "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (unreachable Authentik must not fail the request)", code)
	}
	if len(got) != 1 || got[0].Email != "alice@example.com" {
		t.Fatalf("degraded view = %+v, want just the lazily-observed alice", got)
	}
	// Sanity: the response really is JSON of the suggestion shape.
	if _, err := json.Marshal(got); err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
}
