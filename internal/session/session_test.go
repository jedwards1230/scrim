package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testIdleTTL / testMaxLifetime are the renewal policy the shared helpers use.
// The idle window matches rec's own 12h expiry so a freshly created record
// sits exactly one window out, which keeps the extension assertions readable.
const (
	testIdleTTL     = 12 * time.Hour
	testMaxLifetime = 30 * 24 * time.Hour
)

// newStore returns a Store over a fresh temp meta dir plus that dir, with a
// deterministic clock the test drives and the default test renewal policy.
func newStore(t *testing.T, now *time.Time) (*Store, string) {
	t.Helper()
	return newStoreWithPolicy(t, now, testIdleTTL, testMaxLifetime)
}

// newStoreWithPolicy is newStore with an explicit idle window and absolute
// cap, for the tests that are about the policy itself.
func newStoreWithPolicy(t *testing.T, now *time.Time, idleTTL, maxLifetime time.Duration) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s := New(dir, idleTTL, maxLifetime)
	s.now = func() time.Time { return *now }
	return s, dir
}

// reopen reloads dir's registry with the same test policy and a fixed clock --
// the "did it actually reach disk" half of several tests.
func reopen(t *testing.T, dir string, now time.Time) *Store {
	t.Helper()
	s := New(dir, testIdleTTL, testMaxLifetime)
	s.now = func() time.Time { return now }
	return s
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

			s := New(dir, testIdleTTL, testMaxLifetime)
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
	s := New(dir, testIdleTTL, testMaxLifetime)

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

	reloaded := New(dir, testIdleTTL, testMaxLifetime)
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

// TestLookupNeverWrites pins the hot-path guarantee at its strictest: the
// revocation check runs on every authenticated request, so Lookup must never
// touch the disk at all -- not throttled, not at all. The throttled write
// moved to Renew.
func TestLookupNeverWrites(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	s, dir := newStore(t, &now)
	if err := s.Create(rec("sess-1", "sub-1", now)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, fileName)
	before := statFile(t, path)

	// Well past any throttle window, and many times over.
	for i := range 3 {
		now = now.Add(TouchInterval + time.Minute)
		got, ok, err := s.Lookup("sess-1")
		if !ok || err != nil {
			t.Fatalf("Lookup #%d = (%v, %v), want (true, nil)", i, ok, err)
		}
		if !got.LastSeen.Equal(got.CreatedAt) {
			t.Errorf("Lookup #%d bumped LastSeen to %v, want it left at %v", i, got.LastSeen, got.CreatedAt)
		}
	}
	if after := statFile(t, path); after != before {
		t.Errorf("the registry file changed (%v -> %v) across Lookups, want no disk write", before, after)
	}
}

// TestRenewThrottles pins the cost guarantee for the sliding window: inside
// TouchInterval, Renew writes nothing and reports no extension (so the gate
// issues no Set-Cookie either); past it, one write carrying both the bumped
// LastSeen and the slid expiry.
func TestRenewThrottles(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	s, dir := newStore(t, &now)
	if err := s.Create(rec("sess-1", "sub-1", now)); err != nil {
		t.Fatal(err)
	}
	created := now
	path := filepath.Join(dir, fileName)
	before := statFile(t, path)

	// Inside the throttle: nothing moves, nothing is written, and the caller
	// is told not to re-issue the cookie.
	now = now.Add(TouchInterval - time.Second)
	got, extended, err := s.Renew("sess-1")
	if err != nil {
		t.Fatalf("Renew error = %v, want nil", err)
	}
	if extended {
		t.Error("Renew reported extended=true inside the throttle, want false (no Set-Cookie)")
	}
	if !got.LastSeen.Equal(created) || !got.ExpiresAt.Equal(created.Add(testIdleTTL)) {
		t.Errorf("Renew inside the throttle moved the record (LastSeen %v, ExpiresAt %v), want it untouched", got.LastSeen, got.ExpiresAt)
	}
	if after := statFile(t, path); after != before {
		t.Errorf("the registry file changed (%v -> %v) inside the throttle, want no disk write", before, after)
	}

	// Past it: one write, and both timestamps move.
	now = now.Add(2 * time.Second)
	got, extended, err = s.Renew("sess-1")
	if err != nil {
		t.Fatalf("Renew error = %v, want nil", err)
	}
	if !extended {
		t.Error("Renew reported extended=false past the throttle, want true")
	}
	if !got.LastSeen.Equal(now) {
		t.Errorf("LastSeen = %v, want %v", got.LastSeen, now)
	}
	want := now.Add(testIdleTTL)
	if !got.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want it slid to %v", got.ExpiresAt, want)
	}
	persisted, ok, err := reopen(t, dir, now).Lookup("sess-1")
	if !ok || err != nil {
		t.Fatalf("reloaded Lookup = (%v, %v), want (true, nil)", ok, err)
	}
	if !persisted.ExpiresAt.Equal(want) || !persisted.LastSeen.Equal(now) {
		t.Errorf("persisted record = (LastSeen %v, ExpiresAt %v), want (%v, %v)", persisted.LastSeen, persisted.ExpiresAt, now, want)
	}
}

