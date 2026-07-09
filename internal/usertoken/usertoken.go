// Package usertoken manages the hub's user-minted bearer tokens: named
// credentials that act AS their owning principal on the direct (machine) plane,
// so a canvas created or written through a token is attributed to that token's
// owner rather than the single anonymous global push token. Tokens live in a
// whole-file JSON document under the meta directory, read and written
// atomically under a mutex; only a token's SHA-256 hash is stored -- the raw
// secret is returned once at mint and never persisted.
//
// The global SCRIM_PUSH_TOKEN is deliberately NOT stored here: it stays the
// hub's admin/bootstrap credential (hubConfig.pushToken), owns legacy canvases
// (owner=="admin"), and has an unrestricted allowance. This package is only the
// per-principal token layer above it.
package usertoken

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/jedwards1230/scrim/internal/canvas"
)

// fileName is the tokens document under the meta directory.
const fileName = "tokens.json"

// rawTokenBytes / idBytes size the crypto/rand secrets: a 32-byte token is
// 256 bits of entropy (base64url), an 16-byte id is a stable public handle.
const (
	rawTokenBytes = 32
	idBytes       = 16
)

// Token is one user-minted credential. Hash is the SHA-256 hex of the raw
// token; the raw value is shown ONCE at mint and never stored.
type Token struct {
	ID                  string         `json:"id"`
	Name                string         `json:"name"`
	OwnerEmail          string         `json:"owner_email"`
	Hash                string         `json:"hash"`
	CreatedAt           time.Time      `json:"created_at"`
	LastUsed            *time.Time     `json:"last_used,omitempty"`
	Revoked             bool           `json:"revoked,omitempty"`
	AutoShare           []canvas.Grant `json:"auto_share,omitempty"`
	AllowedGrantTargets Allowance      `json:"allowed_grant_targets,omitempty"`
}

// Allowance bounds which grant targets a token's owner may share to (enforced
// by the #52 grant endpoints via Allows). Its zero value permits nothing --
// a token must be minted with an explicit allowance to share interactively.
// Admin's allowance is unrestricted and handled at the call site, not here.
type Allowance struct {
	Emails   []string `json:"emails,omitempty"`
	Groups   []string `json:"groups,omitempty"`
	Everyone bool     `json:"everyone,omitempty"`
	Links    bool     `json:"links,omitempty"`
}

// Allows reports whether this allowance permits a grant of the given kind to
// the given target: a user grant only to a listed email, a group grant only to
// a listed group, an everyone grant only when Everyone is set, a link grant
// only when Links is set. An unknown kind is never allowed.
func (a Allowance) Allows(kind, target string) bool {
	switch kind {
	case canvas.GrantUser:
		return containsString(a.Emails, target)
	case canvas.GrantGroup:
		return containsString(a.Groups, target)
	case canvas.GrantEveryone:
		return a.Everyone
	case canvas.GrantLink:
		return a.Links
	default:
		return false
	}
}

// Store is the hub's token store, backed by <metaDir>/tokens.json. Construct it
// with New. It is safe for concurrent use.
type Store struct {
	mu   sync.Mutex
	path string
	now  func() time.Time
}

// New returns a Store backed by <metaDir>/tokens.json. The file is not created
// until the first Mint.
func New(metaDir string) *Store {
	return &Store{
		path: filepath.Join(metaDir, fileName),
		now:  time.Now,
	}
}

