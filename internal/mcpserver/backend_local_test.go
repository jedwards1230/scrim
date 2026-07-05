package mcpserver

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jedwards1230/scrim/internal/config"
)

func TestLocalBackendReadWriteRoundTrip(t *testing.T) {
	cfg := config.Config{Dir: t.TempDir(), Host: "127.0.0.1", Port: 7799}
	b := newLocalBackend(cfg)
	ctx := context.Background()

	// The canvas must exist first (writeFile requires it).
	dir := filepath.Join(cfg.CanvasesDir(), "c1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir canvas: %v", err)
	}

	const content = "<h1>local round trip</h1>"
	if err := b.WriteFile(ctx, "c1", "index.html", []byte(content)); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// Written atomically to the real on-disk location.
	onDisk, err := os.ReadFile(filepath.Join(dir, "index.html"))
	if err != nil {
		t.Fatalf("read on-disk file: %v", err)
	}
	if string(onDisk) != content {
		t.Errorf("on-disk content = %q, want %q", onDisk, content)
	}

	// Nested write creates parent dirs.
	if err := b.WriteFile(ctx, "c1", "assets/js/app.js", []byte("x=1")); err != nil {
		t.Fatalf("nested WriteFile: %v", err)
	}
	got, err := b.ReadFile(ctx, "c1", "assets/js/app.js")
	if err != nil {
		t.Fatalf("ReadFile nested: %v", err)
	}
	if string(got) != "x=1" {
		t.Errorf("nested ReadFile = %q, want x=1", got)
	}
}

// TestLocalBackendEditFileRoundTrip proves write → edit → read: the edit
// lands atomically at the real on-disk location and reports its replacement
// count, for both the single-hit and replace_all shapes.
func TestLocalBackendEditFileRoundTrip(t *testing.T) {
	cfg := config.Config{Dir: t.TempDir(), Host: "127.0.0.1", Port: 7799}
	b := newLocalBackend(cfg)
	ctx := context.Background()

	dir := filepath.Join(cfg.CanvasesDir(), "c1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir canvas: %v", err)
	}
	if err := b.WriteFile(ctx, "c1", "index.html", []byte("<h1>alpha</h1><p>beta beta</p>")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	info, err := b.EditFile(ctx, "c1", "index.html", "alpha", "gamma", false)
	if err != nil {
		t.Fatalf("EditFile: %v", err)
	}
	if info.Path != "index.html" || info.Replacements != 1 {
		t.Errorf("EditFile = %+v, want path index.html, 1 replacement", info)
	}

	info, err = b.EditFile(ctx, "c1", "index.html", "beta", "delta", true)
	if err != nil {
		t.Fatalf("EditFile replace_all: %v", err)
	}
	if info.Replacements != 2 {
		t.Errorf("replace_all replacements = %d, want 2", info.Replacements)
	}

	got, err := b.ReadFile(ctx, "c1", "index.html")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if want := "<h1>gamma</h1><p>delta delta</p>"; string(got) != want {
		t.Errorf("edited content = %q, want %q", got, want)
	}
}

// TestLocalBackendEditFileErrors covers the non-conflict error paths the
// backend owns (fileedit.Apply's own table lives in internal/fileedit):
// canvas-must-exist, file-must-exist, and traversal rejection.
func TestLocalBackendEditFileErrors(t *testing.T) {
	cfg := config.Config{Dir: t.TempDir(), Host: "127.0.0.1", Port: 7799}
	b := newLocalBackend(cfg)
	ctx := context.Background()

	if _, err := b.EditFile(ctx, "ghost", "index.html", "a", "b", false); err == nil {
		t.Error("EditFile in missing canvas error = nil, want an error")
	}

	dir := filepath.Join(cfg.CanvasesDir(), "c1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir canvas: %v", err)
	}
	if _, err := b.EditFile(ctx, "c1", "nope.html", "a", "b", false); err == nil {
		t.Error("EditFile of missing file error = nil, want an error (edit never creates)")
	}
	for _, p := range []string{"../secret.txt", "a/../../secret.txt", "/etc/passwd"} {
		if _, err := b.EditFile(ctx, "c1", p, "a", "b", false); err == nil {
			t.Errorf("EditFile(%q) error = nil, want traversal rejection", p)
		}
	}
}

func TestLocalBackendWriteRequiresExistingCanvas(t *testing.T) {
	cfg := config.Config{Dir: t.TempDir(), Host: "127.0.0.1", Port: 7799}
	b := newLocalBackend(cfg)
	if err := b.WriteFile(context.Background(), "ghost", "index.html", []byte("x")); err == nil {
		t.Fatal("WriteFile to missing canvas error = nil, want an error")
	}
}

func TestLocalBackendReadTraversalRejected(t *testing.T) {
	cfg := config.Config{Dir: t.TempDir(), Host: "127.0.0.1", Port: 7799}
	b := newLocalBackend(cfg)
	dir := filepath.Join(cfg.CanvasesDir(), "c1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir canvas: %v", err)
	}
	// Plant a secret one level above the canvas root; a traversal must not
	// reach it.
	if err := os.WriteFile(filepath.Join(cfg.CanvasesDir(), "secret.txt"), []byte("nope"), 0o644); err != nil {
		t.Fatalf("plant secret: %v", err)
	}
	for _, p := range []string{"../secret.txt", "a/../../secret.txt", "/etc/passwd"} {
		if _, err := b.ReadFile(context.Background(), "c1", p); err == nil {
			t.Errorf("ReadFile(%q) error = nil, want traversal rejection", p)
		}
	}
}

func TestLocalBackendWriteSizeCap(t *testing.T) {
	cfg := config.Config{Dir: t.TempDir(), Host: "127.0.0.1", Port: 7799}
	b := newLocalBackend(cfg)
	dir := filepath.Join(cfg.CanvasesDir(), "c1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir canvas: %v", err)
	}
	if err := b.WriteFile(context.Background(), "c1", "big.txt", make([]byte, maxFileBytes+1)); err == nil {
		t.Fatal("oversize WriteFile error = nil, want a cap rejection")
	}
}
