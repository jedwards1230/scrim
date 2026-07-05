package snapshot

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func writeCanvasFile(t *testing.T, canvasDir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(canvasDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canvasDir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCreateCopiesCurrentContents(t *testing.T) {
	canvasDir := filepath.Join(t.TempDir(), "canvas")
	versionsDir := t.TempDir()
	writeCanvasFile(t, canvasDir, "index.html", "<h1>v1</h1>")

	entry, err := Create(canvasDir, versionsDir, "report", "mysnap")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if entry.Label != "mysnap" {
		t.Errorf("Create().Label = %q, want %q", entry.Label, "mysnap")
	}

	got, err := os.ReadFile(filepath.Join(entry.Dir, "index.html"))
	if err != nil {
		t.Fatalf("reading snapshot copy: %v", err)
	}
	if string(got) != "<h1>v1</h1>" {
		t.Errorf("snapshot copy content = %q, want %q", got, "<h1>v1</h1>")
	}
}

func TestCreateEmptyLabelOmitsSuffix(t *testing.T) {
	canvasDir := filepath.Join(t.TempDir(), "canvas")
	versionsDir := t.TempDir()
	writeCanvasFile(t, canvasDir, "index.html", "<h1>v1</h1>")

	entry, err := Create(canvasDir, versionsDir, "report", "")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if entry.Label != "" {
		t.Errorf("Create().Label = %q, want empty", entry.Label)
	}
	if filepath.Base(entry.Dir) != entry.Name {
		t.Errorf("Create().Dir base = %q, want %q", filepath.Base(entry.Dir), entry.Name)
	}
}

func TestCreateRejectsInvalidLabel(t *testing.T) {
	canvasDir := filepath.Join(t.TempDir(), "canvas")
	versionsDir := t.TempDir()
	writeCanvasFile(t, canvasDir, "index.html", "x")

	if _, err := Create(canvasDir, versionsDir, "report", "../escape"); err == nil {
		t.Error("Create() with a path-traversal label should error")
	} else if !errors.Is(err, ErrInvalidLabel) {
		t.Errorf("Create() invalid-label error = %v, want errors.Is(err, ErrInvalidLabel)", err)
	}
}

// TestCreateCleansUpPartialSnapshotOnCopyFailure guards Create's
// orphan-cleanup fix: if copyTree fails partway through, the partially
// written destination snapshot directory must not be left behind under
// versionsDir.
func TestCreateCleansUpPartialSnapshotOnCopyFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission-based copy-failure simulation is not reliable on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses the permission check this test relies on")
	}

	canvasDir := filepath.Join(t.TempDir(), "canvas")
	versionsDir := t.TempDir()
	writeCanvasFile(t, canvasDir, "ok.txt", "fine")

	unreadable := filepath.Join(canvasDir, "unreadable.txt")
	if err := os.WriteFile(unreadable, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unreadable, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o644) })

	if _, err := Create(canvasDir, versionsDir, "report", "partial"); err == nil {
		t.Fatal("Create() with an unreadable source file should error")
	}

	entries, err := os.ReadDir(Dir(versionsDir, "report"))
	if err != nil {
		if !os.IsNotExist(err) {
			t.Fatalf("reading versions dir: %v", err)
		}
		return // versions dir never created at all -- also fine, nothing orphaned
	}
	if len(entries) != 0 {
		t.Errorf("Create() on failure left orphaned entries in versions dir: %v", entries)
	}
}

func TestCreateMissingCanvasDirErrors(t *testing.T) {
	versionsDir := t.TempDir()
	if _, err := Create(filepath.Join(t.TempDir(), "does-not-exist"), versionsDir, "report", ""); err == nil {
		t.Error("Create() of a missing canvas dir should error")
	} else if !errors.Is(err, ErrNotFound) {
		t.Errorf("Create() missing-canvas error = %v, want errors.Is(err, ErrNotFound)", err)
	}
}

