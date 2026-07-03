package snapshot

import (
	"os"
	"path/filepath"
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
	}
}

func TestCreateMissingCanvasDirErrors(t *testing.T) {
	versionsDir := t.TempDir()
	if _, err := Create(filepath.Join(t.TempDir(), "does-not-exist"), versionsDir, "report", ""); err == nil {
		t.Error("Create() of a missing canvas dir should error")
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
	}
}

func TestRevertNoSnapshotsErrors(t *testing.T) {
	canvasDir := filepath.Join(t.TempDir(), "canvas")
	versionsDir := t.TempDir()
	writeCanvasFile(t, canvasDir, "index.html", "v1")

	if _, err := Revert(canvasDir, versionsDir, "report", ""); err == nil {
		t.Error("Revert() with no snapshots at all should error")
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
