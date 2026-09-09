package agentconn

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newStore returns a store over a fresh temp meta dir with a controllable
// clock, plus the clock's setter -- the touch/revocation rules are all about
// time, so a real clock would make them untestable or flaky.
func newStore(t *testing.T) (*Store, func(time.Time)) {
	t.Helper()
	dir := t.TempDir()
	s := New(dir)
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	return s, func(v time.Time) { now = v }
}

// admit is a shorthand for the common Admit call: it fails the test on a store
// error (the fail-closed cases assert on the error explicitly instead).
func admit(t *testing.T, s *Store, subject, clientID string, iat time.Time) bool {
	t.Helper()
	ok, err := s.Admit(subject, clientID, subject+"@example.com", iat)
	if err != nil {
		t.Fatalf("Admit(%q, %q): %v", subject, clientID, err)
	}
	return ok
}

// TestAdmitRecordsAndLists covers the happy path: a first call creates a row,
// a second reuses it, and the row is visible only to its own principal.
func TestAdmitRecordsAndLists(t *testing.T) {
	s, _ := newStore(t)

	if !admit(t, s, "sub-alice", "claude", time.Now()) {
		t.Fatal("first Admit refused, want it admitted")
	}
	if !admit(t, s, "sub-alice", "claude", time.Now()) {
		t.Fatal("second Admit refused, want it admitted")
	}

	got, err := s.List("sub-alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("alice has %d connections, want exactly one row per (subject, client)", len(got))
	}
	if got[0].ClientID != "claude" || got[0].Subject != "sub-alice" {
		t.Errorf("record = %+v, want it keyed on alice+claude", got[0])
	}
	if got[0].FirstSeen.IsZero() || got[0].LastSeen.IsZero() {
		t.Errorf("record timestamps = %+v, want both populated", got[0])
	}
	if got[0].Revoked() {
		t.Error("a fresh connection reports as revoked, want it live")
	}

	// A second client for the same principal is its own connection.
	admit(t, s, "sub-alice", "other-agent", time.Now())
	if got, _ := s.List("sub-alice"); len(got) != 2 {
		t.Errorf("alice has %d connections after a second client, want 2", len(got))
	}

	// Another principal's connections are not hers, and an empty subject lists
	// nothing at all.
	admit(t, s, "sub-bob", "claude", time.Now())
	if got, _ := s.List("sub-alice"); len(got) != 2 {
		t.Errorf("alice sees %d connections, want only her own 2", len(got))
	}
	if got, _ := s.List(""); got != nil {
		t.Errorf("List(\"\") = %v, want nothing -- a caller with no subject has no agents", got)
	}
}

// TestRevokedConnectionIsBlocked is the property the feature exists for:
// revoking a connection blocks that client on its very next request, and
// touches no other client or principal.
func TestRevokedConnectionIsBlocked(t *testing.T) {
	s, setNow := newStore(t)
	base := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

	issued := base.Add(-time.Hour)
	admit(t, s, "sub-alice", "claude", issued)
	admit(t, s, "sub-alice", "other-agent", issued)
	admit(t, s, "sub-bob", "claude", issued)

	recs, _ := s.List("sub-alice")
	var claudeID string
	for _, rec := range recs {
		if rec.ClientID == "claude" {
			claudeID = rec.ID
		}
	}
	if claudeID == "" {
		t.Fatal("no claude connection to revoke")
	}
	setNow(base.Add(time.Minute))
	ok, err := s.Revoke(claudeID, "sub-alice")
	if err != nil || !ok {
		t.Fatalf("Revoke = (%v, %v), want (true, nil)", ok, err)
	}

	setNow(base.Add(2 * time.Minute))
	if admit(t, s, "sub-alice", "claude", issued) {
		t.Error("a revoked connection was admitted, want it blocked on its next request")
	}
	if !admit(t, s, "sub-alice", "other-agent", issued) {
		t.Error("alice's other agent was blocked, want the revocation scoped to one client")
	}
	if !admit(t, s, "sub-bob", "claude", issued) {
		t.Error("bob's connection was blocked, want the revocation scoped to one principal")
	}

	// The revoked row stays listed, marked, so the page can show it as history.
	recs, _ = s.List("sub-alice")
	for _, rec := range recs {
		if rec.ID == claudeID && !rec.Revoked() {
			t.Error("the revoked connection no longer reports as revoked")
		}
	}
}

