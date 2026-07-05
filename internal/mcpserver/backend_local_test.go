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
