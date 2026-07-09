// Package canvas manages canvas directories on disk: validating IDs,
// creating/listing/deleting canvases, and reading/writing per-canvas
// metadata (title, description, and icon). Metadata is deliberately stored
// external to the canvas directory itself -- see MetaFileName's doc comment
// on canvas.Create -- keyed by canvas ID under a separate metadata
// directory the caller provides (config.Config.MetaDir).
package canvas

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// Grant kinds. A canvas is private by default (visible only to its owner);
// each grant on a canvas widens that visibility for one class of principal.
// These are the exact JSON values stored in a Grant.Kind and interpreted by
// the identity package's CanView.
const (
	// GrantUser grants view access to one principal, matched on Grant.Target
	// (their email/principal id).
	GrantUser = "user"
	// GrantGroup grants view access to every principal carrying Grant.Target as
	// one of their groups claim values.
	GrantGroup = "group"
	// GrantEveryone grants view access to any authenticated principal (Target
	// is empty).
	GrantEveryone = "everyone"
	// GrantLink grants view access to any caller presenting the matching share
	// link secret; only the secret's SHA-256 hash is stored (Grant.LinkSecretHash),
	// never the secret itself.
	GrantLink = "link"
)

// Grant is one entry on a canvas's sharing list. It widens the canvas's
// visibility beyond its owner for the class of principal named by Kind. Grants
// are view-only; write access stays owner/admin-only.
type Grant struct {
	Kind           string    `json:"kind"`                       // "user" | "group" | "everyone" | "link"
	Target         string    `json:"target,omitempty"`           // email (user) or group name (group); empty for everyone/link
	LinkID         string    `json:"link_id,omitempty"`          // link kind only: public id in the share URL
	LinkSecretHash string    `json:"link_secret_hash,omitempty"` // link kind only: SHA-256 hex of the link secret
	CreatedBy      string    `json:"created_by,omitempty"`
	CreatedAt      time.Time `json:"created_at,omitempty"`
}

// NewLink mints a fresh share-link identity: a short public link id (safe to
// embed in a share URL) and a high-entropy raw secret (256 bits, base64url).
// Only the secret's hash (see HashLinkSecret) is ever stored on a grant; the
// raw secret is returned here ONCE for the caller to hand back to the sharer.
func NewLink() (linkID, secret string, err error) {
	linkID, err = randB64(9) // 12 base64url chars, ample to avoid collisions
	if err != nil {
		return "", "", err
	}
	secret, err = randB64(32) // 256 bits of entropy
	if err != nil {
		return "", "", err
	}
	return linkID, secret, nil
}

// randB64 returns n cryptographically-random bytes as unpadded base64url.
func randB64(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("canvas: reading randomness: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// HashLinkSecret returns the lowercase-hex SHA-256 of a share-link secret, the
// form stored in Grant.LinkSecretHash. The raw secret is never persisted; a
// caller presenting a ?k=<secret> is checked by hashing it and comparing
// (constant-time) against this value -- see internal/identity.CanView.
func HashLinkSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// idPattern restricts canvas IDs to a safe, portable charset: no path
// separators, no "..", no leading dot. This is what keeps IDs safe to use
// directly as a single path component.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// ErrInvalidID is returned by ValidateID for malformed canvas IDs.
var ErrInvalidID = errors.New("canvas: invalid id")

// ValidateID reports whether id is safe to use as a single path component
// under the canvases directory. It rejects empty strings, path separators,
// leading dots/dashes, and anything outside [A-Za-z0-9_-].
func ValidateID(id string) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("%w: %q (must match %s)", ErrInvalidID, id, idPattern.String())
	}
	return nil
}

// Info describes one canvas.
type Info struct {
	ID          string
	Title       string
	Description string
	// Icon is the canvas's emoji glyph: either an explicit value given at
	// creation time, or a deterministic default derived from ID (see
	// DefaultIcon) when none was given.
	Icon string
	// Color is a deterministic accent color derived from ID (see
	// DefaultColor), used as the icon's swatch background. It is always
	// derived -- there is no way to override it -- so the same ID keeps the
	// same color across restarts regardless of any custom Icon.
	Color   string
	Dir     string
	ModTime time.Time
	// Owner is the canvas's owning principal id (email; "admin" for
	// bootstrap/legacy canvases). Empty when the canvas has no meta file yet --
	// callers treat an empty owner as "admin" for enforcement (see
	// internal/identity).
	Owner string
	// Grants is the canvas's sharing list. Nil/empty means owner-only.
	Grants []Grant
}