func TestListSortedOldestFirst(t *testing.T) {
	canvasDir := filepath.Join(t.TempDir(), "canvas")
	versionsDir := t.TempDir()
	writeCanvasFile(t, canvasDir, "index.html", "v1")

	first, err := Create(canvasDir, versionsDir, "report", "first")
	if err != nil {
		t.Fatal(err)
	}
	writeCanvasFile(t, canvasDir, "index.html", "v2")
	second, err := Create(canvasDir, versionsDir, "report", "second")
	if err != nil {
		t.Fatal(err)
	}

	entries, err := List(versionsDir, "report")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("List() len = %d, want 2", len(entries))
	}
	if entries[0].Name != first.Name || entries[1].Name != second.Name {
		t.Errorf("List() = %+v, want oldest-first order [%s, %s]", entries, first.Name, second.Name)
	}
}

func TestListMissingDirIsEmptyNotError(t *testing.T) {
	entries, err := List(t.TempDir(), "report")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if entries != nil {
		t.Errorf("List() = %+v, want nil", entries)
	}
}

func TestLatest(t *testing.T) {
	canvasDir := filepath.Join(t.TempDir(), "canvas")
	versionsDir := t.TempDir()
	writeCanvasFile(t, canvasDir, "index.html", "v1")

	if _, ok, err := Latest(versionsDir, "report"); err != nil || ok {
		t.Fatalf("Latest() on canvas with no snapshots = (ok=%v, err=%v), want (false, nil)", ok, err)
	}

	if _, err := Create(canvasDir, versionsDir, "report", "first"); err != nil {
		t.Fatal(err)
	}
	writeCanvasFile(t, canvasDir, "index.html", "v2")
	second, err := Create(canvasDir, versionsDir, "report", "second")
	if err != nil {
		t.Fatal(err)
	}

	latest, ok, err := Latest(versionsDir, "report")
	if err != nil {
		t.Fatalf("Latest() error = %v", err)
	}
	if !ok || latest.Name != second.Name {
		t.Errorf("Latest() = %+v (ok=%v), want %s", latest, ok, second.Name)
	}
}

func TestRevertToLatestReplacesContents(t *testing.T) {
	canvasDir := filepath.Join(t.TempDir(), "canvas")
	versionsDir := t.TempDir()
	writeCanvasFile(t, canvasDir, "index.html", "v1")
	if _, err := Create(canvasDir, versionsDir, "report", "v1"); err != nil {
		t.Fatal(err)
	}

	// Modify after snapping: add a new file and change the existing one.
	writeCanvasFile(t, canvasDir, "index.html", "v2 -- modified")
	writeCanvasFile(t, canvasDir, "extra.txt", "should not survive a revert")

	entry, err := Revert(canvasDir, versionsDir, "report", "")
	if err != nil {
		t.Fatalf("Revert() error = %v", err)
	}
	if entry.Label != "v1" {
		t.Errorf("Revert() restored entry.Label = %q, want %q", entry.Label, "v1")
	}

	got, err := os.ReadFile(filepath.Join(canvasDir, "index.html"))
	if err != nil {
		t.Fatalf("reading reverted index.html: %v", err)
	}
	if string(got) != "v1" {
		t.Errorf("reverted index.html = %q, want %q", got, "v1")
	}
	if _, err := os.Stat(filepath.Join(canvasDir, "extra.txt")); !os.IsNotExist(err) {
		t.Errorf("extra.txt added after the snapshot still exists post-revert (want replace, not merge): err = %v", err)
	}
}

