package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newStore returns a Store over a fresh temp meta dir plus that dir, with a
// deterministic clock the test drives.
func newStore(t *testing.T, now *time.Time) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s := New(dir)
	s.now = func() time.Time { return *now }
	return s, dir
}

func rec(id, subject string, now time.Time) Record {
	return Record{
		ID:        id,
		Subject:   subject,
		Email:     subject + "@example.com",
		UserAgent: "Mozilla/5.0 (X11; Linux x86_64) Firefox/135.0",
		ExpiresAt: now.Add(12 * time.Hour),
	}
}

// TestMissingFileIsAnEmptyStore is the one distinction that must never invert:
// a registry file that does not exist yet is a hub's FIRST BOOT, so it must
// behave as an empty store and permit logins. Treating it like the corrupt case
// would lock every user out permanently the moment this shipped.
func TestMissingFileIsAnEmptyStore(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	s, dir := newStore(t, &now)

	if _, err := os.Stat(filepath.Join(dir, fileName)); !os.IsNotExist(err) {
		t.Fatalf("registry file exists before any write, want it absent (stat err = %v)", err)
	}
	if err := s.Err(); err != nil {
		t.Fatalf("Err() on a missing registry = %v, want nil (missing is empty, not broken)", err)
	}

	// It reads as empty...
	if _, ok, err := s.Lookup("anything"); ok || err != nil {
		t.Errorf("Lookup on an empty store = (%v, %v), want (false, nil)", ok, err)
	}
	if got, err := s.List("sub-1"); err != nil || len(got) != 0 {
		t.Errorf("List on an empty store = (%v, %v), want (empty, nil)", got, err)
	}

	// ...and, crucially, a login can be recorded into it.
	if err := s.Create(rec("sess-1", "sub-1", now)); err != nil {
		t.Fatalf("Create on an empty store error = %v, want nil -- a first login must succeed", err)
	}
	if _, ok, err := s.Lookup("sess-1"); !ok || err != nil {
		t.Errorf("Lookup after Create = (%v, %v), want (true, nil)", ok, err)
	}
}

// TestUnreadableAndCorruptFailClosed pins the other half: a registry that
// EXISTS but cannot be read or parsed fails every operation, so the gate
// refuses session-authenticated requests rather than silently forgetting which
// sessions were revoked.
func TestUnreadableAndCorruptFailClosed(t *testing.T) {
	tests := []struct {
		name  string
		write func(t *testing.T, path string)
	}{
		{
			name: "corrupt json",
			write: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("{not json at all"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "wrong json shape",
			write: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(`{"sessions":"not an array"}`), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "unreadable file",
			write: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("[]"), 0o000); err != nil {
					t.Fatal(err)
				}
				// A test running as root can read a 0000 file, which would make
				// this case silently vacuous -- say so instead.
				if _, err := os.ReadFile(path); err == nil { //nolint:gosec // fixture path
					t.Skip("running as a user that can read a 0000 file (root?); unreadable case not exercisable")
				}
			},
		},
	}

	now := time.Now().Truncate(time.Second)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.write(t, filepath.Join(dir, fileName))

			s := New(dir)
			s.now = func() time.Time { return now }

			if err := s.Err(); err == nil {
				t.Fatal("Err() = nil for an existing-but-unusable registry, want a fail-closed error")
			}
			if _, ok, err := s.Lookup("sess-1"); err == nil || ok {
				t.Errorf("Lookup = (%v, %v), want (false, an error) -- a broken registry must not authenticate", ok, err)
			}
			if _, err := s.List("sub-1"); err == nil {
				t.Error("List error = nil, want a fail-closed error")
			}
			if err := s.Create(rec("sess-1", "sub-1", now)); err == nil {
				t.Error("Create error = nil, want a fail-closed error")
			}
			if _, err := s.Revoke("sess-1", "sub-1"); err == nil {
				t.Error("Revoke error = nil, want a fail-closed error")
			}
			if err := s.End("sess-1"); err == nil {
				t.Error("End error = nil, want a fail-closed error")
			}
		})
	}
}