// Mint creates a new token owned by ownerEmail, returning the raw secret ONCE
// (it is never recoverable afterward -- only its hash is stored) alongside the
// stored Token metadata. autoShare grants are applied to canvases the token
// creates; allowance bounds what it may later share interactively.
func (s *Store) Mint(name, ownerEmail string, autoShare []canvas.Grant, allowance Allowance) (raw string, tok Token, err error) {
	if ownerEmail == "" {
		return "", Token{}, fmt.Errorf("usertoken: owner email is required")
	}
	raw, err = randToken(rawTokenBytes)
	if err != nil {
		return "", Token{}, err
	}
	id, err := randToken(idBytes)
	if err != nil {
		return "", Token{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tokens := s.load()
	tok = Token{
		ID:                  id,
		Name:                name,
		OwnerEmail:          ownerEmail,
		Hash:                hashToken(raw),
		CreatedAt:           s.now(),
		AutoShare:           autoShare,
		AllowedGrantTargets: allowance,
	}
	tokens = append(tokens, tok)
	if err := s.save(tokens); err != nil {
		return "", Token{}, err
	}
	return raw, tok, nil
}

// Lookup returns the non-revoked token matching raw, comparing hashes in
// constant time across every stored token (so a caller can't time-probe which
// token matched), and best-effort bumps its LastUsed. A miss returns
// (nil, false).
func (s *Store) Lookup(raw string) (*Token, bool) {
	if raw == "" {
		return nil, false
	}
	want := hashToken(raw)

	s.mu.Lock()
	defer s.mu.Unlock()

	tokens := s.load()
	matched := -1
	for i := range tokens {
		if tokens[i].Revoked {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(tokens[i].Hash), []byte(want)) == 1 {
			matched = i
			// No early break: iterating every token keeps the match position
			// from being observable via timing.
		}
	}
	if matched < 0 {
		return nil, false
	}

	now := s.now()
	tokens[matched].LastUsed = &now
	// Best-effort: a failed LastUsed write must not fail the lookup (the token
	// is valid regardless). The returned copy still reflects the bump.
	_ = s.save(tokens)

	out := tokens[matched]
	return &out, true
}

// Get returns the token with the given id (revoked or not), for a
// revoke-by-admin path. A miss returns (nil, false).
func (s *Store) Get(id string) (*Token, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.load() {
		if t.ID == id {
			out := t
			return &out, true
		}
	}
	return nil, false
}

// List returns ownerEmail's tokens (revoked ones included, so a UI can show
// their state), sorted newest-first. Each returned token still carries its
// Hash; callers that serialize to an API response must strip it.
func (s *Store) List(ownerEmail string) []Token {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Token
	for _, t := range s.load() {
		if t.OwnerEmail == ownerEmail {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// Revoke marks the token with the given id revoked, but only when it is owned
// by ownerEmail -- so a principal can revoke only its own tokens. It reports
// whether a matching, not-already-revoked token was found and revoked. An admin
// path revokes any token by first resolving its owner via Get.
func (s *Store) Revoke(id, ownerEmail string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tokens := s.load()
	for i := range tokens {
		if tokens[i].ID == id && tokens[i].OwnerEmail == ownerEmail {
			if tokens[i].Revoked {
				return false, nil
			}
			tokens[i].Revoked = true
			if err := s.save(tokens); err != nil {
				return false, err
			}
			return true, nil
		}
	}
	return false, nil
}

// load reads the tokens file, tolerating a missing or corrupt file as empty.
// Callers hold s.mu.
func (s *Store) load() []Token {
	data, err := os.ReadFile(s.path) //nolint:gosec // path is a fixed file under the hub's owner-only meta dir
	if err != nil {
		return nil
	}
	var tokens []Token
	if err := json.Unmarshal(data, &tokens); err != nil {
		// A corrupt file is treated as empty rather than propagated: the store
		// must not wedge on a torn/hand-edited file. The next successful Mint
		// overwrites it.
		return nil
	}
	return tokens
}

// save writes tokens atomically (temp file + rename), mirroring
// canvas.writeMeta so a concurrent reader only ever sees a fully-written file.
// Callers hold s.mu.
func (s *Store) save(tokens []Token) error {
	data, err := json.Marshal(tokens)
	if err != nil {
		return fmt.Errorf("encoding token store: %w", err)
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // meta dir is user-owned working state
		return fmt.Errorf("creating meta dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".tokens-*.json.tmp")
	if err != nil {
		return fmt.Errorf("creating temp token file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }() // no-op once renamed; cleans up on any early return

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing temp token file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp token file: %w", err)
	}
	// Tokens store secret hashes -- keep the file owner-only (0600), unlike the
	// non-sensitive canvas/principal sidecars.
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return fmt.Errorf("setting token file permissions: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("renaming token file into place: %w", err)
	}
	return nil
}

// hashToken returns the lowercase-hex SHA-256 of a raw token.
func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// randToken returns n cryptographically-random bytes, base64url (unpadded).
func randToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("usertoken: reading randomness: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