func TestRevertToNamedSnapshot(t *testing.T) {
	canvasDir := filepath.Join(t.TempDir(), "canvas")
	versionsDir := t.TempDir()
	writeCanvasFile(t, canvasDir, "index.html", "v1")
	first, err := Create(canvasDir, versionsDir, "report", "v1")
	if err != nil {
		t.Fatal(err)
	}
	writeCanvasFile(t, canvasDir, "index.html", "v2")
	if _, err := Create(canvasDir, versionsDir, "report", "v2"); err != nil {
		t.Fatal(err)
	}
	writeCanvasFile(t, canvasDir, "index.html", "v3 -- current")

	if _, err := Revert(canvasDir, versionsDir, "report", first.Name); err != nil {
		t.Fatalf("Revert() error = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(canvasDir, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "v1" {
		t.Errorf("reverted index.html = %q, want %q (the named, not latest, snapshot)", got, "v1")
	}
}

func TestRevertUnknownSnapshotErrors(t *testing.T) {
	canvasDir := filepath.Join(t.TempDir(), "canvas")
	versionsDir := t.TempDir()
	writeCanvasFile(t, canvasDir, "index.html", "v1")
	if _, err := Create(canvasDir, versionsDir, "report", ""); err != nil {
		t.Fatal(err)
	}

	if _, err := Revert(canvasDir, versionsDir, "report", "20200101-000000.000000000-nope"); err == nil {
		t.Error("Revert() with an unknown snapshot name should error")
	} else if !errors.Is(err, ErrNotFound) {
		t.Errorf("Revert() unknown-snapshot error = %v, want errors.Is(err, ErrNotFound)", err)
	}
}

// TestValidateNameRejectsUnsafeComponents is the table-driven test for
// validateName itself: every malicious payload here is rejected purely by
// string inspection (filepath.Base comparison), with no filesystem access
// at all -- validateName never opens, stats, or walks anything.
func TestValidateNameRejectsUnsafeComponents(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"empty", "", true},
		{"dot", ".", true},
		{"dotdot", "..", true},
		{"bare timestamp", "20260703-120000.000000000", false},
		{"timestamp with legitimate label", "20260703-120000.000000000-mysnap", false},
		// The exact vulnerability class this closes: parseName only
		// validates the leading timestamp, so a traversal sequence smuggled
		// in as the "label" suffix previously reached the filesystem
		// unchecked.
		{"traversal smuggled in label suffix", "20260703-120000.000000000-../../../etc/passwd", true},
		{"absolute path payload", "/etc/passwd", true},
		{"embedded separator", "20260703-120000.000000000-sub/dir", true},
		{"windows-style separator", "20260703-120000.000000000-sub\\dir", runtime.GOOS == "windows"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateName(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateName(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
		})
	}
}

// TestRevertRejectsExactVulnerabilityPayload reproduces the exact
// path-traversal payload the vulnerability class allows: a name whose
// leading tsLen characters parse as a well-formed timestamp (clearing
// parseName's prefix check) but whose trailing "label" portion is a
// "../"-traversal sequence. Before this fix, that raw name was joined
// straight into the snapshot's directory path with no containment check at
// all, letting Revert read from (and, via the old remove-then-copy
// behavior, write into) an arbitrary host path. The rejection happens in
// validateName, before Revert performs any filesystem read or write, and
// canvasDir must be left completely untouched.
func TestRevertRejectsExactVulnerabilityPayload(t *testing.T) {
	canvasDir := filepath.Join(t.TempDir(), "canvas")
	versionsDir := t.TempDir()
	writeCanvasFile(t, canvasDir, "index.html", "v1")
	if _, err := Create(canvasDir, versionsDir, "report", ""); err != nil {
		t.Fatal(err)
	}

	const payload = "20260703-120000.000000000-../../../../etc"
	if _, err := Revert(canvasDir, versionsDir, "report", payload); err == nil {
		t.Fatalf("Revert() with path-traversal payload %q should be rejected, not accepted", payload)
	}

	got, err := os.ReadFile(filepath.Join(canvasDir, "index.html"))
	if err != nil {
		t.Fatalf("reading canvasDir after rejected Revert(): %v", err)
	}
	if string(got) != "v1" {
		t.Errorf("canvasDir was modified by a rejected Revert(): got %q, want untouched %q", got, "v1")
	}
}

