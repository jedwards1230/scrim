// Package session is the hub's server-side registry of browser sign-ins: the
// records that make an OIDC session listable on the devices page and
// revocable before its cookie expires. Each login records the session id the
// cookie carries, the principal it authenticates, and the browser's
// User-Agent string; the request gate consults the registry on every
// session-authenticated request, so revoking a record ends that browser's
// access on its very next request.
//
// Expiry SLIDES. A record's ExpiresAt is an idle deadline, not a fixed
// lifetime: Renew pushes it out to now+idleTTL as the session is used,
// clamped to CreatedAt+maxLifetime so a session that is merely kept warm
// still ends. Renewal is throttled by TouchInterval for the same reason the
// LastSeen bump it subsumes always was -- the check runs on every request and
// must not write the file each time.
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

// TouchInterval is how stale a record's LastSeen must be before Renew
// persists a fresh one -- and, with it, the slid expiry. Doing either on every
// request would mean a disk write (and a Set-Cookie) per request; a
// five-minute floor keeps "last seen" honest enough for a devices list, and an
// idle window measured in days is not meaningfully coarsened by rounding its
// renewals to five minutes. Lookup itself never writes at all.
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

	// idleTTL is the SLIDING window: a session that goes unused for this long
	// expires. Renew pushes ExpiresAt out to now+idleTTL as the session is
	// used. maxLifetime is the absolute cap, measured from CreatedAt: renewal
	// can never push a record past it, so a session merely kept warm still
	// ends and its holder re-authenticates. A non-positive maxLifetime means
	// no cap; a non-positive idleTTL disables renewal entirely (expiry stays
	// whatever Create recorded), which is what a Store built by a caller with
	// no policy of its own gets.
	idleTTL     time.Duration
	maxLifetime time.Duration

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
//
// idleTTL and maxLifetime are the renewal policy (see the Store fields): the
// sliding idle window, and the absolute cap from CreatedAt that renewal may
// never cross.
func New(metaDir string, idleTTL, maxLifetime time.Duration) *Store {
	s := &Store{
		path:        filepath.Join(metaDir, fileName),
		now:         time.Now,
		idleTTL:     idleTTL,
		maxLifetime: maxLifetime,
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
	// The absolute cap binds from the very first moment, not just at renewal:
	// an operator who sets an idle window longer than the cap gets the cap,
	// rather than one initial session that outlives every renewed one.
	if limit, ok := s.hardExpiry(rec); ok && rec.ExpiresAt.After(limit) {
		rec.ExpiresAt = limit
	}
	s.prune()
	s.records[rec.ID] = rec
	return s.save()
}

// Lookup returns the live record for id. It is a pure read -- a map lookup and
// nothing else, no disk write -- because it is the revocation check on the hot
// path of every authenticated browser request. An unknown or expired id
// returns (Record{}, false, nil); the fail-closed state returns the load
// error, which callers must treat as "not authenticated" rather than as
// "unknown session". Marking a session used (and sliding its expiry) is
// Renew's job, not Lookup's.
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
	if !s.now().Before(rec.ExpiresAt) {
		return Record{}, false, nil
	}
	return rec, true, nil
}

// Renew marks a live session used and slides its expiry out to now+idleTTL,
// clamped to CreatedAt+maxLifetime. It returns the resulting record, whether
// THIS call moved the expiry (the caller must then re-issue the session cookie
// so the cookie's own signed expiry can't become the binding constraint), and
// the fail-closed error.
//
// Three properties it is required to hold, each with a test:
//
//   - It never resurrects. An id that is unknown, already expired, or revoked
//     is a miss (Record{}, false, nil) and nothing is written -- the liveness
//     check runs BEFORE any extension, so a lapsed session cannot be renewed
//     back into existence.
//   - It never shrinks an expiry, and never pushes one past the absolute cap.
//     A session sitting at the cap keeps being used but stops being extended,
//     and then expires for real.
//   - It is throttled by TouchInterval, exactly like the LastSeen bump it
//     subsumes: inside the window it writes nothing and reports extended=false,
//     so neither the registry file nor a Set-Cookie lands on every request.
//
// The write is best-effort, like usertoken's LastUsed bump: the in-memory view
// is already correct, and the session is valid whether or not the new
// timestamps reached the disk.
func (s *Store) Renew(id string) (Record, bool, error) {
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
	if now.Sub(rec.LastSeen) < TouchInterval {
		return rec, false, nil
	}

	rec.LastSeen = now
	extended := false
	if s.idleTTL > 0 {
		want := now.Add(s.idleTTL)
		if limit, capped := s.hardExpiry(rec); capped && want.After(limit) {
			want = limit
		}
		if want.After(rec.ExpiresAt) {
			rec.ExpiresAt = want
			extended = true
		}
	}
	s.records[id] = rec
	_ = s.save()
	return rec, extended, nil
}

// hardExpiry returns the latest instant rec may ever expire at -- CreatedAt
// plus the absolute cap -- and whether a cap is configured at all. Callers
// hold s.mu.
func (s *Store) hardExpiry(rec Record) (time.Time, bool) {
	if s.maxLifetime <= 0 || rec.CreatedAt.IsZero() {
		return time.Time{}, false
	}
	return rec.CreatedAt.Add(s.maxLifetime), true
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
