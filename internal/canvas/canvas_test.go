package canvas

import (
	"os"
	"path/filepath"
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

	canvasDir, err := Create(dir, "report", "My Report")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if canvasDir != Dir(dir, "report") {
		t.Errorf("Create() dir = %q, want %q", canvasDir, Dir(dir, "report"))
	}
	if !Exists(dir, "report") {
		t.Error("Exists() = false after Create()")
	}

	info, err := Get(dir, "report")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if info.Title != "My Report" {
		t.Errorf("Get().Title = %q, want %q", info.Title, "My Report")
	}

	if _, err := Create(dir, "untitled", ""); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	list, err := List(dir)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("List() len = %d, want 2", len(list))
	}
	if list[0].ID != "report" || list[1].ID != "untitled" {
		t.Errorf("List() not sorted by ID: %+v", list)
	}

	if err := Delete(dir, "report"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if Exists(dir, "report") {
		t.Error("Exists() = true after Delete()")
	}

	list, err = List(dir)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("List() after delete len = %d, want 1", len(list))
	}
}

func TestListMissingDir(t *testing.T) {
	list, err := List(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if list != nil {
		t.Errorf("List() = %+v, want nil", list)
	}
}

func TestListSkipsInvalidEntries(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "valid"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, ".hidden"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "not-a-dir"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	list, err := List(dir)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 1 || list[0].ID != "valid" {
		t.Errorf("List() = %+v, want only [valid]", list)
	}
}

func TestDeleteInvalidID(t *testing.T) {
	dir := t.TempDir()
	if err := Delete(dir, "../escape"); err == nil {
		t.Error("Delete() with traversal id should error")
	}
}

func TestLastModifiedReflectsNestedWrites(t *testing.T) {
	dir := t.TempDir()
	canvasDir, err := Create(dir, "report", "")
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

	info, err := Get(dir, "report")
	if err != nil {
		t.Fatal(err)
	}
	if info.ModTime.Before(old.Add(time.Minute)) {
		t.Errorf("Get().ModTime = %v, want it to reflect the nested file's recent write", info.ModTime)
	}
}