// Dir returns the on-disk directory for the given canvas ID under
// canvasesDir. Callers must validate id first.
func Dir(canvasesDir, id string) string {
	return filepath.Join(canvasesDir, id)
}

// Create makes a new canvas directory (idempotent -- an existing directory
// is fine) and records its title/description/icon/owner, if any are given, in
// an external metadata file under metaDir (keyed by id, not a sidecar file
// inside the canvas directory). This is a deliberate v0.2 behavior change
// from the v0.1 ".scrim.json" sidecar: anything under canvasesDir is
// servable and filesystem-watched, and metadata must be neither. It returns
// the canvas's directory.
//
// owner is the canvas's owning principal id (email, or "admin" for
// bootstrap/legacy). An empty owner leaves any existing owner in place rather
// than clearing it, so re-recording title/description on a push never orphans
// an already-owned canvas. Existing grants are always preserved -- Create
// never drops a canvas's sharing state.
func Create(canvasesDir, metaDir, id, title, description, icon, owner string) (string, error) {
	if err := ValidateID(id); err != nil {
		return "", err
	}
	dir := Dir(canvasesDir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // canvas dirs are user-owned working files, not sensitive
		return "", fmt.Errorf("creating canvas dir %s: %w", id, err)
	}
	existing := readMeta(metaDir, id)
	m := meta{
		Title:       title,
		Description: description,
		Icon:        icon,
		Owner:       owner,
		Grants:      existing.Grants,
	}
	if owner == "" {
		m.Owner = existing.Owner
	}
	// Nothing worth persisting (a bare canvas with no metadata, no owner, no
	// grants) leaves no metadata file at all -- matching the pre-owner
	// behavior, so a delete of such a canvas has nothing to remove.
	if m.Title == "" && m.Description == "" && m.Icon == "" && m.Owner == "" && len(m.Grants) == 0 {
		return dir, nil
	}
	if err := writeMeta(metaDir, id, m); err != nil {
		return "", err
	}
	return dir, nil
}

// GetOwnerGrants returns canvas id's owner and grants straight from its
// metadata, tolerating a missing/empty metadata file as owner-only ("" owner,
// no grants). Callers must validate id first is unnecessary -- it validates.
func GetOwnerGrants(metaDir, id string) (owner string, grants []Grant, err error) {
	if err := ValidateID(id); err != nil {
		return "", nil, err
	}
	m := readMeta(metaDir, id)
	return m.Owner, m.Grants, nil
}

// SetOwner writes canvas id's owner, preserving every other metadata field
// (title/description/icon/grants). The write is atomic (temp+rename via
// writeMeta).
func SetOwner(metaDir, id, owner string) error {
	if err := ValidateID(id); err != nil {
		return err
	}
	m := readMeta(metaDir, id)
	m.Owner = owner
	return writeMeta(metaDir, id, m)
}

// AddGrant appends g to canvas id's grants, preserving every other metadata
// field. It does no de-duplication -- the caller (a grant endpoint, #52) is
// responsible for rejecting a duplicate before calling. The write is atomic.
func AddGrant(metaDir, id string, g Grant) error {
	if err := ValidateID(id); err != nil {
		return err
	}
	m := readMeta(metaDir, id)
	m.Grants = append(m.Grants, g)
	return writeMeta(metaDir, id, m)
}

// RemoveGrant drops every grant on canvas id for which match reports true,
// preserving every other metadata field, and returns how many were removed. A
// zero return means no grant matched (the caller can surface a 404). The write
// is atomic and is skipped entirely when nothing matched.
func RemoveGrant(metaDir, id string, match func(Grant) bool) (removed int, err error) {
	if err := ValidateID(id); err != nil {
		return 0, err
	}
	m := readMeta(metaDir, id)
	kept := m.Grants[:0:0]
	for _, g := range m.Grants {
		if match(g) {
			removed++
			continue
		}
		kept = append(kept, g)
	}
	if removed == 0 {
		return 0, nil
	}
	m.Grants = kept
	if err := writeMeta(metaDir, id, m); err != nil {
		return 0, err
	}
	return removed, nil
}

