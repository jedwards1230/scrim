package canvas

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestValidateID(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		wantErr bool
	}{
		{"simple alnum", "report", false},
		{"with dash", "my-report", false},
		{"with underscore", "my_report", false},
		{"digits", "123abc", false},
		{"empty", "", true},
		{"path separator", "foo/bar", true},
		{"dot dot", "..", true},
		{"leading dot traversal", "../etc", true},
		{"absolute path", "/etc/passwd", true},
		{"leading underscore reserved-looking", "__events", true},
		{"leading dash", "-foo", true},
		{"dot file", ".hidden", true},
		{"spaces", "my report", true},
		{"too long", string(make([]byte, 200)), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateID(tt.id)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateID(%q) error = %v, wantErr %v", tt.id, err, tt.wantErr)
			}
		})
	}
}

func TestCreateGetListDelete(t *testing.T) {
	dir := t.TempDir()
	metaDir := filepath.Join(t.TempDir(), "meta")

	canvasDir, err := Create(dir, metaDir, "report", "My Report", "A description", "")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if canvasDir != Dir(dir, "report") {
		t.Errorf("Create() dir = %q, want %q", canvasDir, Dir(dir, "report"))
	}
	if !Exists(dir, "report") {
		t.Error("Exists() = false after Create()")
	}

	info, err := Get(dir, metaDir, "report")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if info.Title != "My Report" {
		t.Errorf("Get().Title = %q, want %q", info.Title, "My Report")
	}
	if info.Description != "A description" {
		t.Errorf("Get().Description = %q, want %q", info.Description, "A description")
	}
	if info.Icon != DefaultIcon("report") {
		t.Errorf("Get().Icon = %q, want deterministic default %q", info.Icon, DefaultIcon("report"))
	}

	if _, err := Create(dir, metaDir, "untitled", "", "", ""); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	list, err := List(dir, metaDir)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("List() len = %d, want 2", len(list))
	}
	if list[0].ID != "report" || list[1].ID != "untitled" {
		t.Errorf("List() not sorted by ID: %+v", list)
	}

	if err := Delete(dir, metaDir, "report"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if Exists(dir, "report") {
		t.Error("Exists() = true after Delete()")
	}

	list, err = List(dir, metaDir)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("List() after delete len = %d, want 1", len(list))
	}
}

func TestListMissingDir(t *testing.T) {
	list, err := List(filepath.Join(t.TempDir(), "does-not-exist"), t.TempDir())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if list != nil {
		t.Errorf("List() = %+v, want nil", list)
	}
}

func TestListSkipsInvalidEntries(t *testing.T) {
	dir := t.TempDir()
	metaDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "valid"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, ".hidden"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "not-a-dir"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	list, err := List(dir, metaDir)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 1 || list[0].ID != "valid" {
		t.Errorf("List() = %+v, want only [valid]", list)
	}
}

func TestDeleteInvalidID(t *testing.T) {
	dir := t.TempDir()
	if err := Delete(dir, t.TempDir(), "../escape"); err == nil {
		t.Error("Delete() with traversal id should error")
	}
}