// TestRenewClampsToMaxLifetime is the absolute-cap contract: renewal walks a
// session forward until CreatedAt+maxLifetime and no further, stops reporting
// extensions once it is pinned there, and then lets the session expire for
// real rather than sliding past the cap.
func TestRenewClampsToMaxLifetime(t *testing.T) {
	const (
		idle    = time.Hour
		maxLife = 4 * time.Hour
	)
	now := time.Now().Truncate(time.Second)
	created := now
	s, _ := newStoreWithPolicy(t, &now, idle, maxLife)
	if err := s.Create(Record{ID: "sess-1", Subject: "sub-1", ExpiresAt: now.Add(idle)}); err != nil {
		t.Fatal(err)
	}
	hardStop := created.Add(maxLife)

	tests := []struct {
		name         string
		advance      time.Duration
		wantExtended bool
		wantExpiry   time.Duration // from created; only read when the session is still live
		wantLive     bool
	}{
		{"first renewal slides a full window", 45 * time.Minute, true, 45*time.Minute + idle, true},
		{"second renewal slides again", 45 * time.Minute, true, 90*time.Minute + idle, true},
		{"third renewal slides again", 45 * time.Minute, true, 135*time.Minute + idle, true},
		{"renewal near the cap is clamped to it", 45 * time.Minute, true, maxLife, true},
		{"at the cap there is nothing left to extend", 30 * time.Minute, false, maxLife, true},
		{"past the cap the session is simply gone", time.Hour, false, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			now = now.Add(tc.advance)
			got, extended, err := s.Renew("sess-1")
			if err != nil {
				t.Fatalf("Renew error = %v, want nil", err)
			}
			if extended != tc.wantExtended {
				t.Errorf("extended = %v, want %v", extended, tc.wantExtended)
			}
			_, live, err := s.Lookup("sess-1")
			if err != nil {
				t.Fatalf("Lookup error = %v, want nil", err)
			}
			if live != tc.wantLive {
				t.Fatalf("session live = %v, want %v", live, tc.wantLive)
			}
			if !tc.wantLive {
				return
			}
			if want := created.Add(tc.wantExpiry); !got.ExpiresAt.Equal(want) {
				t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, want)
			}
			if got.ExpiresAt.After(hardStop) {
				t.Errorf("ExpiresAt = %v, past the absolute cap %v", got.ExpiresAt, hardStop)
			}
		})
	}
}