// Delete removes a canvas directory and everything under it, along with its
// external metadata file, if any.
func Delete(canvasesDir, metaDir, id string) error {
	if err := ValidateID(id); err != nil {
		return err
	}
	dir := Dir(canvasesDir, id)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("removing canvas %s: %w", id, err)
	}
	if err := os.Remove(metaPath(metaDir, id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing canvas metadata %s: %w", id, err)
	}
	return nil
}

// Exists reports whether a canvas directory exists for id.
func Exists(canvasesDir, id string) bool {
	if err := ValidateID(id); err != nil {
		return false
	}
	fi, err := os.Stat(Dir(canvasesDir, id))
	return err == nil && fi.IsDir()
}

// Get returns Info for a single canvas.
func Get(canvasesDir, metaDir, id string) (Info, error) {
	if err := ValidateID(id); err != nil {
		return Info{}, err
	}
	dir := Dir(canvasesDir, id)
	fi, err := os.Stat(dir)
	if err != nil {
		return Info{}, fmt.Errorf("stat canvas %s: %w", id, err)
	}
	if !fi.IsDir() {
		return Info{}, fmt.Errorf("canvas %s: not a directory", id)
	}
	return readInfo(canvasesDir, metaDir, id), nil
}

// List returns Info for every canvas under canvasesDir, sorted by ID. A
// missing canvasesDir is treated as "no canvases" rather than an error.
func List(canvasesDir, metaDir string) ([]Info, error) {
	entries, err := os.ReadDir(canvasesDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing canvases dir: %w", err)
	}

	infos := make([]Info, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() || ValidateID(e.Name()) != nil {
			continue
		}
		infos = append(infos, readInfo(canvasesDir, metaDir, e.Name()))
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].ID < infos[j].ID })
	return infos, nil
}

// FileMeta describes one file inside a canvas: its canvas-relative,
// slash-separated path plus size and modification time. It deliberately
// carries NO content -- Files enumerates a canvas cheaply and privately, and
// callers read individual files (read_file / the files GET route) when they
// need bytes.
type FileMeta struct {
	Path       string    `json:"path"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modified_at"`
}

// Files walks canvas id's directory and returns every regular file within it
// as a FileMeta, sorted by path. Paths are canvas-relative and always
// slash-separated (so a hub on Linux and a client on Windows agree). Only
// regular files are reported: directories, symlinks, and other irregular
// entries are skipped -- the same "regular files only" stance push extraction
// and the file GET/PUT routes take, so a listing never exposes a symlink as
// if it were content. A missing canvas directory is an error (the caller
// distinguishes it from an empty canvas); an empty canvas is an empty slice.
func Files(canvasesDir, id string) ([]FileMeta, error) {
	if err := ValidateID(id); err != nil {
		return nil, err
	}
	root := Dir(canvasesDir, id)
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("canvas %s: no such directory", id)
	}

	var out []FileMeta
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Only regular files carry content worth listing; skip the root and
		// nested directories, and skip symlinks/devices/etc. outright.
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, FileMeta{
			Path:       filepath.ToSlash(rel),
			Size:       info.Size(),
			ModifiedAt: info.ModTime(),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing files for canvas %s: %w", id, err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// CopyMeta duplicates canvas from's external metadata onto canvas to. It
// copies the raw metadata FILE, so only authored title/description/icon are
// carried -- a derived default icon stays derived from to's own id rather than
// being baked in from the source (Get would otherwise return the source's
// derived icon, which is the wrong default for a differently-named canvas). If
// from has no metadata file, any existing metadata for to is removed, so an
// overwrite-copy never leaves the previous canvas's metadata behind.
func CopyMeta(metaDir, from, to string) error {
	if err := ValidateID(from); err != nil {
		return err
	}
	if err := ValidateID(to); err != nil {
		return err
	}
	data, err := os.ReadFile(metaPath(metaDir, from))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Source carries no explicit metadata: clear the target's for
			// parity (matters on an overwrite copy).
			if rmErr := os.Remove(metaPath(metaDir, to)); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				return fmt.Errorf("clearing canvas metadata %s: %w", to, rmErr)
			}
			return nil
		}
		return fmt.Errorf("reading canvas metadata %s: %w", from, err)
	}
	if err := os.MkdirAll(metaDir, 0o755); err != nil { //nolint:gosec // metadata dir is user-owned working state, not sensitive
		return fmt.Errorf("creating metadata dir: %w", err)
	}
	if err := os.WriteFile(metaPath(metaDir, to), data, 0o644); err != nil { //nolint:gosec // metadata is not sensitive
		return fmt.Errorf("writing canvas metadata %s: %w", to, err)
	}
	return nil
}

