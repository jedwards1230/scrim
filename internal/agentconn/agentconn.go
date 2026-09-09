// Package agentconn is the hub's server-side registry of AGENT CONNECTIONS:
// the MCP clients that reach a principal's canvases through scrim mcp's OAuth
// plane. Those requests arrive at the hub as the admin push token plus verified
// X-Scrim-Actor-* headers, so before this package existed they resolved into
// claims and vanished -- an agent holding a live refresh token had ongoing
// access and appeared nowhere on the page whose whole job is "what has access".
//
// A record is keyed by the pair (IdP subject, OAuth client id): one row per
// "this agent, acting for this person". It carries first-seen/last-seen and a
// revocation instant, and the hub's gate consults it on every forwarded-actor
// request, so revoking a connection blocks that client on its very next call.
//
// Shape and storage mirror internal/session deliberately (which in turn mirrors
// internal/usertoken): a whole-file JSON document under the meta directory,
// written atomically (temp file + rename) under a mutex, with the parsed
// records held IN MEMORY and written through on mutation because the check runs
// on a hot path. LastSeen is persisted at most once per TouchInterval for the
// same reason.
//
// It FAILS CLOSED, on exactly the same distinction session draws: a file that
// exists but cannot be read or parsed leaves the store in an error state and
// every operation returns that error, so the gate refuses forwarded-actor
// requests rather than silently forgetting which agents were revoked. A MISSING
// file is not a failure -- it is an empty store, which is what a hub boots into
// before any agent has ever connected. (Recovery is the bare admin push token,
// which carries no actor headers and never consults this store at all.)
//
// What revocation does NOT do is delete anything at the identity provider. The
// client keeps its refresh token; re-authorizing mints a token whose `iat` is
// after the revocation, and Admit lets that one through (and clears the stale
// revocation). That is deliberate -- see Admit -- and the devices page says so
// in as many words.
//
// Errors are deliberately path-free, like internal/session's.
package agentconn

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// fileName is the agent-connection document under the meta directory.
const fileName = "agent-connections.json"

// TouchInterval is how stale a record's LastSeen must be before Admit persists
// a fresh one -- the same five-minute floor internal/session uses, for the same
// reason: the check runs per request and must not cost a disk write each time.
const TouchInterval = 5 * time.Minute

// Retention is how long an unused, unrevoked connection is kept before it is
// pruned. A REVOKED record is never pruned, whatever its age: dropping it would
// silently un-revoke the client, which is the one outcome this store exists to
// prevent.
const Retention = 90 * 24 * time.Hour

// errUnreadable / errCorrupt are the two fail-closed states. They are
// deliberately path-free and carry no file content.
var (
	errUnreadable = errors.New("agent connection store: registry file exists but cannot be read")
	errCorrupt    = errors.New("agent connection store: registry file is not valid JSON")
)

// Record is one agent connection: an OAuth client acting for one principal.
// ID is derived deterministically from Subject+ClientID (see RecordID), so the
// pair has exactly one row no matter how many tokens the client rotates
// through, and the id is stable enough to address in a URL. It is not a
// credential and carries no token material.
type Record struct {
	ID       string `json:"id"`
	Subject  string `json:"sub"`
	ClientID string `json:"client_id,omitempty"`
	Email    string `json:"email,omitempty"`

	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	// RevokedAt is the instant the principal revoked this connection, or the
	// zero time while it is live. It is compared against a presented token's
	// `iat`, so it is retained rather than deleted: a revocation with no
	// timestamp could not tell an old token from a re-authorized one.
	RevokedAt time.Time `json:"revoked_at,omitempty"`
}

// Revoked reports whether this connection has been revoked.
func (r Record) Revoked() bool { return !r.RevokedAt.IsZero() }

// RecordID is the deterministic id for a (subject, client id) pair: a truncated
// SHA-256 of the two, domain-separated. Deterministic rather than random so the
// same agent+person always lands on the same row (the hot path derives the key
// from the request, never a lookup), and hashed rather than concatenated so the
// id in a URL doesn't spell out the subject.
func RecordID(subject, clientID string) string {
	sum := sha256.Sum256([]byte("scrim-agent-connection-v1\n" + subject + "\n" + clientID))
	return hex.EncodeToString(sum[:16])
}

