// Package principal maintains the hub's lazily-populated registry of the
// principals it has seen -- from logins, verified CF identity headers, and
// grant targets. It exists purely for display and autocomplete (a future
// share UI listing who a canvas can be shared with); enforcement NEVER reads
// it. The registry is a single whole-file JSON document under the meta
// directory, read and written atomically under a mutex, and a missing or
// corrupt file is tolerated as an empty registry -- a feeder that can't
// persist must never break the request that fed it.
package principal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Principal is one identity the hub has observed. It carries no secret -- just
// enough for a share UI to show and autocomplete who a canvas can be shared
// with.
type Principal struct {
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name,omitempty"`
	GroupsSeen  []string  `json:"groups_seen,omitempty"`
	FirstSeen   time.Time `json:"first_seen"`
	LastSeen    time.Time `json:"last_seen"`
	Source      string    `json:"source"` // "login" | "cf-header" | "grant-target"
}

// Sources for Observe's source argument.
const (
	SourceLogin       = "login"
	SourceCFHeader    = "cf-header"
	SourceGrantTarget = "grant-target"
)

// fileName is the registry's single JSON document under the meta directory.
const fileName = "principals.json"

// Registry is the lazily-populated principal store. Construct it with New. It
// is safe for concurrent use.
type Registry struct {
	mu   sync.Mutex
	path string
	now  func() time.Time
}

// New returns a Registry backed by <metaDir>/principals.json. The file is not
// created until the first Observe that has something to persist.
func New(metaDir string) *Registry {
	return &Registry{
		path: filepath.Join(metaDir, fileName),
		now:  time.Now,
	}
}

// Observe upserts the principal identified by email: it merges any new groups,
// updates the display name (when a non-empty one is given), bumps LastSeen, and
// records FirstSeen on the first sighting. An empty email is ignored (there is
// nothing to key on). It returns an error only if the registry could not be
// persisted; callers feed the registry best-effort and typically log-and-ignore
// -- a failed write must never fail the login/request that triggered it.
func (r *Registry) Observe(email, displayName string, groups []string, source string) error {
	if email == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	principals := r.load()
	now := r.now()
	p, ok := principals[email]
	if !ok {
		p = Principal{Email: email, FirstSeen: now, Source: source}
	}
	if displayName != "" {
		p.DisplayName = displayName
	}
	p.GroupsSeen = mergeGroups(p.GroupsSeen, groups)
	p.LastSeen = now
	principals[email] = p

	return r.save(principals)
}

// List returns every observed principal, sorted by email, for a share UI /
// autocomplete. It tolerates a missing or corrupt file as an empty list.
func (r *Registry) List() []Principal {
	r.mu.Lock()
	defer r.mu.Unlock()

	principals := r.load()
	out := make([]Principal, 0, len(principals))
	for _, p := range principals {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Email < out[j].Email })
	return out
}

// load reads the registry file into a map keyed by email, tolerating a
// missing or corrupt file as empty. Callers hold r.mu.
func (r *Registry) load() map[string]Principal {
	data, err := os.ReadFile(r.path) //nolint:gosec // path is a fixed file under the hub's owner-only meta dir
	if err != nil {
		return map[string]Principal{}
	}
	var list []Principal
	if err := json.Unmarshal(data, &list); err != nil {
		// A corrupt file is treated as empty rather than propagated: this is a
		// display-only feeder, and a torn/hand-edited file must not wedge it.
		return map[string]Principal{}
	}
	m := make(map[string]Principal, len(list))
	for _, p := range list {
		if p.Email != "" {
			m[p.Email] = p
		}
	}
	return m
}

// save writes principals to the registry file atomically (temp file in the
// same directory + rename), mirroring canvas.writeMeta so a concurrent reader
// only ever sees a fully-written file. Callers hold r.mu.
func (r *Registry) save(principals map[string]Principal) error {
	list := make([]Principal, 0, len(principals))
	for _, p := range principals {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Email < list[j].Email })

	data, err := json.Marshal(list)
	if err != nil {
		return fmt.Errorf("encoding principal registry: %w", err)
	}

	dir := filepath.Dir(r.path)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // meta dir is user-owned working state, not sensitive
		return fmt.Errorf("creating meta dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".principals-*.json.tmp")
	if err != nil {
		return fmt.Errorf("creating temp registry file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }() // no-op once renamed; cleans up on any early return

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing temp registry file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp registry file: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil { //nolint:gosec // registry holds no secrets
		return fmt.Errorf("setting registry file permissions: %w", err)
	}
	if err := os.Rename(tmpPath, r.path); err != nil {
		return fmt.Errorf("renaming registry file into place: %w", err)
	}
	return nil
}

// mergeGroups returns existing with any new groups appended, de-duplicated and
// order-preserving (existing first). Empty group names are skipped.
func mergeGroups(existing, incoming []string) []string {
	seen := make(map[string]struct{}, len(existing)+len(incoming))
	out := make([]string, 0, len(existing)+len(incoming))
	for _, g := range existing {
		if g == "" {
			continue
		}
		if _, ok := seen[g]; !ok {
			seen[g] = struct{}{}
			out = append(out, g)
		}
	}
	for _, g := range incoming {
		if g == "" {
			continue
		}
		if _, ok := seen[g]; !ok {
			seen[g] = struct{}{}
			out = append(out, g)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
