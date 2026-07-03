// Package canvas manages canvas directories on disk: validating IDs,
// creating/listing/deleting canvases, and reading/writing per-canvas
// metadata (currently just an optional title).
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

// MetaFileName is the per-canvas metadata sidecar file. It is deliberately
// dotfile-prefixed so the server's static file handler never serves it.
const MetaFileName = ".scrim.json"

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
	ID      string
	Title   string
	Dir     string
	ModTime time.Time
}

// Dir returns the on-disk directory for the given canvas ID under
// canvasesDir. Callers must validate id first.
func Dir(canvasesDir, id string) string {
	return filepath.Join(canvasesDir, id)
}

// Create makes a new canvas directory (idempotent — an existing directory is
// fine) and records its title, if given. It returns the canvas's directory.
func Create(canvasesDir, id, title string) (string, error) {
	if err := ValidateID(id); err != nil {
		return "", err
	}
	dir := Dir(canvasesDir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // canvas dirs are user-owned working files, not sensitive
		return "", fmt.Errorf("creating canvas dir %s: %w", id, err)
	}
	if title != "" {
		if err := writeMeta(dir, meta{Title: title}); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// Delete removes a canvas directory and everything under it.
func Delete(canvasesDir, id string) error {
	if err := ValidateID(id); err != nil {
		return err
	}
	dir := Dir(canvasesDir, id)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("removing canvas %s: %w", id, err)
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
func Get(canvasesDir, id string) (Info, error) {
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
	return readInfo(dir, id)
}

// List returns Info for every canvas under canvasesDir, sorted by ID. A
// missing canvasesDir is treated as "no canvases" rather than an error.
func List(canvasesDir string) ([]Info, error) {
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
		info, err := readInfo(filepath.Join(canvasesDir, e.Name()), e.Name())
		if err != nil {
			continue // skip unreadable canvas rather than failing the whole list
		}
		infos = append(infos, info)
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].ID < infos[j].ID })
	return infos, nil
}

type meta struct {
	Title string `json:"title"`
}

func writeMeta(dir string, m meta) error {
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("encoding canvas metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, MetaFileName), data, 0o644); err != nil { //nolint:gosec // metadata sidecar is not sensitive
		return fmt.Errorf("writing canvas metadata: %w", err)
	}
	return nil
}

func readMeta(dir string) meta {
	data, err := os.ReadFile(filepath.Join(dir, MetaFileName)) //nolint:gosec // dir is derived from a validated canvas ID under the configured canvases dir
	if err != nil {
		return meta{}
	}
	var m meta
	if err := json.Unmarshal(data, &m); err != nil {
		return meta{}
	}
	return m
}

func readInfo(dir, id string) (Info, error) {
	m := readMeta(dir)
	modTime := lastModified(dir)
	return Info{ID: id, Title: m.Title, Dir: dir, ModTime: modTime}, nil
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