// Store is the hub's agent-connection registry, backed by
// <metaDir>/agent-connections.json and cached in memory. Construct it with New.
// It is safe for concurrent use.
type Store struct {
	mu   sync.Mutex
	path string
	now  func() time.Time

	// records is the authoritative in-memory view, keyed by RecordID, valid only
	// while loadErr is nil.
	records map[string]Record
	// loadErr is non-nil when the on-disk file exists but could not be read or
	// parsed. Every operation then fails, and the gate refuses the request --
	// fail closed.
	loadErr error
}

// New returns a Store backed by <metaDir>/agent-connections.json, loading it
// immediately: a missing file yields an empty store, an unreadable or corrupt
// one puts the store in its fail-closed error state (see Err). Stale unrevoked
// records are pruned as part of the load and written back best-effort. The file
// is not created until the first connection is recorded.
func New(metaDir string) *Store {
	s := &Store{
		path: filepath.Join(metaDir, fileName),
		now:  time.Now,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	if s.loadErr == nil && s.prune() > 0 {
		// Best-effort: a startup prune that can't be written is not a reason to
		// refuse traffic -- the in-memory view is already correct.
		_ = s.save()
	}
	return s
}

// Err reports the store's fail-closed state: nil when the registry loaded (or
// was absent), otherwise the reason every operation is failing. The hub logs it
// once at startup rather than per request.
func (s *Store) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadErr
}

// Admit is the whole enforcement surface: it decides whether a forwarded-actor
// request may proceed and records the connection in the same pass. subject is
// the actor's IdP subject, clientID the OAuth client id (`azp`, falling back to
// `client_id`) the token was minted for, and issuedAt the token's `iat`.
//
// The rules, in order:
//
//   - An EMPTY subject is admitted and records nothing: there is no principal to
//     key a row on, and therefore no revocation that could ever apply to it.
//   - A record with RevokedAt set BLOCKS the request, unless issuedAt is strictly
//     after RevokedAt -- a token minted after the revocation means the principal
//     re-authorized the client at the IdP, which is exactly how access is meant
//     to come back. Admitting it also CLEARS the revocation, so the devices page
//     doesn't keep calling a working connection revoked.
//   - A MISSING client id or a MISSING iat can never satisfy that exemption, so a
//     matching revocation blocks unconditionally. This is the HMAC
//     forwarded-identity plane (no JWT, hence no client id and no iat) and any
//     token without an `iat`: with no proof the credential is newer than the
//     revocation, the fail-closed answer is the only one that keeps a revocation
//     from being silently unenforceable. A clientID-less caller keys on the
//     (subject, "") row, so revoking that row blocks that whole plane for the
//     principal.
//
// A store in its fail-closed state returns its load error and admits nothing.
// The bare admin push token never reaches here (it carries no actor), which is
// what keeps it the recovery path when this registry is broken.
func (s *Store) Admit(subject, clientID, email string, issuedAt time.Time) (bool, error) {
	if subject == "" {
		return true, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return false, s.loadErr
	}

	now := s.now()
	id := RecordID(subject, clientID)
	rec, ok := s.records[id]
	if !ok {
		s.records[id] = Record{
			ID:        id,
			Subject:   subject,
			ClientID:  clientID,
			Email:     email,
			FirstSeen: now,
			LastSeen:  now,
		}
		// Best-effort persistence, like session's LastSeen bump: the connection is
		// admitted whether or not the row reached the disk. A lost row costs a
		// devices-page entry, never an authorization decision (an absent record
		// admits by definition).
		_ = s.save()
		return true, nil
	}

	changed := false
	if rec.Revoked() {
		if clientID == "" || issuedAt.IsZero() || !issuedAt.After(rec.RevokedAt) {
			return false, nil
		}
		// Re-authorized: the presented token postdates the revocation, so the
		// connection is live again. FirstSeen stays -- it is the same connection.
		rec.RevokedAt = time.Time{}
		changed = true
	}
	if email != "" && rec.Email != email {
		rec.Email = email
		changed = true
	}
	// A real change is written immediately; a plain LastSeen bump waits for
	// TouchInterval so the hot path stays write-free.
	if changed || now.Sub(rec.LastSeen) >= TouchInterval {
		rec.LastSeen = now
		s.records[id] = rec
		_ = s.save()
	}
	return true, nil
}