// TestRenewNeverResurrects is the one that matters most: renewal must extend a
// LIVE session and nothing else. A revoked, ended, expired, or unknown id is a
// miss -- never a record written back into existence with a fresh deadline.
func TestRenewNeverResurrects(t *testing.T) {
	tests := []struct {
		name string
		// kill takes the store past the point where sess-1 should be renewable
		// and returns the clock offset to renew at.
		kill func(t *testing.T, s *Store) time.Duration
		id   string
	}{
		{
			name: "revoked",
			kill: func(t *testing.T, s *Store) time.Duration {
				t.Helper()
				if revoked, err := s.Revoke("sess-1", "sub-1"); !revoked || err != nil {
					t.Fatalf("Revoke = (%v, %v), want (true, nil)", revoked, err)
				}
				return TouchInterval + time.Minute
			},
			id: "sess-1",
		},
		{
			name: "signed out",
			kill: func(t *testing.T, s *Store) time.Duration {
				t.Helper()
				if err := s.End("sess-1"); err != nil {
					t.Fatal(err)
				}
				return TouchInterval + time.Minute
			},
			id: "sess-1",
		},
		{
			name: "lapsed past its idle window",
			kill: func(t *testing.T, _ *Store) time.Duration { return testIdleTTL + time.Minute },
			id:   "sess-1",
		},
		{
			name: "never existed",
			kill: func(t *testing.T, _ *Store) time.Duration { return TouchInterval + time.Minute },
			id:   "sess-nope",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().Truncate(time.Second)
			s, dir := newStore(t, &now)
			if err := s.Create(rec("sess-1", "sub-1", now)); err != nil {
				t.Fatal(err)
			}
			advance := tc.kill(t, s)
			now = now.Add(advance)

			got, extended, err := s.Renew(tc.id)
			if err != nil {
				t.Fatalf("Renew error = %v, want nil (a dead session is a miss, not a failure)", err)
			}
			if extended {
				t.Error("Renew extended a dead session, want extended=false")
			}
			if got.ID != "" {
				t.Errorf("Renew returned record %q, want the zero record", got.ID)
			}
			// And it must not be back: neither in memory nor on disk.
			if _, ok, err := s.Lookup(tc.id); ok || err != nil {
				t.Errorf("Lookup after Renew = (%v, %v), want (false, nil) -- renewal resurrected a dead session", ok, err)
			}
			if _, ok, err := reopen(t, dir, now).Lookup(tc.id); ok || err != nil {
				t.Errorf("reloaded Lookup = (%v, %v), want (false, nil) -- a dead session was written back", ok, err)
			}
		})
	}
}

// TestRenewNeverShrinks pins the other direction: a record whose expiry is
// already further out than a fresh idle window (an operator shortened the
// window, say) keeps the later deadline.
func TestRenewNeverShrinks(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	s, _ := newStoreWithPolicy(t, &now, time.Hour, testMaxLifetime)
	far := now.Add(6 * time.Hour)
	if err := s.Create(Record{ID: "sess-1", Subject: "sub-1", ExpiresAt: far}); err != nil {
		t.Fatal(err)
	}

	now = now.Add(TouchInterval + time.Minute)
	got, extended, err := s.Renew("sess-1")
	if err != nil {
		t.Fatalf("Renew error = %v, want nil", err)
	}
	if extended {
		t.Error("extended = true, want false -- there was nothing to extend")
	}
	if !got.ExpiresAt.Equal(far) {
		t.Errorf("ExpiresAt = %v, want the original, later %v -- renewal must never shrink an expiry", got.ExpiresAt, far)
	}
}

// TestCreateClampsToMaxLifetime covers the misconfiguration where the idle
// window is longer than the absolute cap: the very first session gets the cap,
// not a lifetime no renewed session could ever reach.
func TestCreateClampsToMaxLifetime(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	s, _ := newStoreWithPolicy(t, &now, 48*time.Hour, 6*time.Hour)
	if err := s.Create(Record{ID: "sess-1", Subject: "sub-1", ExpiresAt: now.Add(48 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Lookup("sess-1")
	if !ok || err != nil {
		t.Fatalf("Lookup = (%v, %v), want (true, nil)", ok, err)
	}
	if want := now.Add(6 * time.Hour); !got.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want it clamped to the cap at %v", got.ExpiresAt, want)
	}
}

// TestRenewFailsClosed keeps renewal inside the store's fail-closed contract:
// an unreadable registry errors rather than quietly extending anything.
func TestRenewFailsClosed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(dir, testIdleTTL, testMaxLifetime)
	if _, extended, err := s.Renew("sess-1"); err == nil || extended {
		t.Errorf("Renew on a corrupt registry = (%v, %v), want (false, an error)", extended, err)
	}
}

// statFile returns a cheap change-detector for the registry file: its size and
// modification time. Tests compare it across calls to assert "nothing was
// written", which is the whole point of the throttle.
func statFile(t *testing.T, path string) [2]int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return [2]int64{fi.Size(), fi.ModTime().UnixNano()}
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

	s := New(dir, testIdleTTL, testMaxLifetime)
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