// TestErrorsArePathFree keeps this package to internal/fileedit's rule: no error
// message names a filesystem path.
func TestErrorsArePathFree(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, fileName)
	if err := os.WriteFile(path, []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(dir)

	errs := []error{s.Err()}
	if _, _, err := s.Lookup("x"); err != nil {
		errs = append(errs, err)
	}
	if err := s.Create(Record{ID: "a", Subject: "b", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		errs = append(errs, err)
	}
	for _, err := range errs {
		if err == nil {
			t.Fatal("expected an error to inspect")
		}
		if strings.Contains(err.Error(), dir) || strings.Contains(err.Error(), fileName) {
			t.Errorf("error %q names a path, want a path-free message", err)
		}
	}
}

func TestCreateValidates(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	s, _ := newStore(t, &now)

	tests := []struct {
		name string
		rec  Record
	}{
		{"no id", Record{Subject: "sub-1", ExpiresAt: now.Add(time.Hour)}},
		{"no subject", Record{ID: "sess-1", ExpiresAt: now.Add(time.Hour)}},
		{"no expiry", Record{ID: "sess-1", Subject: "sub-1"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.Create(tc.rec); err == nil {
				t.Error("Create error = nil, want a validation error")
			}
		})
	}
}

func TestCreateListRevokeRoundTrip(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	s, dir := newStore(t, &now)

	if err := s.Create(rec("sess-1", "sub-1", now)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if err := s.Create(rec("sess-2", "sub-1", now)); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(rec("sess-3", "sub-2", now)); err != nil {
		t.Fatal(err)
	}

	// List is scoped to the principal and ordered newest-first.
	got, err := s.List("sub-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "sess-2" || got[1].ID != "sess-1" {
		t.Fatalf("List(sub-1) = %v, want [sess-2 sess-1] (newest first)", ids(got))
	}
	if got[0].UserAgent == "" {
		t.Error("listed record has no user agent, want the recorded one")
	}

	// Another principal's session is invisible...
	for _, r := range got {
		if r.Subject != "sub-1" {
			t.Errorf("List(sub-1) returned a %q session", r.Subject)
		}
	}
	// ...and unrevocable, without saying so.
	revoked, err := s.Revoke("sess-3", "sub-1")
	if err != nil {
		t.Fatal(err)
	}
	if revoked {
		t.Error("Revoke of another principal's session = true, want false (404, not 403)")
	}
	if _, ok, _ := s.Lookup("sess-3"); !ok {
		t.Error("another principal's session was revoked, want it untouched")
	}

	// Revoking one's own works and is durable across a reload.
	revoked, err = s.Revoke("sess-1", "sub-1")
	if err != nil {
		t.Fatal(err)
	}
	if !revoked {
		t.Fatal("Revoke of an owned session = false, want true")
	}
	if _, ok, _ := s.Lookup("sess-1"); ok {
		t.Error("Lookup after Revoke = ok, want the session gone")
	}

	reloaded := New(dir)
	reloaded.now = func() time.Time { return now }
	if _, ok, err := reloaded.Lookup("sess-1"); ok || err != nil {
		t.Errorf("revoked session survived a reload: Lookup = (%v, %v)", ok, err)
	}
	if _, ok, _ := reloaded.Lookup("sess-2"); !ok {
		t.Error("a live session did not survive a reload, want it persisted")
	}
}

// TestEndIsUnconditional covers the logout path: the caller already presented
// the session's own cookie, so there is no ownership left to check.
func TestEndIsUnconditional(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	s, _ := newStore(t, &now)
	if err := s.Create(rec("sess-1", "sub-1", now)); err != nil {
		t.Fatal(err)
	}
	if err := s.End("sess-1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Lookup("sess-1"); ok {
		t.Error("Lookup after End = ok, want the session gone")
	}
	// A miss is a no-op, not an error.
	if err := s.End("sess-nope"); err != nil {
		t.Errorf("End of an unknown id = %v, want nil", err)
	}
}

func TestExpiredSessionsAreInvisibleAndPruned(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	s, dir := newStore(t, &now)
	if err := s.Create(rec("sess-old", "sub-1", now)); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(rec("sess-new", "sub-1", now)); err != nil {
		t.Fatal(err)
	}

	// Push one record past its expiry by hand, then advance the clock past it.
	now = now.Add(13 * time.Hour)
	if _, ok, _ := s.Lookup("sess-old"); ok {
		t.Error("Lookup of an expired session = ok, want it invisible")
	}
	if got, _ := s.List("sub-1"); len(got) != 0 {
		t.Errorf("List with only expired sessions = %v, want empty", ids(got))
	}

	// A write prunes them off disk, not just out of the view.
	fresh := rec("sess-fresh", "sub-1", now)
	if err := s.Create(fresh); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, fileName)) //nolint:gosec // fixture path
	if err != nil {
		t.Fatal(err)
	}
	var onDisk []Record
	if err := json.Unmarshal(data, &onDisk); err != nil {
		t.Fatal(err)
	}
	if len(onDisk) != 1 || onDisk[0].ID != "sess-fresh" {
		t.Errorf("on-disk records = %v, want only [sess-fresh] (expired ones pruned on write)", ids(onDisk))
	}
}

