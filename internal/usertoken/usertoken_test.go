package usertoken

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jedwards1230/scrim/internal/canvas"
)

func TestMintReturnsRawOnceAndStoresOnlyHash(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)

	raw, tok, err := s.Mint("laptop", "alice@example.com", nil, Allowance{})
	if err != nil {
		t.Fatal(err)
	}
	if raw == "" {
		t.Fatal("Mint returned an empty raw token")
	}
	if tok.OwnerEmail != "alice@example.com" || tok.Name != "laptop" {
		t.Errorf("token metadata = %+v, want laptop/alice", tok)
	}
	if tok.Hash == "" || strings.Contains(tok.Hash, raw) {
		t.Errorf("stored hash = %q, want a SHA-256 that is not the raw token", tok.Hash)
	}

	// The on-disk file must contain the hash but NEVER the raw secret.
	data, err := os.ReadFile(filepath.Join(dir, fileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), raw) {
		t.Error("tokens.json contains the raw secret, want only its hash")
	}
	if !strings.Contains(string(data), tok.Hash) {
		t.Error("tokens.json missing the token hash")
	}
	// The token file holds secret hashes -> must be owner-only (0600).
	fi, err := os.Stat(filepath.Join(dir, fileName))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("tokens.json perm = %o, want 0600", perm)
	}
}

func TestMintRequiresOwner(t *testing.T) {
	if _, _, err := New(t.TempDir()).Mint("x", "", nil, Allowance{}); err == nil {
		t.Error("Mint with empty owner error = nil, want an error")
	}
}

func TestLookup(t *testing.T) {
	s := New(t.TempDir())
	raw, tok, err := s.Mint("laptop", "alice@example.com", nil, Allowance{})
	if err != nil {
		t.Fatal(err)
	}

	got, ok := s.Lookup(raw)
	if !ok {
		t.Fatal("Lookup of a valid raw token = not ok, want ok")
	}
	if got.ID != tok.ID || got.OwnerEmail != "alice@example.com" {
		t.Errorf("Lookup returned %+v, want the minted token", got)
	}
	if got.LastUsed == nil {
		t.Error("Lookup did not bump LastUsed")
	}

	if _, ok := s.Lookup("not-a-real-token"); ok {
		t.Error("Lookup of a bogus token = ok, want not ok")
	}
	if _, ok := s.Lookup(""); ok {
		t.Error("Lookup of an empty token = ok, want not ok")
	}
}

func TestRevokedTokenDoesNotLookUp(t *testing.T) {
	s := New(t.TempDir())
	raw, tok, err := s.Mint("laptop", "alice@example.com", nil, Allowance{})
	if err != nil {
		t.Fatal(err)
	}
	ok, err := s.Revoke(tok.ID, "alice@example.com")
	if err != nil || !ok {
		t.Fatalf("Revoke = (%v, %v), want (true, nil)", ok, err)
	}
	if _, ok := s.Lookup(raw); ok {
		t.Error("Lookup of a revoked token = ok, want not ok")
	}
	// Revoking again is a no-op (already revoked).
	if ok, _ := s.Revoke(tok.ID, "alice@example.com"); ok {
		t.Error("second Revoke = true, want false (already revoked)")
	}
}

func TestRevokeOnlyByOwner(t *testing.T) {
	s := New(t.TempDir())
	raw, tok, err := s.Mint("laptop", "alice@example.com", nil, Allowance{})
	if err != nil {
		t.Fatal(err)
	}
	// A different principal cannot revoke Alice's token.
	if ok, _ := s.Revoke(tok.ID, "bob@example.com"); ok {
		t.Error("Revoke by a non-owner = true, want false")
	}
	// Alice's token still works.
	if _, ok := s.Lookup(raw); !ok {
		t.Error("token was revoked by a non-owner, want it still valid")
	}
}