// TestRevertRejectsSymlinkEscapeInVersionsDir exercises the second,
// independent containment layer: a snapshot "name" that is itself a single
// bare path component (so it passes validateName) but is actually a symlink
// planted directly under the versions directory pointing outside of it.
// validateName alone would accept this name; only resolveSnapshotDir's
// EvalSymlinks-based containment check (mirroring
// server/staticpath.go's resolveStaticPath) catches it -- and it does so
// before Revert removes or writes anything under canvasDir.
func TestRevertRejectsSymlinkEscapeInVersionsDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks requires elevated privileges on windows")
	}

	canvasDir := filepath.Join(t.TempDir(), "canvas")
	versionsDir := t.TempDir()
	writeCanvasFile(t, canvasDir, "index.html", "v1")

	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("should never be read"), 0o644); err != nil {
		t.Fatal(err)
	}

	snapDir := Dir(versionsDir, "report")
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const linkName = "20260703-120000.000000000-evil"
	if err := os.Symlink(outside, filepath.Join(snapDir, linkName)); err != nil {
		t.Fatal(err)
	}

	if _, err := Revert(canvasDir, versionsDir, "report", linkName); err == nil {
		t.Fatal("Revert() through a versions-dir symlink escaping outside should be rejected")
	}

	got, err := os.ReadFile(filepath.Join(canvasDir, "index.html"))
	if err != nil {
		t.Fatalf("reading canvasDir after rejected Revert(): %v", err)
	}
	if string(got) != "v1" {
		t.Errorf("canvasDir was modified by a rejected Revert(): got %q, want untouched %q", got, "v1")
	}
}

// TestRevertLegitimateLabeledSnapshotStillWorks guards against the fix
// being overzealous: a normal, non-malicious labeled snapshot name (exactly
// what Create produces and List reports) must still revert successfully.
func TestRevertLegitimateLabeledSnapshotStillWorks(t *testing.T) {
	canvasDir := filepath.Join(t.TempDir(), "canvas")
	versionsDir := t.TempDir()
	writeCanvasFile(t, canvasDir, "index.html", "v1")
	first, err := Create(canvasDir, versionsDir, "report", "my-label_1")
	if err != nil {
		t.Fatal(err)
	}
	writeCanvasFile(t, canvasDir, "index.html", "v2 -- current")

	entry, err := Revert(canvasDir, versionsDir, "report", first.Name)
	if err != nil {
		t.Fatalf("Revert() with a legitimate labeled snapshot name errored: %v", err)
	}
	if entry.Label != "my-label_1" {
		t.Errorf("Revert().Label = %q, want %q", entry.Label, "my-label_1")
	}
	got, err := os.ReadFile(filepath.Join(canvasDir, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "v1" {
		t.Errorf("reverted index.html = %q, want %q", got, "v1")
	}
}

// TestRevertLeavesCanvasUntouchedOnCopyFailure guards Revert's atomicity
// fix: if copying the snapshot into the temp directory fails partway, the
// live canvasDir must be left completely untouched (not emptied, not
// half-restored), and no ".revert-tmp"/".revert-old" directory left behind.
func TestRevertLeavesCanvasUntouchedOnCopyFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission-based copy-failure simulation is not reliable on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses the permission check this test relies on")
	}

	canvasDir := filepath.Join(t.TempDir(), "canvas")
	versionsDir := t.TempDir()
	writeCanvasFile(t, canvasDir, "index.html", "v1")
	first, err := Create(canvasDir, versionsDir, "report", "v1")
	if err != nil {
		t.Fatal(err)
	}

	// Corrupt the snapshot itself so copying it back out fails partway
	// through.
	unreadable := filepath.Join(first.Dir, "index.html")
	if err := os.Chmod(unreadable, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o644) })

	writeCanvasFile(t, canvasDir, "index.html", "v2 -- current, should survive")

	if _, err := Revert(canvasDir, versionsDir, "report", first.Name); err == nil {
		t.Fatal("Revert() from an unreadable snapshot should error")
	}

	got, err := os.ReadFile(filepath.Join(canvasDir, "index.html"))
	if err != nil {
		t.Fatalf("reading canvasDir after failed Revert(): %v", err)
	}
	if string(got) != "v2 -- current, should survive" {
		t.Errorf("canvasDir was modified despite Revert() failing: got %q", got)
	}
	if _, err := os.Stat(canvasDir + ".revert-tmp"); !os.IsNotExist(err) {
		t.Errorf("Revert() left a stale .revert-tmp directory behind: stat err = %v", err)
	}
	if _, err := os.Stat(canvasDir + ".revert-old"); !os.IsNotExist(err) {
		t.Errorf("Revert() left a stale .revert-old directory behind: stat err = %v", err)
	}
}