type meta struct {
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	// Icon is only populated here when explicitly given (e.g. `scrim add
	// --icon`); an empty Icon means Info.Icon falls back to DefaultIcon(id).
	Icon string `json:"icon,omitempty"`
	// Owner is the owning principal id (email; "admin" for bootstrap/legacy).
	Owner string `json:"owner,omitempty"`
	// Grants is the canvas's sharing list; empty means owner-only.
	Grants []Grant `json:"grants,omitempty"`
}

// metaPath returns the external metadata file path for id under metaDir.
// Callers must validate id first -- this does no validation of its own.
func metaPath(metaDir, id string) string {
	return filepath.Join(metaDir, id+".json")
}

// writeMeta writes metadata atomically: it writes to a temp file in metaDir
// and renames it into place, mirroring state.Save's pattern, so a concurrent
// reader (readMeta, called from the dashboard/favicon/status handlers) can
// only ever observe the file fully written or not yet renamed -- never a
// partial write that fails to unmarshal and silently degrades to defaults.
func writeMeta(metaDir, id string, m meta) error {
	if err := os.MkdirAll(metaDir, 0o755); err != nil { //nolint:gosec // metadata dir is user-owned working state, not sensitive
		return fmt.Errorf("creating metadata dir: %w", err)
	}
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("encoding canvas metadata: %w", err)
	}

	tmp, err := os.CreateTemp(metaDir, "."+id+"-*.json.tmp")
	if err != nil {
		return fmt.Errorf("creating temp metadata file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }() // no-op once renamed; cleans up on any early return

	if _, err := tmp.Write(data); err != nil {
		tmp.Close() //nolint:errcheck // best-effort close on error path
		return fmt.Errorf("writing temp metadata file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp metadata file: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil { //nolint:gosec // metadata is not sensitive
		return fmt.Errorf("setting metadata file permissions: %w", err)
	}
	if err := os.Rename(tmpPath, metaPath(metaDir, id)); err != nil {
		return fmt.Errorf("renaming metadata file into place: %w", err)
	}
	return nil
}

func readMeta(metaDir, id string) meta {
	data, err := os.ReadFile(metaPath(metaDir, id)) //nolint:gosec // id is validated by every exported entry point before it reaches here
	if err != nil {
		return meta{}
	}
	var m meta
	if err := json.Unmarshal(data, &m); err != nil {
		return meta{}
	}
	return m
}

func readInfo(canvasesDir, metaDir, id string) Info {
	dir := Dir(canvasesDir, id)
	m := readMeta(metaDir, id)
	icon := m.Icon
	if icon == "" {
		icon = DefaultIcon(id)
	}
	return Info{
		ID:          id,
		Title:       m.Title,
		Description: m.Description,
		Icon:        icon,
		Color:       DefaultColor(id),
		Dir:         dir,
		ModTime:     lastModified(dir),
		Owner:       m.Owner,
		Grants:      m.Grants,
	}
}

// lastModified walks dir and returns the most recent modification time among
// its files, falling back to the directory's own mtime if the walk fails or
// the canvas is empty.
func lastModified(dir string) time.Time {
	latest := time.Time{}
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // best-effort walk; skip unreadable entries
		}
		info, err := d.Info()
		if err != nil {
			return nil //nolint:nilerr // best-effort walk; skip unreadable entries
		}
		if info.ModTime().After(latest) {
			latest = info.ModTime()
		}
		return nil
	})
	if latest.IsZero() {
		if fi, err := os.Stat(dir); err == nil {
			latest = fi.ModTime()
		}
	}
	return latest
}