func TestListScopedToOwner(t *testing.T) {
	s := New(t.TempDir())
	if _, _, err := s.Mint("a1", "alice@example.com", nil, Allowance{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Mint("a2", "alice@example.com", nil, Allowance{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Mint("b1", "bob@example.com", nil, Allowance{}); err != nil {
		t.Fatal(err)
	}

	alice := s.List("alice@example.com")
	if len(alice) != 2 {
		t.Fatalf("alice's tokens = %d, want 2", len(alice))
	}
	if len(s.List("bob@example.com")) != 1 {
		t.Errorf("bob's tokens = %d, want 1", len(s.List("bob@example.com")))
	}
	if len(s.List("carol@example.com")) != 0 {
		t.Errorf("carol's tokens = %d, want 0", len(s.List("carol@example.com")))
	}
}

func TestAutoShareAndAllowancePersist(t *testing.T) {
	s := New(t.TempDir())
	autoShare := []canvas.Grant{{Kind: canvas.GrantUser, Target: "justin@example.com"}}
	allow := Allowance{Groups: []string{"family"}, Links: true}
	raw, _, err := s.Mint("openclaw", "agent@example.com", autoShare, allow)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := s.Lookup(raw)
	if !ok {
		t.Fatal("Lookup failed")
	}
	if len(got.AutoShare) != 1 || got.AutoShare[0].Target != "justin@example.com" {
		t.Errorf("auto_share = %+v, want the justin grant", got.AutoShare)
	}
	if !got.AllowedGrantTargets.Allows(canvas.GrantGroup, "family") {
		t.Error("allowance lost the family group across persistence")
	}
}

func TestAllowanceAllows(t *testing.T) {
	a := Allowance{Emails: []string{"bob@example.com"}, Groups: []string{"eng"}, Links: true}
	tests := []struct {
		name   string
		kind   string
		target string
		want   bool
	}{
		{"listed email", canvas.GrantUser, "bob@example.com", true},
		{"unlisted email", canvas.GrantUser, "carol@example.com", false},
		{"listed group", canvas.GrantGroup, "eng", true},
		{"unlisted group", canvas.GrantGroup, "finance", false},
		{"everyone not permitted", canvas.GrantEveryone, "", false},
		{"links permitted", canvas.GrantLink, "", true},
		{"unknown kind", "wat", "x", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := a.Allows(tc.kind, tc.target); got != tc.want {
				t.Errorf("Allows(%q, %q) = %v, want %v", tc.kind, tc.target, got, tc.want)
			}
		})
	}

	// The zero allowance permits nothing.
	var zero Allowance
	for _, kind := range []string{canvas.GrantUser, canvas.GrantGroup, canvas.GrantEveryone, canvas.GrantLink} {
		if zero.Allows(kind, "x") {
			t.Errorf("zero Allowance.Allows(%q) = true, want false", kind)
		}
	}
	// Everyone flag gates everyone grants.
	if !(Allowance{Everyone: true}).Allows(canvas.GrantEveryone, "") {
		t.Error("Everyone allowance should permit an everyone grant")
	}
}

func TestCorruptFileToleratedAsEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte("{bad json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(dir)
	if _, ok := s.Lookup("anything"); ok {
		t.Error("Lookup over a corrupt file = ok, want not ok")
	}
	// Mint recovers by overwriting the corrupt file.
	raw, _, err := s.Mint("laptop", "alice@example.com", nil, Allowance{})
	if err != nil {
		t.Fatalf("Mint over a corrupt file error = %v, want recovery", err)
	}
	if _, ok := s.Lookup(raw); !ok {
		t.Error("token not usable after recovery")
	}
}

func TestPersistsAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	raw, _, err := New(dir).Mint("laptop", "alice@example.com", nil, Allowance{})
	if err != nil {
		t.Fatal(err)
	}
	// A fresh Store over the same dir (a hub restart) still resolves the token.
	if _, ok := New(dir).Lookup(raw); !ok {
		t.Error("token not resolvable from a fresh Store, want persistence")
	}
}

func TestConcurrentMintAndLookup(t *testing.T) {
	// Under -race this proves the mutex-guarded whole-file read/write is
	// race-free and never loses a mint to a torn file.
	s := New(t.TempDir())
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			raw, _, err := s.Mint("t", "alice@example.com", nil, Allowance{})
			if err != nil {
				t.Errorf("Mint: %v", err)
				return
			}
			if _, ok := s.Lookup(raw); !ok {
				t.Error("minted token not immediately resolvable")
			}
		}()
	}
	wg.Wait()
	if got := len(s.List("alice@example.com")); got != 20 {
		t.Errorf("minted 20 tokens concurrently, store has %d", got)
	}
}