// TestRevertSuccessLeavesNoTempDirsBehind guards against the atomic
// rename-swap itself leaking its scratch directories on the happy path.
func TestRevertSuccessLeavesNoTempDirsBehind(t *testing.T) {
	canvasDir := filepath.Join(t.TempDir(), "canvas")
	versionsDir := t.TempDir()
	writeCanvasFile(t, canvasDir, "index.html", "v1")
	if _, err := Create(canvasDir, versionsDir, "report", "v1"); err != nil {
		t.Fatal(err)
	}
	writeCanvasFile(t, canvasDir, "index.html", "v2")

	if _, err := Revert(canvasDir, versionsDir, "report", ""); err != nil {
		t.Fatalf("Revert() error = %v", err)
	}
	if _, err := os.Stat(canvasDir + ".revert-tmp"); !os.IsNotExist(err) {
		t.Errorf("successful Revert() left a stale .revert-tmp directory behind: stat err = %v", err)
	}
	if _, err := os.Stat(canvasDir + ".revert-old"); !os.IsNotExist(err) {
		t.Errorf("successful Revert() left a stale .revert-old directory behind: stat err = %v", err)
	}
}

func TestRevertNoSnapshotsErrors(t *testing.T) {
	canvasDir := filepath.Join(t.TempDir(), "canvas")
	versionsDir := t.TempDir()
	writeCanvasFile(t, canvasDir, "index.html", "v1")

	if _, err := Revert(canvasDir, versionsDir, "report", ""); err == nil {
		t.Error("Revert() with no snapshots at all should error")
	} else if !errors.Is(err, ErrNotFound) {
		t.Errorf("Revert() no-snapshots error = %v, want errors.Is(err, ErrNotFound)", err)
	}
}

func TestCreateBackToBackProducesDistinctSnapshots(t *testing.T) {
	canvasDir := filepath.Join(t.TempDir(), "canvas")
	versionsDir := t.TempDir()
	writeCanvasFile(t, canvasDir, "index.html", "v1")

	a, err := Create(canvasDir, versionsDir, "report", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Create(canvasDir, versionsDir, "report", "")
	if err != nil {
		t.Fatal(err)
	}
	if a.Name == b.Name {
		t.Errorf("two back-to-back Create() calls produced the same snapshot name %q", a.Name)
	}
}

func TestParseNameRoundTrips(t *testing.T) {
	tests := []struct {
		name      string
		wantOK    bool
		wantLabel string
	}{
		{"20260703-120000.000000000", true, ""},
		{"20260703-120000.000000000-mysnap", true, "mysnap"},
		{"not-a-timestamp", false, ""},
		{"", false, ""},
	}
	for _, tt := range tests {
		entry, ok := parseName(tt.name)
		if ok != tt.wantOK {
			t.Errorf("parseName(%q) ok = %v, want %v", tt.name, ok, tt.wantOK)
			continue
		}
		if ok && entry.Label != tt.wantLabel {
			t.Errorf("parseName(%q).Label = %q, want %q", tt.name, entry.Label, tt.wantLabel)
		}
	}
}

func TestListSkipsNonSnapshotEntries(t *testing.T) {
	dir := Dir(t.TempDir(), "report")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "not-a-snapshot"), 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := List(filepath.Dir(dir), "report")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("List() = %+v, want empty (non-snapshot dir entry should be skipped)", entries)
	}
}