// TestLookupThrottlesLastSeen pins the hot-path guarantee: the revocation check
// runs on every authenticated request, so it must not write to disk each time.
func TestLookupThrottlesLastSeen(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	s, dir := newStore(t, &now)
	if err := s.Create(rec("sess-1", "sub-1", now)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, fileName)
	stat0, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	// Well within the throttle: LastSeen stays put and nothing is written.
	now = now.Add(TouchInterval - time.Second)
	got, ok, err := s.Lookup("sess-1")
	if !ok || err != nil {
		t.Fatalf("Lookup = (%v, %v), want (true, nil)", ok, err)
	}
	if !got.LastSeen.Equal(got.CreatedAt) {
		t.Errorf("LastSeen = %v, want it unbumped inside the %v throttle", got.LastSeen, TouchInterval)
	}
	stat1, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !stat1.ModTime().Equal(stat0.ModTime()) || stat1.Size() != stat0.Size() {
		t.Error("the registry file was rewritten by a throttled Lookup, want no disk write")
	}

	// Past the throttle: one bump, persisted.
	now = now.Add(2 * time.Second)
	got, ok, err = s.Lookup("sess-1")
	if !ok || err != nil {
		t.Fatalf("Lookup = (%v, %v), want (true, nil)", ok, err)
	}
	if !got.LastSeen.Equal(now) {
		t.Errorf("LastSeen = %v, want it bumped to %v once the throttle lapsed", got.LastSeen, now)
	}
	reloaded := New(dir)
	reloaded.now = func() time.Time { return now }
	persisted, ok, err := reloaded.Lookup("sess-1")
	if !ok || err != nil {
		t.Fatalf("reloaded Lookup = (%v, %v), want (true, nil)", ok, err)
	}
	if !persisted.LastSeen.Equal(now) {
		t.Errorf("persisted LastSeen = %v, want %v", persisted.LastSeen, now)
	}
}

// TestListRejectsEmptySubject pins that a caller with no subject -- the admin
// push token, or a user-token principal -- is handed nobody's sessions.
func TestListRejectsEmptySubject(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	s, _ := newStore(t, &now)
	if err := s.Create(rec("sess-1", "sub-1", now)); err != nil {
		t.Fatal(err)
	}
	got, err := s.List("")
	if err != nil || len(got) != 0 {
		t.Errorf("List(\"\") = (%v, %v), want (empty, nil)", ids(got), err)
	}
	if revoked, err := s.Revoke("sess-1", ""); revoked || err != nil {
		t.Errorf("Revoke with an empty subject = (%v, %v), want (false, nil)", revoked, err)
	}
}

// TestStartupPrunesExpired covers the startup half of pruning.
func TestStartupPrunesExpired(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	dir := t.TempDir()
	seed := []Record{
		{ID: "dead", Subject: "sub-1", CreatedAt: now, LastSeen: now, ExpiresAt: now.Add(-time.Hour)},
		{ID: "live", Subject: "sub-1", CreatedAt: now, LastSeen: now, ExpiresAt: now.Add(time.Hour)},
	}
	data, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fileName), data, 0o600); err != nil {
		t.Fatal(err)
	}

	s := New(dir)
	if _, ok, _ := s.Lookup("dead"); ok {
		t.Error("an expired session survived startup, want it pruned")
	}
	if _, ok, _ := s.Lookup("live"); !ok {
		t.Error("a live session was pruned at startup, want it kept")
	}

	after, err := os.ReadFile(filepath.Join(dir, fileName)) //nolint:gosec // fixture path
	if err != nil {
		t.Fatal(err)
	}
	var onDisk []Record
	if err := json.Unmarshal(after, &onDisk); err != nil {
		t.Fatal(err)
	}
	if len(onDisk) != 1 || onDisk[0].ID != "live" {
		t.Errorf("on-disk records after startup = %v, want only [live]", ids(onDisk))
	}
}

func ids(recs []Record) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.ID)
	}
	return out
}
