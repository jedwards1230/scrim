package authentik

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// fakeAuthentik stands up an httptest server that answers core/users and
// core/groups the way Authentik does (paged envelope), counting requests and
// recording every method seen so tests can assert read-only behavior.
type fakeAuthentik struct {
	users   []userResult
	groups  []groupResult
	reqs    atomic.Int64
	methods chan string
	// status, when non-zero, is returned for every request instead of 200.
	status int
}

func newFakeAuthentik(t *testing.T, f *fakeAuthentik) *httptest.Server {
	t.Helper()
	f.methods = make(chan string, 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.reqs.Add(1)
		select {
		case f.methods <- r.Method:
		default:
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if f.status != 0 {
			w.WriteHeader(f.status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v3/core/users/":
			_ = json.NewEncoder(w).Encode(usersPage{Results: f.users})
		case "/api/v3/core/groups/":
			_ = json.NewEncoder(w).Encode(groupsPage{Results: f.groups})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestClient(t *testing.T, base string, now func() time.Time, ttl time.Duration) *Client {
	t.Helper()
	c, err := New(Config{
		BaseURL:    base,
		Token:      "test-token",
		TTL:        ttl,
		HTTPClient: base2Client(),
		Now:        now,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return c
}

func base2Client() *http.Client { return &http.Client{Timeout: 5 * time.Second} }

func TestNewRejectsBadConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{"empty url", Config{Token: "t"}},
		{"missing scheme", Config{BaseURL: "auth.example.com", Token: "t"}},
		{"non-http scheme", Config{BaseURL: "ftp://auth.example.com", Token: "t"}},
		{"empty token", Config{BaseURL: "https://auth.example.com"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.cfg); err == nil {
				t.Fatalf("New(%+v) error = nil, want an error", tt.cfg)
			}
		})
	}
}

func TestClientMapsUsersAndGroups(t *testing.T) {
	fake := &fakeAuthentik{
		users: []userResult{
			{Email: "alice@example.com", Name: "Alice Anderson", IsActive: true, GroupsObj: []groupRef{{Name: "eng"}}},
			{Email: "bob@example.com", Username: "bob", IsActive: true},  // no Name -> username label
			{Email: "carol@example.com", Name: "Carol", IsActive: false}, // inactive -> skipped
			{Email: "", Name: "No Email", IsActive: true},                // no email -> skipped
		},
		groups: []groupResult{{Name: "eng"}, {Name: "design"}, {Name: ""}}, // empty group skipped
	}
	srv := newFakeAuthentik(t, fake)
	c := newTestClient(t, srv.URL, time.Now, 0)

	got := c.List()

	// Expected keys, email-sorted: alice, bob, design (group), eng (group).
	// carol (inactive) and the empty-email user are dropped.
	wantEmails := []string{"alice@example.com", "bob@example.com", "design", "eng"}
	if len(got) != len(wantEmails) {
		t.Fatalf("List() returned %d principals, want %d: %+v", len(got), len(wantEmails), got)
	}
	for i, want := range wantEmails {
		if got[i].Email != want {
			t.Errorf("principal[%d].Email = %q, want %q", i, got[i].Email, want)
		}
	}
	if got[0].DisplayName != "Alice Anderson" || len(got[0].GroupsSeen) != 1 || got[0].GroupsSeen[0] != "eng" {
		t.Errorf("alice = %+v, want display name + group eng", got[0])
	}
	if got[1].DisplayName != "bob" {
		t.Errorf("bob DisplayName = %q, want username fallback %q", got[1].DisplayName, "bob")
	}
}

func TestClientCachesUntilExpiry(t *testing.T) {
	fake := &fakeAuthentik{users: []userResult{{Email: "a@example.com", IsActive: true}}}
	srv := newFakeAuthentik(t, fake)

	clock := time.Unix(1_700_000_000, 0)
	now := func() time.Time { return clock }
	c := newTestClient(t, srv.URL, now, time.Minute)

	// First List is a miss -> fetches users + groups (2 HTTP calls).
	_ = c.List()
	after1 := fake.reqs.Load()
	if after1 == 0 {
		t.Fatal("first List made no requests")
	}

	// Second List within the TTL is a hit -> zero new requests.
	_ = c.List()
	if fake.reqs.Load() != after1 {
		t.Errorf("List within TTL made %d extra requests, want 0", fake.reqs.Load()-after1)
	}

	// Advance past the TTL -> next List refetches.
	clock = clock.Add(2 * time.Minute)
	_ = c.List()
	if fake.reqs.Load() <= after1 {
		t.Errorf("List after TTL made no new requests (still %d)", fake.reqs.Load())
	}
}

func TestClientDegradesOnError(t *testing.T) {
	fake := &fakeAuthentik{status: http.StatusInternalServerError}
	srv := newFakeAuthentik(t, fake)

	var logged int
	clock := time.Unix(1_700_000_000, 0)
	c, err := New(Config{
		BaseURL:    srv.URL,
		Token:      "test-token",
		TTL:        time.Minute,
		HTTPClient: base2Client(),
		Now:        func() time.Time { return clock },
		Log:        func(error) { logged++ },
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// A 500 from Authentik degrades to an empty view, never an error/panic.
	got := c.List()
	if len(got) != 0 {
		t.Errorf("List() on error returned %d principals, want 0 (degrade)", len(got))
	}
	if logged != 1 {
		t.Errorf("Log hook called %d times, want 1", logged)
	}

	// Within the error backoff window, a repeat List serves the cached empty
	// view without re-hitting the failing endpoint.
	before := fake.reqs.Load()
	_ = c.List()
	if fake.reqs.Load() != before {
		t.Errorf("List within error backoff made extra requests (%d)", fake.reqs.Load()-before)
	}
}

func TestClientIssuesOnlyGETs(t *testing.T) {
	fake := &fakeAuthentik{users: []userResult{{Email: "a@example.com", IsActive: true}}}
	srv := newFakeAuthentik(t, fake)
	c := newTestClient(t, srv.URL, time.Now, time.Minute)

	_ = c.List()
	close(fake.methods)
	for m := range fake.methods {
		if m != http.MethodGet {
			t.Errorf("saw a %s request; the driver must be read-only (GET only)", m)
		}
	}
}

// TestClientNeverPersists asserts invariant 2: the driver holds directory data
// only in memory. It runs a full List against a fake Authentik with the process
// CWD set to an empty temp dir and asserts nothing was written there.
func TestClientNeverPersists(t *testing.T) {
	dir := t.TempDir()
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWD) })

	fake := &fakeAuthentik{
		users:  []userResult{{Email: "a@example.com", Name: "A", IsActive: true}},
		groups: []groupResult{{Name: "eng"}},
	}
	srv := newFakeAuthentik(t, fake)
	c := newTestClient(t, srv.URL, time.Now, time.Minute)

	_ = c.List()
	_ = c.List()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("driver wrote %v to disk; directory data must never be persisted", names)
	}
}