// TestTimestampLayoutIsFixedWidth guards the assumption List/parseName rely
// on: every formatted timestamp has the same length as the layout string
// itself, which is what makes a plain name-string sort also a chronological
// sort.
func TestTimestampLayoutIsFixedWidth(t *testing.T) {
	times := []time.Time{
		time.Date(2026, 1, 1, 0, 0, 0, 1, time.UTC),
		time.Date(2026, 12, 31, 23, 59, 59, 999999999, time.UTC),
	}
	for _, tm := range times {
		formatted := tm.Format(timestampLayout)
		if len(formatted) != len(timestampLayout) {
			t.Errorf("time.Format(timestampLayout) for %v = %q (len %d), want len %d", tm, formatted, len(formatted), len(timestampLayout))
		}
	}
}

// TestRevertWithSafetyBareRevert proves the shared helper preserves the
// resolve-before-snapshot semantic: a bare revert (empty name) restores the
// canvas to the latest PRE-EXISTING snapshot -- never to its own current
// state via the prerevert safety snapshot -- and the prerevert snapshot of
// the pre-revert contents exists afterwards.
func TestRevertWithSafetyBareRevert(t *testing.T) {
	canvasDir := filepath.Join(t.TempDir(), "canvas")
	versionsDir := t.TempDir()

	writeCanvasFile(t, canvasDir, "index.html", "v1")
	if _, err := Create(canvasDir, versionsDir, "report", ""); err != nil {
		t.Fatal(err)
	}
	writeCanvasFile(t, canvasDir, "index.html", "v2")

	entry, err := RevertWithSafety(canvasDir, versionsDir, "report", "")
	if err != nil {
		t.Fatalf("RevertWithSafety() error = %v", err)
	}

	got, err := os.ReadFile(filepath.Join(canvasDir, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "v1" {
		t.Errorf("reverted index.html = %q, want v1 (bare revert must not restore the canvas to its own current state)", got)
	}
	if entry.Label == "prerevert" {
		t.Errorf("RevertWithSafety() reverted to %q -- the safety snapshot itself", entry.Name)
	}

	entries, err := List(versionsDir, "report")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("List() after revert = %d entries, want 2 (original + prerevert)", len(entries))
	}
	if newest := entries[len(entries)-1]; newest.Label != "prerevert" {
		t.Errorf("newest snapshot label = %q, want prerevert", newest.Label)
	}
}

// TestRevertWithSafetyUnknownNameLeavesNoPrerevert is the regression test for
// the prerevert-before-validation bug: a revert to a nonexistent snapshot
// name must fail with ErrNotFound AND leave no new prerevert snapshot behind
// -- otherwise the typo'd revert would poison a later bare revert-to-latest.
func TestRevertWithSafetyUnknownNameLeavesNoPrerevert(t *testing.T) {
	canvasDir := filepath.Join(t.TempDir(), "canvas")
	versionsDir := t.TempDir()

	writeCanvasFile(t, canvasDir, "index.html", "v1")
	if _, err := Create(canvasDir, versionsDir, "report", ""); err != nil {
		t.Fatal(err)
	}

	_, err := RevertWithSafety(canvasDir, versionsDir, "report", "20200101-000000.000000000-nope")
	if err == nil {
		t.Fatal("RevertWithSafety() with an unknown snapshot name should error")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("RevertWithSafety() unknown-name error = %v, want errors.Is(err, ErrNotFound)", err)
	}

	entries, err := List(versionsDir, "report")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("List() after failed revert = %d entries, want 1 (no prerevert debris)", len(entries))
	}
	if entries[0].Label == "prerevert" {
		t.Error("failed revert left a prerevert snapshot behind")
	}
}
