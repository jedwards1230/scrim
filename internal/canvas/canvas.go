// Package canvas manages canvas directories on disk: validating IDs,
// creating/listing/deleting canvases, and reading/writing per-canvas
// metadata (title, description, and icon). Metadata is deliberately stored
// external to the canvas directory itself -- see MetaFileName's doc comment
// on canvas.Create -- keyed by canvas ID under a separate metadata
// directory the caller provides (config.Config.MetaDir).
package canvas

import (
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
}

// Dir returns the on-disk directory for the given canvas ID under
// canvasesDir. Callers must validate id first.
func Dir(canvasesDir, id string) string {
	return filepath.Join(canvasesDir, id)
}

// Create makes a new canvas directory (idempotent -- an existing directory
// is fine) and records its title/description/icon, if any are given, in an
// external metadata file under metaDir (keyed by id, not a sidecar file
// inside the canvas directory). This is a deliberate v0.2 behavior change
// from the v0.1 ".scrim.json" sidecar: anything under canvasesDir is
// servable and filesystem-watched, and metadata must be neither. It returns
// the canvas's directory.
func Create(canvasesDir, metaDir, id, title, description, icon string) (string, error) {
	if err := ValidateID(id); err != nil {
		return "", err
	}
	dir := Dir(canvasesDir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // canvas dirs are user-owned working files, not sensitive
		return "", fmt.Errorf("creating canvas dir %s: %w", id, err)
	}
	if title != "" || description != "" || icon != "" {
		m := meta{Title: title, Description: description, Icon: icon}
		if err := writeMeta(metaDir, id, m); err != nil {
			return "", err
		}
	}
	return dir, nil
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

type meta struct {
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	// Icon is only populated here when explicitly given (e.g. `scrim add
	// --icon`); an empty Icon means Info.Icon falls back to DefaultIcon(id).
	Icon string `json:"icon,omitempty"`
}

// metaPath returns the external metadata file path for id under metaDir.
// Callers must validate id first -- this does no validation of its own.
func metaPath(metaDir, id string) string {
	return filepath.Join(metaDir, id+".json")
}

func writeMeta(metaDir, id string, m meta) error {
	if err := os.MkdirAll(metaDir, 0o755); err != nil { //nolint:gosec // metadata dir is user-owned working state, not sensitive
		return fmt.Errorf("creating metadata dir: %w", err)
	}
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("encoding canvas metadata: %w", err)
	}
	if err := os.WriteFile(metaPath(metaDir, id), data, 0o644); err != nil { //nolint:gosec // metadata is not sensitive
		return fmt.Errorf("writing canvas metadata: %w", err)
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