func TestLastModifiedReflectsNestedWrites(t *testing.T) {
	dir := t.TempDir()
	metaDir := t.TempDir()
	canvasDir, err := Create(dir, metaDir, "report", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(canvasDir, "assets")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(canvasDir, old, old); err != nil {
		t.Fatal(err)
	}

	nested := filepath.Join(sub, "style.css")
	if err := os.WriteFile(nested, []byte("body{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	info, err := Get(dir, metaDir, "report")
	if err != nil {
		t.Fatal(err)
	}
	if info.ModTime.Before(old.Add(time.Minute)) {
		t.Errorf("Get().ModTime = %v, want it to reflect the nested file's recent write", info.ModTime)
	}
}

// TestMetadataStoredExternally is a regression test for the v0.2 metadata
// relocation: metadata must live under metaDir, keyed by id, and must NOT
// be written anywhere inside the canvas directory itself (anything under
// the canvas directory is servable/watchable by the daemon).
func TestMetadataStoredExternally(t *testing.T) {
	canvasesDir := t.TempDir()
	metaDir := t.TempDir()

	canvasDir, err := Create(canvasesDir, metaDir, "report", "My Report", "desc", "")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	entries, err := os.ReadDir(canvasDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("canvas dir has entries after Create() with only metadata given: %+v (metadata must not be written inside the canvas dir)", entries)
	}

	metaEntries, err := os.ReadDir(metaDir)
	if err != nil {
		t.Fatalf("reading metaDir: %v", err)
	}
	if len(metaEntries) != 1 || metaEntries[0].Name() != "report.json" {
		t.Errorf("metaDir entries = %+v, want exactly [report.json]", metaEntries)
	}

	// A second daemon instance pointed at the same directories (simulating
	// a restart) must read back the same metadata.
	info, err := Get(canvasesDir, metaDir, "report")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if info.Title != "My Report" || info.Description != "desc" {
		t.Errorf("Get() after simulated restart = %+v, want title/description preserved", info)
	}
}

// TestMetadataDeletedWithCanvas confirms Delete removes the external
// metadata file too, not just the canvas directory.
func TestMetadataDeletedWithCanvas(t *testing.T) {
	canvasesDir := t.TempDir()
	metaDir := t.TempDir()
	if _, err := Create(canvasesDir, metaDir, "report", "My Report", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := Delete(canvasesDir, metaDir, "report"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := os.Stat(metaPath(metaDir, "report")); !os.IsNotExist(err) {
		t.Errorf("metadata file still exists after Delete(): err = %v", err)
	}
}

// TestDeleteMissingMetadataIsNotAnError confirms deleting a canvas that was
// created with no metadata at all (so no metadata file was ever written)
// doesn't error just because there's nothing to remove.
func TestDeleteMissingMetadataIsNotAnError(t *testing.T) {
	canvasesDir := t.TempDir()
	metaDir := t.TempDir()
	if _, err := Create(canvasesDir, metaDir, "report", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := Delete(canvasesDir, metaDir, "report"); err != nil {
		t.Errorf("Delete() of a canvas with no metadata file error = %v, want nil", err)
	}
}

// TestWriteMetaLeavesExistingFileUntouchedOnFailure guards writeMeta's
// atomicity fix: if the final rename fails, whatever already existed at the
// destination path must be left completely untouched -- not corrupted, not
// partially overwritten -- and no stray temp file left behind in metaDir.
func TestWriteMetaLeavesExistingFileUntouchedOnFailure(t *testing.T) {
	metaDir := t.TempDir()
	const id = "report"

	// Force the final os.Rename to fail: a directory can never be replaced
	// by renaming a regular file over it (POSIX and Windows both refuse
	// this), so this deterministically exercises the rename-failure path
	// without needing permission tricks.
	if err := os.Mkdir(metaPath(metaDir, id), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := writeMeta(metaDir, id, meta{Title: "should not be written"}); err == nil {
		t.Fatal("writeMeta() with a directory occupying the destination path should error")
	}

	fi, err := os.Stat(metaPath(metaDir, id))
	if err != nil {
		t.Fatalf("stat metadata path after failed writeMeta(): %v", err)
	}
	if !fi.IsDir() {
		t.Error("writeMeta() failure left the destination path replaced instead of untouched")
	}

	entries, err := os.ReadDir(metaDir)
	if err != nil {
		t.Fatal(err)
	}
	wantName := filepath.Base(metaPath(metaDir, id))
	for _, e := range entries {
		if e.Name() != wantName {
			t.Errorf("writeMeta() left a stray temp file behind after failure: %q", e.Name())
		}
	}
}

// TestWriteMetaAtomicUnderConcurrentReads guards the actual failure mode
// the review comment described: a reader (readMeta, via the dashboard or
// favicon handlers) hitting a torn, partially-written metadata file mid
// write. It runs writes and reads concurrently against the same id --
// under `go test -race` this also proves the write path itself introduces
// no data race -- and asserts a reader never observes content that fails
// to unmarshal, which is what a non-atomic write would eventually produce
// under enough concurrent iterations. This holds deterministically here
// because writeMeta always builds the new content in a separate temp file
// and only exposes it at metaPath via a single os.Rename: a concurrent
// os.ReadFile of metaPath can only ever observe the complete previous
// file, the complete new file, or "not found" (before the first write) --
// never a partial one.
func TestWriteMetaAtomicUnderConcurrentReads(t *testing.T) {
	metaDir := t.TempDir()
	const id = "concurrent"
	const iterations = 200

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			m := meta{Title: fmt.Sprintf("title-%d", i)}
			if err := writeMeta(metaDir, id, m); err != nil {
				t.Errorf("writeMeta() error = %v", err)
				return
			}
		}
	}()

	var corrupted int32
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			data, err := os.ReadFile(metaPath(metaDir, id)) //nolint:gosec // metaDir/id are test-controlled constants
			if err != nil {
				continue // not written yet -- fine, not what this test guards against
			}
			var m meta
			if jsonErr := json.Unmarshal(data, &m); jsonErr != nil {
				atomic.AddInt32(&corrupted, 1)
			}
		}
	}()

	wg.Wait()
	if corrupted != 0 {
		t.Errorf("readMeta observed %d corrupted (partially-written) metadata file(s) during concurrent writes", corrupted)
	}
}

func TestDefaultIconAndColorAreDeterministic(t *testing.T) {
	ids := []string{"report", "my-canvas", "another_one", "z", "123abc"}
	for _, id := range ids {
		icon1, icon2 := DefaultIcon(id), DefaultIcon(id)
		if icon1 != icon2 {
			t.Errorf("DefaultIcon(%q) not stable across calls: %q vs %q", id, icon1, icon2)
		}
		color1, color2 := DefaultColor(id), DefaultColor(id)
		if color1 != color2 {
			t.Errorf("DefaultColor(%q) not stable across calls: %q vs %q", id, color1, color2)
		}
	}
}

func TestDefaultIconAndColorDistinguishDifferentIDs(t *testing.T) {
	ids := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel"}
	icons := make(map[string]bool)
	colorsSeen := make(map[string]bool)
	for _, id := range ids {
		icons[DefaultIcon(id)] = true
		colorsSeen[DefaultColor(id)] = true
	}
	if len(icons) < 2 {
		t.Errorf("DefaultIcon produced only %d distinct glyph(s) across %d ids, want more variety", len(icons), len(ids))
	}
	if len(colorsSeen) < 2 {
		t.Errorf("DefaultColor produced only %d distinct color(s) across %d ids, want more variety", len(colorsSeen), len(ids))
	}
}

func TestExplicitIconOverridesDefault(t *testing.T) {
	canvasesDir := t.TempDir()
	metaDir := t.TempDir()
	if _, err := Create(canvasesDir, metaDir, "report", "", "", "🐸"); err != nil {
		t.Fatal(err)
	}
	info, err := Get(canvasesDir, metaDir, "report")
	if err != nil {
		t.Fatal(err)
	}
	if info.Icon != "🐸" {
		t.Errorf("Get().Icon = %q, want explicit override %q", info.Icon, "🐸")
	}
	// Color is always derived, even when Icon is overridden.
	if info.Color != DefaultColor("report") {
		t.Errorf("Get().Color = %q, want deterministic default %q even with a custom icon", info.Color, DefaultColor("report"))
	}
}

func TestFiles(t *testing.T) {
	canvasesDir := t.TempDir()
	metaDir := t.TempDir()
	if _, err := Create(canvasesDir, metaDir, "c1", "", "", ""); err != nil {
		t.Fatalf("Create: %v", err)
	}
	dir := Dir(canvasesDir, "c1")
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>hi</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "assets", "js"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assets", "js", "app.js"), []byte("x=1"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, err := Files(canvasesDir, "c1")
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	// Two regular files, sorted by path, directories omitted, slash-separated.
	if len(files) != 2 {
		t.Fatalf("got %d files, want 2: %+v", len(files), files)
	}
	if files[0].Path != "assets/js/app.js" || files[1].Path != "index.html" {
		t.Errorf("paths = %q, %q; want assets/js/app.js, index.html", files[0].Path, files[1].Path)
	}
	if files[1].Size != int64(len("<h1>hi</h1>")) {
		t.Errorf("index.html size = %d, want %d", files[1].Size, len("<h1>hi</h1>"))
	}
	if files[0].ModifiedAt.IsZero() {
		t.Error("modified_at is zero")
	}
}

func TestFilesEmptyCanvas(t *testing.T) {
	canvasesDir := t.TempDir()
	if _, err := Create(canvasesDir, t.TempDir(), "c1", "", "", ""); err != nil {
		t.Fatal(err)
	}
	files, err := Files(canvasesDir, "c1")
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("got %d files, want 0", len(files))
	}
}

func TestFilesMissingCanvas(t *testing.T) {
	if _, err := Files(t.TempDir(), "nope"); err == nil {
		t.Error("Files on a missing canvas: err = nil, want an error")
	}
}

func TestCopyMeta(t *testing.T) {
	metaDir := t.TempDir()
	canvasesDir := t.TempDir()
	// Source has explicit title + icon.
	if _, err := Create(canvasesDir, metaDir, "from", "My Title", "desc", "🎯"); err != nil {
		t.Fatal(err)
	}
	if err := CopyMeta(metaDir, "from", "to"); err != nil {
		t.Fatalf("CopyMeta: %v", err)
	}
	// The target must exist to read Info; create its dir.
	if _, err := Create(canvasesDir, metaDir, "to", "", "", ""); err != nil {
		t.Fatal(err)
	}
	info, err := Get(canvasesDir, metaDir, "to")
	if err != nil {
		t.Fatal(err)
	}
	if info.Title != "My Title" || info.Icon != "🎯" || info.Description != "desc" {
		t.Errorf("copied meta = %+v, want title/desc/icon carried", info)
	}
}

func TestCopyMetaNoSourceMetaClearsTarget(t *testing.T) {
	metaDir := t.TempDir()
	canvasesDir := t.TempDir()
	// Source has NO explicit meta; target has stale meta.
	if _, err := Create(canvasesDir, metaDir, "from", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(canvasesDir, metaDir, "to", "Stale", "", "📌"); err != nil {
		t.Fatal(err)
	}
	if err := CopyMeta(metaDir, "from", "to"); err != nil {
		t.Fatalf("CopyMeta: %v", err)
	}
	// The stale metadata file must be gone.
	if _, err := os.Stat(metaPath(metaDir, "to")); !os.IsNotExist(err) {
		t.Errorf("target meta file still exists (err=%v), want it cleared", err)
	}
}