// TestReauthorizationRestoresAccess pins the `iat` rule: a token issued AFTER
// the revocation is admitted (that is how re-authorizing at the IdP restores
// access) and clears the stale revocation, while an older token stays blocked.
func TestReauthorizationRestoresAccess(t *testing.T) {
	s, setNow := newStore(t)
	base := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

	old := base.Add(-time.Hour)
	admit(t, s, "sub-alice", "claude", old)
	recs, _ := s.List("sub-alice")
	setNow(base)
	if ok, _ := s.Revoke(recs[0].ID, "sub-alice"); !ok {
		t.Fatal("Revoke did not take")
	}

	setNow(base.Add(time.Minute))
	if admit(t, s, "sub-alice", "claude", old) {
		t.Fatal("the pre-revocation token was admitted, want it blocked")
	}

	// A token minted after the revocation: the principal authorized the client
	// again, which must work.
	fresh := base.Add(30 * time.Second)
	if !admit(t, s, "sub-alice", "claude", fresh) {
		t.Fatal("a token issued after the revocation was blocked, want re-authorization to restore access")
	}
	recs, _ = s.List("sub-alice")
	if recs[0].Revoked() {
		t.Error("the connection still reports revoked after re-authorization, want the page to say it is live")
	}
	// And the connection is live again for subsequent calls with that token.
	if !admit(t, s, "sub-alice", "claude", fresh) {
		t.Error("the re-authorized connection was blocked on its next call")
	}
}

// TestRevocationHoldsWithoutClientIDOrIssuedAt pins the fail-closed half. A
// caller that cannot prove its credential postdates the revocation -- the HMAC
// forwarded-identity plane (no client id) or a token with no `iat` -- stays
// blocked. A revocation must never be silently unenforceable.
func TestRevocationHoldsWithoutClientIDOrIssuedAt(t *testing.T) {
	base := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

	t.Run("no client id: the subject's empty-client row governs", func(t *testing.T) {
		s, setNow := newStore(t)
		admit(t, s, "sub-alice", "", base.Add(-time.Hour))
		recs, _ := s.List("sub-alice")
		if recs[0].ClientID != "" {
			t.Fatalf("recorded client id = %q, want empty", recs[0].ClientID)
		}
		setNow(base)
		if ok, _ := s.Revoke(recs[0].ID, "sub-alice"); !ok {
			t.Fatal("Revoke did not take")
		}
		setNow(base.Add(time.Minute))
		// Even a token minted after the revocation cannot lift it: with no client
		// id there is nothing to prove it belongs to this connection.
		if admit(t, s, "sub-alice", "", base.Add(time.Minute)) {
			t.Error("a client-id-less caller was admitted past a revocation, want it blocked")
		}
	})

	t.Run("no issued-at: a revocation blocks unconditionally", func(t *testing.T) {
		s, setNow := newStore(t)
		admit(t, s, "sub-alice", "claude", base.Add(-time.Hour))
		recs, _ := s.List("sub-alice")
		setNow(base)
		if ok, _ := s.Revoke(recs[0].ID, "sub-alice"); !ok {
			t.Fatal("Revoke did not take")
		}
		setNow(base.Add(time.Minute))
		if admit(t, s, "sub-alice", "claude", time.Time{}) {
			t.Error("a token with no iat was admitted past a revocation, want it blocked")
		}
	})
}

