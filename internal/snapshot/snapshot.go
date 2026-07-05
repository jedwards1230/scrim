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

// errInvalidName is returned by validateName for a snapshot name that is
// not safe to use as a single path component.
var errInvalidName = errors.New("snapshot: invalid snapshot name")

// ErrNotFound is the sentinel wrapped by every "the thing you named does not
// exist" failure: a missing canvas directory (Create), a missing or unknown
// snapshot (Revert with a name that doesn't exist), or no snapshots at all
// (revert-to-latest with none). Callers translating snapshot errors into
// HTTP statuses (the hub machine API) match it with errors.Is to tell a
// client's 404 from a server-side 500.
var ErrNotFound = errors.New("snapshot: not found")

// ErrInvalidLabel is the sentinel wrapped when Create rejects a label that
// fails labelPattern -- pure client input, distinct from a server fault.
var ErrInvalidLabel = errors.New("snapshot: invalid label")

// errSnapshotEscape mirrors server/staticpath.go's errOutsideRoot for the
// same class of problem on the snapshot side: a resolved snapshot path that
// falls outside its canvas's versions directory.
var errSnapshotEscape = errors.New("snapshot: name escapes versions directory")

// validateName reports whether name is safe to use as a single path
// component under a canvas's versions directory -- the same bare-component
// precedent canvas.ValidateID holds IDs to, and labelPattern holds labels
// to. Unlike an id or a label, though, a snapshot *name* (as accepted by
// Revert, straight from a CLI argument -- see cmd/revert.go) is not
// constrained by either of those: it's the on-disk directory name Create
// produced, and parseName only validates that its leading timestampLayout
// prefix parses as a timestamp, deliberately leaving everything after the
// prefix as an opaque label suffix. That means a name like
// "20260703-120000.000000000-../../../etc/passwd" clears parseName's check
// while still being a path-traversal payload once joined into a directory
// path. validateName closes that gap by requiring name to round-trip
// unchanged through filepath.Base: anything containing a path separator,
// "..", or that is otherwise not a bare component is rejected outright,
// before Revert ever touches the filesystem.
func validateName(name string) error {
	if name == "" || name == "." || name == ".." || name != filepath.Base(name) {
		return fmt.Errorf("%w: %q (must be a single path component)", errInvalidName, name)
	}
	return nil
}

// resolveSnapshotDir resolves name (already checked by validateName as a
// bare path component) against Dir(versionsDir, id), and independently
// verifies -- after resolving symlinks -- that the result does not escape
// that directory. This mirrors server/staticpath.go's resolveStaticPath,
// which solves the identical containment problem for canvas static-file
// serving. It is deliberately a *second*, independent layer on top of
// validateName: that check alone rejects traversal sequences embedded in
// name itself, but it can't catch a symlink already planted directly under
// the versions directory (e.g. by something else with write access to it)
// that points outside -- only resolving the real path and checking
// containment catches that.
func resolveSnapshotDir(versionsDir, id, name string) (string, error) {
	root := Dir(versionsDir, id)
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	target := filepath.Join(rootAbs, name)
	if target != rootAbs && !strings.HasPrefix(target, rootAbs+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: %q", errSnapshotEscape, name)
	}

	resolvedRoot, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// No versions dir at all means no snapshots for this canvas -- the
			// named one certainly doesn't exist.
			return "", fmt.Errorf("%w: %s: no such snapshot for canvas %s", ErrNotFound, name, id)
		}
		return "", fmt.Errorf("snapshot: resolving versions dir for canvas %s: %w", id, err)
	}
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", fmt.Errorf("%w: %s: no such snapshot for canvas %s", ErrNotFound, name, id)
	}
	if resolvedTarget != resolvedRoot && !strings.HasPrefix(resolvedTarget, resolvedRoot+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: %q", errSnapshotEscape, name)
	}
	return target, nil
}

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
// If copyTree fails partway through, the partially-written destination
// directory is removed rather than left behind as orphaned snapshot debris.
func Create(canvasDir, versionsDir, id, label string) (entry Entry, err error) {
	if err := canvas.ValidateID(id); err != nil {
		return Entry{}, err
	}
	if !labelPattern.MatchString(label) {
		return Entry{}, fmt.Errorf("%w %q (must match %s)", ErrInvalidLabel, label, labelPattern.String())
	}
	if fi, statErr := os.Stat(canvasDir); statErr != nil || !fi.IsDir() {
		return Entry{}, fmt.Errorf("%w: canvas %s: no such directory", ErrNotFound, id)
	}

	now := time.Now().UTC()
	name := now.Format(timestampLayout)
	if label != "" {
		name += "-" + label
	}
	dstDir := filepath.Join(Dir(versionsDir, id), name)
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dstDir)
		}
	}()
	if err = copyTree(canvasDir, dstDir); err != nil {
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
// latest snapshot for id. name comes straight from a CLI argument (see
// cli.cmdRevert), so it is validated as a bare path component -- and the
// directory it resolves to independently re-checked for containment under
// the canvas's versions directory -- before anything is read or written; see
// validateName and resolveSnapshotDir.
//
// The swap onto canvasDir is atomic with respect to partial failure: the
// snapshot is copied into a fresh sibling temp directory first, and only
// once that copy has fully succeeded is canvasDir itself touched, via a
// rename-out/rename-in pair rather than a remove-then-copy -- so a failure
// partway through (a bad symlink target, a permission error, disk full)
// never leaves canvasDir emptied or half-restored.
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
			return Entry{}, fmt.Errorf("%w: no snapshots for canvas %s", ErrNotFound, id)
		}
		entry = latest
	} else {
		if err := validateName(name); err != nil {
			return Entry{}, err
		}
		parsed, ok := parseName(name)
		if !ok {
			return Entry{}, fmt.Errorf("%w %q", errInvalidName, name)
		}
		dir, err := resolveSnapshotDir(versionsDir, id, name)
		if err != nil {
			return Entry{}, err
		}
		if fi, statErr := os.Stat(dir); statErr != nil || !fi.IsDir() {
			return Entry{}, fmt.Errorf("%w: %s: no such snapshot for canvas %s", ErrNotFound, name, id)
		}
		parsed.Dir = dir
		entry = parsed
	}

	tmpDir := canvasDir + ".revert-tmp"
	oldDir := canvasDir + ".revert-old"
	// Clear any stale temp/old directories left behind by a previous
	// interrupted revert before reusing those names.
	if err := os.RemoveAll(tmpDir); err != nil {
		return Entry{}, fmt.Errorf("clearing stale revert temp dir for canvas %s: %w", id, err)
	}
	if err := os.RemoveAll(oldDir); err != nil {
		return Entry{}, fmt.Errorf("clearing stale revert backup dir for canvas %s: %w", id, err)
	}

	if err := copyTree(entry.Dir, tmpDir); err != nil {
		_ = os.RemoveAll(tmpDir)
		return Entry{}, fmt.Errorf("reverting canvas %s: %w", id, err)
	}

	// Move the live directory aside and the new copy into place with two
	// renames -- each a single, atomic filesystem operation -- instead of
	// removing canvasDir and then copying into it. canvasDir is never
	// observably missing for longer than the gap between these two rename
	// calls, and if the second one fails, the first is undone so the
	// original contents are restored rather than lost.
	if err := os.Rename(canvasDir, oldDir); err != nil && !os.IsNotExist(err) {
		_ = os.RemoveAll(tmpDir)
		return Entry{}, fmt.Errorf("clearing canvas %s before revert: %w", id, err)
	}
	if err := os.Rename(tmpDir, canvasDir); err != nil {
		_ = os.Rename(oldDir, canvasDir) // best-effort restore of the pre-revert contents
		return Entry{}, fmt.Errorf("finalizing revert for canvas %s: %w", id, err)
	}
	_ = os.RemoveAll(oldDir)
	return entry, nil
}

