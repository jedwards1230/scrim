// Package snapshot implements scrim's canvas versioning: copying a canvas
// directory's current contents into a timestamped directory (`scrim snap`),
// listing those snapshots (`scrim snaps`), and restoring one back onto the
// canvas directory (`scrim revert`). All of it is a pure filesystem
// operation against a caller-provided canvas directory and versions
// directory (config.Config.VersionsDir) -- none of it talks to the daemon.
package snapshot

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jedwards1230/scrim/internal/canvas"
)

// timestampLayout is the sortable, filesystem-safe timestamp format used to
// name snapshot directories. Every reference-time token in it expands to a
// fixed width (including the nanosecond fraction, thanks to the trailing
// zeros rather than nines), so the formatted string always has the same
// length and sorts lexicographically in the same order as chronologically --
// which is also what makes two snapshots taken back-to-back land in
// distinct directories without any extra collision handling.
const timestampLayout = "20060102-150405.000000000"

// labelPattern restricts a snapshot label to a safe, portable charset --
// it's used as (part of) a single path component, the same constraint
// canvas IDs are held to.
var labelPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{0,64}$`)

// Entry describes one snapshot.
type Entry struct {
	// Name is the on-disk directory name, e.g. "20260703-120000.000000000"
	// or "20260703-120000.000000000-mysnap" -- exactly what `scrim snaps`
	// prints and what `scrim revert <id> <snapshot>` expects back.
	Name      string
	Label     string
	Timestamp time.Time
	Dir       string
}

// Dir returns the directory that holds every snapshot for canvas id under
// versionsDir.
func Dir(versionsDir, id string) string {
	return filepath.Join(versionsDir, id)
}

// Create copies canvasDir's current contents into a new timestamped
// directory under Dir(versionsDir, id), returning the new snapshot's Entry.
func Create(canvasDir, versionsDir, id, label string) (Entry, error) {
	if err := canvas.ValidateID(id); err != nil {
		return Entry{}, err
	}
	if !labelPattern.MatchString(label) {
		return Entry{}, fmt.Errorf("snapshot: invalid label %q (must match %s)", label, labelPattern.String())
	}
	if fi, err := os.Stat(canvasDir); err != nil || !fi.IsDir() {
		return Entry{}, fmt.Errorf("snapshot: canvas %s: no such directory", id)
	}

	now := time.Now().UTC()
	name := now.Format(timestampLayout)
	if label != "" {
		name += "-" + label
	}
	dstDir := filepath.Join(Dir(versionsDir, id), name)
	if err := copyTree(canvasDir, dstDir); err != nil {
		return Entry{}, fmt.Errorf("snapshotting canvas %s: %w", id, err)
	}
	return Entry{Name: name, Label: label, Timestamp: now, Dir: dstDir}, nil
}

// List returns every snapshot for canvas id under versionsDir, sorted
// oldest-first -- Name's fixed-width leading timestamp means a plain string
// sort is also a chronological sort. A missing snapshots directory is
// treated as "no snapshots" rather than an error.
func List(versionsDir, id string) ([]Entry, error) {
	if err := canvas.ValidateID(id); err != nil {
		return nil, err
	}
	dir := Dir(versionsDir, id)
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing snapshots for %s: %w", id, err)
	}

	out := make([]Entry, 0, len(dirEntries))
	for _, e := range dirEntries {
		if !e.IsDir() {
			continue
		}
		entry, ok := parseName(e.Name())
		if !ok {
			continue // skip anything in the versions dir that isn't a snapshot we created
		}
		entry.Dir = filepath.Join(dir, e.Name())
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Latest returns the most recent snapshot for id, or ok == false if there
// are none.
func Latest(versionsDir, id string) (entry Entry, ok bool, err error) {
	entries, err := List(versionsDir, id)
	if err != nil || len(entries) == 0 {
		return Entry{}, false, err
	}
	return entries[len(entries)-1], true, nil
}

// Revert replaces canvasDir's current contents with a snapshot's, entirely
// -- not merged -- deleting whatever was there first. name selects which
// snapshot (see List for the Name format); an empty name defaults to the
// latest snapshot for id.
func Revert(canvasDir, versionsDir, id, name string) (Entry, error) {
	if err := canvas.ValidateID(id); err != nil {
		return Entry{}, err
	}

	var entry Entry
	if name == "" {
		latest, ok, err := Latest(versionsDir, id)
		if err != nil {
			return Entry{}, err
		}
		if !ok {
			return Entry{}, fmt.Errorf("snapshot: no snapshots for canvas %s", id)
		}
		entry = latest
	} else {
		parsed, ok := parseName(name)
		if !ok {
			return Entry{}, fmt.Errorf("snapshot: invalid snapshot name %q", name)
		}
		parsed.Dir = filepath.Join(Dir(versionsDir, id), name)
		if fi, err := os.Stat(parsed.Dir); err != nil || !fi.IsDir() {
			return Entry{}, fmt.Errorf("snapshot: %s: no such snapshot for canvas %s", name, id)
		}
		entry = parsed
	}

	if err := os.RemoveAll(canvasDir); err != nil {
		return Entry{}, fmt.Errorf("clearing canvas %s before revert: %w", id, err)
	}
	if err := copyTree(entry.Dir, canvasDir); err != nil {
		return Entry{}, fmt.Errorf("reverting canvas %s: %w", id, err)
	}
	return entry, nil
}

// parseName parses a snapshot directory name back into an Entry (without
// Dir, which the caller fills in -- it already knows the parent it read the
// name from). It reports ok == false for anything that doesn't start with a
// well-formed timestampLayout prefix, which is how List skips unrelated
// entries that might exist under the versions dir.
func parseName(name string) (Entry, bool) {
	tsLen := len(timestampLayout)
	if len(name) < tsLen {
		return Entry{}, false
	}
	ts, err := time.ParseInLocation(timestampLayout, name[:tsLen], time.UTC)
	if err != nil {
		return Entry{}, false
	}
	label := strings.TrimPrefix(name[tsLen:], "-")
	return Entry{Name: name, Label: label, Timestamp: ts}, true
}

// copyTree recursively copies src onto dst, creating dst if it doesn't
// exist. Regular files and directories are copied with their existing
// permission bits; symlinks are recreated pointing at the same (possibly
// relative) target rather than followed, so a snapshot doesn't silently
// balloon by dereferencing one.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)

		switch {
		case d.Type()&fs.ModeSymlink != 0:
			linkTarget, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("reading symlink %s: %w", path, err)
			}
			return os.Symlink(linkTarget, target)
		case d.IsDir():
			info, err := d.Info()
			if err != nil {
				return err
			}
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		default:
			return copyFile(path, target, d)
		}
	})
}

func copyFile(src, dst string, d fs.DirEntry) error {
	info, err := d.Info()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil { //nolint:gosec // mirrors the source canvas dir's own layout, not sensitive
		return err
	}

	in, err := os.Open(src) //nolint:gosec // src is walked from a caller-provided canvas/snapshot dir, not arbitrary user input
	if err != nil {
		return err
	}
	defer in.Close() //nolint:errcheck // read-only handle, close error not actionable

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copying %s: %w", src, err)
	}
	return out.Close()
}
