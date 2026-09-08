// Package session is the hub's server-side registry of browser sign-ins: the
// records that make an OIDC session listable on the devices page and
// revocable before its cookie expires. Each login records the session id the
// cookie carries, the principal it authenticates, and the browser's
// User-Agent string; the request gate consults the registry on every
// session-authenticated request, so revoking a record ends that browser's
// access on its very next request.
//
// Shape and storage mirror internal/usertoken deliberately: a whole-file JSON
// document under the meta directory, written atomically (temp file + rename)
// under a mutex. The hub is single-replica by design (an RWO volume, a
// Recreate rollout, canvases served from local files), so a single process
// owning the file is the whole concurrency model -- with one addition
// usertoken does not need: the parsed records are held IN MEMORY and written
// through on mutation, because the revocation check runs on every
// authenticated browser request and must not touch the disk each time.
//
// It FAILS CLOSED. A file that exists but cannot be read or parsed leaves the
// store in an error state and every operation returns that error, so the gate
// refuses session-authenticated requests rather than silently forgetting which
// sessions were revoked. A MISSING file is not a failure -- it is an empty
// store, which is exactly the state a hub boots into before anyone has ever
// logged in. (Recovery from the error state is the admin push token, which
// never consults this store at all, plus a hub restart once the file is
// repaired or removed.)
//
// Errors are deliberately path-free, like internal/fileedit's.
package session

import (
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

// fileName is the sessions document under the meta directory.
const fileName = "sessions.json"

// TouchInterval is how stale a record's LastSeen must be before Lookup
// persists a fresh one. Bumping it on every request would mean a disk write
// per request; a five-minute floor keeps "last seen" honest enough for a
// devices list while leaving the hot path allocation-cheap and write-free.
const TouchInterval = 5 * time.Minute

// errUnreadable / errCorrupt are the two fail-closed states. They are
// deliberately path-free and carry no file content.
var (
	errUnreadable = errors.New("session store: registry file exists but cannot be read")
	errCorrupt    = errors.New("session store: registry file is not valid JSON")
)

// Record is one browser sign-in. ID is the opaque session id minted at login
// and carried inside the signed session cookie -- it is not a credential on
// its own (the cookie's HMAC is), which is why it is safe to list and to
// address in a URL.
type Record struct {
	ID      string `json:"id"`
	Subject string `json:"sub"`
	Email   string `json:"email,omitempty"`
	// UserAgent is the browser's User-Agent string, recorded verbatim so the
	// UI can derive a device label from it. It is the ONLY request-derived
	// datum kept: no IP address is stored, ever.
	UserAgent string    `json:"user_agent,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	LastSeen  time.Time `json:"last_seen"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Store is the hub's session registry, backed by <metaDir>/sessions.json and
// cached in memory. Construct it with New. It is safe for concurrent use.
type Store struct {
	mu   sync.Mutex
	path string
	now  func() time.Time

	// records is the authoritative in-memory view, keyed by session id, valid
	// only while loadErr is nil.
	records map[string]Record
	// loadErr is non-nil when the on-disk file exists but could not be read or
	// parsed. Every operation then fails, and the gate treats that as "not
	// authenticated" -- fail closed.
	loadErr error
}

// New returns a Store backed by <metaDir>/sessions.json, loading it
// immediately: a missing file yields an empty store, an unreadable or corrupt
// one puts the store in its fail-closed error state (see Err). Expired records
// are pruned as part of the load, and the pruned set is written back
// best-effort. The file is not created until the first Create.
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
		// refuse logins -- the in-memory view is already correct.
		_ = s.save()
	}
	return s
}

// Err reports the store's fail-closed state: nil when the registry loaded (or
// was absent), otherwise the reason every operation is failing. The hub logs
// it once at startup rather than per request.
func (s *Store) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadErr
}

// Create records a new sign-in. rec.ID, rec.Subject, and rec.ExpiresAt are
// required; CreatedAt and LastSeen default to now when zero. It fails when the
// store is in its fail-closed state, so a hub that cannot read its registry
// mints no further sessions either.
func (s *Store) Create(rec Record) error {
	if rec.ID == "" {
		return errors.New("session store: session id is required")
	}
	if rec.Subject == "" {
		return errors.New("session store: subject is required")
	}
	if rec.ExpiresAt.IsZero() {
		return errors.New("session store: expiry is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return s.loadErr
	}

	now := s.now()
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = now
	}
	if rec.LastSeen.IsZero() {
		rec.LastSeen = rec.CreatedAt
	}
	s.prune()
	s.records[rec.ID] = rec
	return s.save()
}