// RevertWithSafety is the shared revert protocol every scrim surface (the
// revert CLI verb, the MCP local backend, the hub machine API) runs: resolve
// the target snapshot FIRST, then take a labeled "prerevert" safety snapshot
// of the canvas's current contents, then Revert. The ordering carries two
// guarantees:
//
//   - An empty name resolves "latest" BEFORE the safety snapshot exists --
//     otherwise the safety snapshot would immediately become latest and a
//     bare revert would restore the canvas to its own current state.
//   - A named target is verified to exist BEFORE the safety snapshot is
//     taken -- otherwise a typo'd name would leave a spurious prerevert
//     behind as the newest snapshot, poisoning a later bare
//     revert-to-latest.
//
// A missing named snapshot (or an empty name with no snapshots at all) is
// reported as an error wrapping ErrNotFound.
func RevertWithSafety(canvasDir, versionsDir, id, name string) (Entry, error) {
	if err := canvas.ValidateID(id); err != nil {
		return Entry{}, err
	}

	target := name
	if target == "" {
		latest, ok, err := Latest(versionsDir, id)
		if err != nil {
			return Entry{}, err
		}
		if !ok {
			return Entry{}, fmt.Errorf("%w: no snapshots for canvas %s", ErrNotFound, id)
		}
		target = latest.Name
	} else {
		// Validate and resolve the named target now, before any snapshot is
		// taken -- Revert repeats these same checks, but by then the safety
		// snapshot would already exist.
		if err := validateName(target); err != nil {
			return Entry{}, err
		}
		if _, ok := parseName(target); !ok {
			return Entry{}, fmt.Errorf("%w %q", errInvalidName, target)
		}
		dir, err := resolveSnapshotDir(versionsDir, id, target)
		if err != nil {
			return Entry{}, err
		}
		if fi, statErr := os.Stat(dir); statErr != nil || !fi.IsDir() {
			return Entry{}, fmt.Errorf("%w: %s: no such snapshot for canvas %s", ErrNotFound, target, id)
		}
	}

	// Safety snapshot of the live contents, so the revert is itself undoable
	// -- but only if the canvas dir actually exists (a revert onto a
	// never-created canvas has nothing to preserve).
	if fi, statErr := os.Stat(canvasDir); statErr == nil && fi.IsDir() {
		if _, err := Create(canvasDir, versionsDir, id, "prerevert"); err != nil {
			return Entry{}, err
		}
	}

	return Revert(canvasDir, versionsDir, id, target)
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