// List returns subject's agent connections, newest first, including revoked
// ones (the page shows them as history, the same way it shows revoked tokens).
// An empty subject matches nothing -- a caller with no subject of its own must
// never be handed someone else's.
func (s *Store) List(subject string) ([]Record, error) {
	if subject == "" {
		return nil, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return nil, s.loadErr
	}

	var out []Record
	for _, rec := range s.records {
		if rec.Subject != subject {
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FirstSeen.After(out[j].FirstSeen) })
	return out, nil
}

// Revoke marks the connection with the given id revoked, but only when it
// belongs to subject -- so a principal can cut off only its own agents. It
// reports whether a matching LIVE connection was found; an unknown id, another
// principal's, or one already revoked is a miss, so the caller answers 404
// without revealing whether the id exists elsewhere.
func (s *Store) Revoke(id, subject string) (bool, error) {
	if id == "" || subject == "" {
		return false, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return false, s.loadErr
	}

	rec, ok := s.records[id]
	if !ok || rec.Subject != subject || rec.Revoked() {
		return false, nil
	}
	rec.RevokedAt = s.now()
	s.records[id] = rec
	if err := s.save(); err != nil {
		return false, err
	}
	return true, nil
}

// load reads the registry into memory. A missing file is an EMPTY store, not a
// failure: that is a hub that has never seen an agent, and treating it as a
// failure would refuse every agent call permanently. Anything else --
// unreadable, or unparseable -- sets loadErr and fails every subsequent
// operation closed. Callers hold s.mu.
func (s *Store) load() {
	s.records = make(map[string]Record)
	s.loadErr = nil

	data, err := os.ReadFile(s.path) //nolint:gosec // path is a fixed file under the hub's owner-only meta dir
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return
		}
		s.loadErr = errUnreadable
		return
	}
	var recs []Record
	if err := json.Unmarshal(data, &recs); err != nil {
		s.loadErr = errCorrupt
		return
	}
	for _, rec := range recs {
		if rec.ID == "" || rec.Subject == "" {
			continue
		}
		s.records[rec.ID] = rec
	}
}

// prune drops stale UNREVOKED records (nothing seen for Retention) from the
// in-memory view and returns how many it removed. A revoked record is kept
// forever: pruning it would un-revoke the client. Callers hold s.mu and are
// responsible for persisting the result.
func (s *Store) prune() int {
	cutoff := s.now().Add(-Retention)
	removed := 0
	for id, rec := range s.records {
		if rec.Revoked() || !rec.LastSeen.Before(cutoff) {
			continue
		}
		delete(s.records, id)
		removed++
	}
	return removed
}

// save writes the in-memory records atomically (temp file + rename), pruning
// stale ones on the way out. The atomic rename is what makes the fail-closed
// read safe: a reader sees either the whole previous file or the whole new one,
// never a torn write that would wedge the store. Callers hold s.mu.
func (s *Store) save() error {
	s.prune()

	recs := make([]Record, 0, len(s.records))
	for _, rec := range s.records {
		recs = append(recs, rec)
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].FirstSeen.Before(recs[j].FirstSeen) })

	data, err := json.Marshal(recs)
	if err != nil {
		return fmt.Errorf("agent connection store: encoding registry: %w", err)
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // meta dir is user-owned working state
		return errors.New("agent connection store: creating meta dir failed")
	}
	tmp, err := os.CreateTemp(dir, ".agent-connections-*.json.tmp")
	if err != nil {
		return errors.New("agent connection store: creating temp registry file failed")
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }() // no-op once renamed; cleans up on any early return

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return errors.New("agent connection store: writing temp registry file failed")
	}
	if err := tmp.Close(); err != nil {
		return errors.New("agent connection store: closing temp registry file failed")
	}
	// The registry names every principal an agent has acted for -- keep it
	// owner-only like the token and session stores.
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return errors.New("agent connection store: setting registry file permissions failed")
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return errors.New("agent connection store: renaming registry file into place failed")
	}
	return nil
}