// Lookup returns the live record for id, throttling a LastSeen bump through
// TouchInterval so the common case is a map read and nothing else. An unknown
// or expired id returns (Record{}, false, nil); the fail-closed state returns
// the load error, which callers must treat as "not authenticated" rather than
// as "unknown session".
func (s *Store) Lookup(id string) (Record, bool, error) {
	if id == "" {
		return Record{}, false, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return Record{}, false, s.loadErr
	}

	rec, ok := s.records[id]
	if !ok {
		return Record{}, false, nil
	}
	now := s.now()
	if !now.Before(rec.ExpiresAt) {
		return Record{}, false, nil
	}
	if now.Sub(rec.LastSeen) >= TouchInterval {
		rec.LastSeen = now
		s.records[id] = rec
		// Best-effort, exactly like usertoken's LastUsed bump: the session is
		// valid whether or not the timestamp reached the disk.
		_ = s.save()
	}
	return rec, true, nil
}

// List returns subject's live (unexpired) sessions, newest first. An empty
// subject matches nothing -- a caller with no subject (the admin push token,
// or a user-token principal) has no browser sessions of its own to list, and
// must never be handed someone else's.
func (s *Store) List(subject string) ([]Record, error) {
	if subject == "" {
		return nil, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return nil, s.loadErr
	}

	now := s.now()
	var out []Record
	for _, rec := range s.records {
		if rec.Subject != subject || !now.Before(rec.ExpiresAt) {
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// Revoke deletes the session with the given id, but only when it belongs to
// subject -- so a principal can end only its own sign-ins. It reports whether
// a matching live session was found; a miss is not an error, so the caller can
// answer 404 without revealing whether the id exists under another principal.
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
	if !ok || rec.Subject != subject || !s.now().Before(rec.ExpiresAt) {
		return false, nil
	}
	delete(s.records, id)
	if err := s.save(); err != nil {
		return false, err
	}
	return true, nil
}

// End deletes the session with the given id unconditionally -- the logout path,
// where the caller has already presented that session's own valid cookie, so
// there is no ownership left to check. A miss is a no-op.
func (s *Store) End(id string) error {
	if id == "" {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return s.loadErr
	}

	if _, ok := s.records[id]; !ok {
		return nil
	}
	delete(s.records, id)
	return s.save()
}

// load reads the registry into memory. A missing file is an EMPTY store, not a
// failure: that is a hub's first boot, and treating it as a failure would lock
// every user out permanently. Anything else -- unreadable, or unparseable --
// sets loadErr and fails every subsequent operation closed. Callers hold s.mu.
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
		if rec.ID == "" {
			continue
		}
		s.records[rec.ID] = rec
	}
}

// prune drops expired records from the in-memory view and returns how many it
// removed. Callers hold s.mu and are responsible for persisting the result.
func (s *Store) prune() int {
	now := s.now()
	removed := 0
	for id, rec := range s.records {
		if !now.Before(rec.ExpiresAt) {
			delete(s.records, id)
			removed++
		}
	}
	return removed
}

// save writes the in-memory records atomically (temp file + rename), pruning
// expired ones on the way out so the file never accumulates dead sessions.
// The atomic rename is what makes the fail-closed read safe: a reader sees
// either the whole previous file or the whole new one, never a torn write that
// would wedge the store. Callers hold s.mu.
func (s *Store) save() error {
	s.prune()

	recs := make([]Record, 0, len(s.records))
	for _, rec := range s.records {
		recs = append(recs, rec)
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].CreatedAt.Before(recs[j].CreatedAt) })

	data, err := json.Marshal(recs)
	if err != nil {
		return fmt.Errorf("session store: encoding registry: %w", err)
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // meta dir is user-owned working state
		return errors.New("session store: creating meta dir failed")
	}
	tmp, err := os.CreateTemp(dir, ".sessions-*.json.tmp")
	if err != nil {
		return errors.New("session store: creating temp registry file failed")
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }() // no-op once renamed; cleans up on any early return

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return errors.New("session store: writing temp registry file failed")
	}
	if err := tmp.Close(); err != nil {
		return errors.New("session store: closing temp registry file failed")
	}
	// Session ids are not credentials on their own, but the registry still
	// names every principal signed in -- keep it owner-only like the token
	// store, not world-readable like the canvas sidecars.
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return errors.New("session store: setting registry file permissions failed")
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return errors.New("session store: renaming registry file into place failed")
	}
	return nil
}