// TestAdmitWithoutSubjectRecordsNothing: with no principal there is no row to
// key, so the call is admitted and nothing is stored. (The hub only reaches
// Admit for an authenticated actor, so this is the degenerate case where the
// forwarding plane supplied no id.)
func TestAdmitWithoutSubjectRecordsNothing(t *testing.T) {
	s, _ := newStore(t)
	ok, err := s.Admit("", "claude", "", time.Now())
	if err != nil || !ok {
		t.Fatalf("Admit with no subject = (%v, %v), want (true, nil)", ok, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(s.path), fileName)); !os.IsNotExist(err) {
		t.Errorf("a subject-less Admit wrote the registry (stat err = %v), want nothing recorded", err)
	}
}

// TestRevokeOnlyOwnConnections mirrors the session/token stores: another
// principal's id is a miss (the handler turns that into a 404, never a 403),
// and so is an unknown id or one already revoked.
func TestRevokeOnlyOwnConnections(t *testing.T) {
	s, _ := newStore(t)
	admit(t, s, "sub-alice", "claude", time.Now())
	admit(t, s, "sub-bob", "claude", time.Now())

	bobs, _ := s.List("sub-bob")
	if len(bobs) != 1 {
		t.Fatalf("bob has %d connections, want 1", len(bobs))
	}

	if ok, err := s.Revoke(bobs[0].ID, "sub-alice"); ok || err != nil {
		t.Errorf("alice revoking bob's connection = (%v, %v), want (false, nil)", ok, err)
	}
	if ok, _ := s.Revoke("no-such-id", "sub-alice"); ok {
		t.Error("revoking an unknown id reported success")
	}
	if ok, _ := s.Revoke("", "sub-alice"); ok {
		t.Error("revoking an empty id reported success")
	}
	// Bob's connection is untouched.
	if !admit(t, s, "sub-bob", "claude", time.Now()) {
		t.Error("bob's connection was blocked by alice's revoke attempt")
	}

	// Revoking twice: the second is a miss, so the API answers 404 rather than
	// pretending it just did something.
	alices, _ := s.List("sub-alice")
	if ok, _ := s.Revoke(alices[0].ID, "sub-alice"); !ok {
		t.Fatal("first revoke failed")
	}
	if ok, _ := s.Revoke(alices[0].ID, "sub-alice"); ok {
		t.Error("revoking an already-revoked connection reported success, want a miss")
	}
}

// TestMissingFileIsEmptyStore is one half of the load contract: a hub that has
// never seen an agent must work normally, not lock every agent out.
func TestMissingFileIsEmptyStore(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	if err := s.Err(); err != nil {
		t.Fatalf("Err() on a missing registry = %v, want nil (missing means empty)", err)
	}
	got, err := s.List("sub-alice")
	if err != nil || len(got) != 0 {
		t.Errorf("List on a missing registry = (%v, %v), want (empty, nil)", got, err)
	}
	if ok, err := s.Admit("sub-alice", "claude", "", time.Now()); !ok || err != nil {
		t.Errorf("Admit against a missing registry = (%v, %v), want (true, nil)", ok, err)
	}
}

