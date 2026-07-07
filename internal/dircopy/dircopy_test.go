package dircopy

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

const (
	testMaxBytes   = 50 * 1024 * 1024
	testMaxEntries = 1000
)

func TestCopyRoundTrip(t *testing.T) {
	src := t.TempDir()
	// A nested tree with two files.
	mustWrite(t, filepath.Join(src, "index.html"), "<h1>hi</h1>")
	mustWrite(t, filepath.Join(src, "assets", "app.js"), "console.log(1)")

	dst := filepath.Join(t.TempDir(), "copy")
	if err := Copy(src, dst, testMaxBytes, testMaxEntries); err != nil {
		t.Fatalf("Copy: %v", err)
	}

	if got := mustRead(t, filepath.Join(dst, "index.html")); got != "<h1>hi</h1>" {
		t.Errorf("index.html = %q", got)
	}
	if got := mustRead(t, filepath.Join(dst, "assets", "app.js")); got != "console.log(1)" {
		t.Errorf("assets/app.js = %q", got)
	}
	// Copied files are 0o644 regardless of the source mode.
	fi, err := os.Stat(filepath.Join(dst, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("perm = %o, want 0644", fi.Mode().Perm())
	}
}

func TestCopyIntoExistingEmptyDir(t *testing.T) {
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "f.txt"), "data")
	dst := t.TempDir() // already exists, empty
	if err := Copy(src, dst, testMaxBytes, testMaxEntries); err != nil {
		t.Fatalf("Copy into existing empty dir: %v", err)
	}
	if got := mustRead(t, filepath.Join(dst, "f.txt")); got != "data" {
		t.Errorf("f.txt = %q", got)
	}
}

func TestCopyRefusesSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is privileged on Windows")
	}
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "real.txt"), "x")
	if err := os.Symlink("real.txt", filepath.Join(src, "link.txt")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "copy")
	err := Copy(src, dst, testMaxBytes, testMaxEntries)
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("err = %v, want ErrUnsupported", err)
	}
}

func TestCopyEntryCap(t *testing.T) {
	src := t.TempDir()
	for i := 0; i < 5; i++ {
		mustWrite(t, filepath.Join(src, string(rune('a'+i))+".txt"), "x")
	}
	dst := filepath.Join(t.TempDir(), "copy")
	err := Copy(src, dst, testMaxBytes, 3)
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("err = %v, want ErrTooLarge (entry cap)", err)
	}
}

func TestCopyByteCap(t *testing.T) {
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "big.txt"), "0123456789")
	dst := filepath.Join(t.TempDir(), "copy")
	err := Copy(src, dst, 4, testMaxEntries)
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("err = %v, want ErrTooLarge (byte cap)", err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // test path
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