// TestCorruptFileFailsClosed is the other half: a registry that EXISTS and
// cannot be parsed must refuse everything rather than silently forget which
// agents were revoked.
func TestCorruptFileFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{name: "not json", data: []byte("{ this is not the registry")},
		{name: "wrong shape", data: []byte(`{"records":[]}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, fileName), tc.data, 0o600); err != nil {
				t.Fatal(err)
			}
			s := New(dir)
			if s.Err() == nil {
				t.Fatal("Err() on a corrupt registry = nil, want the fail-closed error")
			}
			if ok, err := s.Admit("sub-alice", "claude", "", time.Now()); ok || err == nil {
				t.Errorf("Admit against a corrupt registry = (%v, %v), want (false, error)", ok, err)
			}
			if _, err := s.List("sub-alice"); err == nil {
				t.Error("List against a corrupt registry returned no error, want it to fail closed")
			}
			if _, err := s.Revoke("some-id", "sub-alice"); err == nil {
				t.Error("Revoke against a corrupt registry returned no error, want it to fail closed")
			}
		})
	}
}

// TestUnreadableFileFailsClosed: a registry present but unreadable (bad
// permissions) is a failure, not an empty store.
func TestUnreadableFileFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not deny reads")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte("[]"), 0o000); err != nil {
		t.Fatal(err)
	}
	if err := New(dir).Err(); err == nil {
		t.Error("Err() on an unreadable registry = nil, want the fail-closed error")
	}
}

// TestLastSeenWriteIsThrottled pins the hot-path property: a repeat call inside
// TouchInterval neither bumps LastSeen nor rewrites the file; one after it does.
func TestLastSeenWriteIsThrottled(t *testing.T) {
	s, setNow := newStore(t)
	base := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	admit(t, s, "sub-alice", "claude", base)

	modTime := func() time.Time {
		t.Helper()
		fi, err := os.Stat(s.path)
		if err != nil {
			t.Fatal(err)
		}
		return fi.ModTime()
	}
	before := modTime()

	setNow(base.Add(TouchInterval - time.Second))
	admit(t, s, "sub-alice", "claude", base)
	recs, _ := s.List("sub-alice")
	if !recs[0].LastSeen.Equal(base) {
		t.Errorf("LastSeen = %v after a call inside TouchInterval, want it unchanged (%v)", recs[0].LastSeen, base)
	}
	if got := modTime(); !got.Equal(before) {
		t.Error("the registry was rewritten inside TouchInterval, want the hot path write-free")
	}

	setNow(base.Add(TouchInterval))
	admit(t, s, "sub-alice", "claude", base)
	recs, _ = s.List("sub-alice")
	if !recs[0].LastSeen.Equal(base.Add(TouchInterval)) {
		t.Errorf("LastSeen = %v after TouchInterval elapsed, want it bumped", recs[0].LastSeen)
	}
}

// TestPruneKeepsRevocations: a stale LIVE connection is pruned on load, a
// stale REVOKED one is not -- dropping it would un-revoke the client.
func TestPruneKeepsRevocations(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-2 * Retention)
	recs := []Record{
		{ID: RecordID("sub-alice", "stale"), Subject: "sub-alice", ClientID: "stale", FirstSeen: old, LastSeen: old},
		{ID: RecordID("sub-alice", "cut"), Subject: "sub-alice", ClientID: "cut", FirstSeen: old, LastSeen: old, RevokedAt: old},
	}
	data, err := json.Marshal(recs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fileName), data, 0o600); err != nil {
		t.Fatal(err)
	}

	s := New(dir)
	got, err := s.List("sub-alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ClientID != "cut" {
		t.Fatalf("after prune, connections = %+v, want only the revoked one kept", got)
	}
	// And it still blocks.
	if ok, _ := s.Admit("sub-alice", "cut", "", old.Add(-time.Hour)); ok {
		t.Error("a surviving revocation admitted an old token, want it still enforced")
	}
}

// TestRecordIDIsDeterministicAndDoesNotLeakTheSubject: the same pair always
// hashes to the same id (the hot path derives the key rather than searching),
// different pairs don't collide, and the id in a URL doesn't spell out who it
// belongs to.
func TestRecordIDIsDeterministicAndDoesNotLeakTheSubject(t *testing.T) {
	a := RecordID("sub-alice", "claude")
	if a != RecordID("sub-alice", "claude") {
		t.Error("RecordID is not deterministic")
	}
	for _, other := range []string{
		RecordID("sub-alice", "other"),
		RecordID("sub-bob", "claude"),
		RecordID("sub-alice", ""),
	} {
		if a == other {
			t.Errorf("RecordID collision between distinct pairs (%q)", other)
		}
	}
	// Domain separation: the two fields are not simply concatenated, so
	// ("sub-al", "iceclaude") is a different connection than ("sub-alice", "claude").
	if RecordID("sub-al", "iceclaude") == a {
		t.Error("RecordID does not separate its two fields")
	}
	if len(a) != 32 {
		t.Errorf("RecordID length = %d, want 32 hex chars", len(a))
	}
}
