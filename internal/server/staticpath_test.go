package server

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestResolveStaticPath(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "index.html"), "<html></html>")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(root, "sub", "page.html"), "<html></html>")
	mustWriteFile(t, filepath.Join(root, ".scrim.json"), `{"title":"x"}`)

	outside := t.TempDir()
	mustWriteFile(t, filepath.Join(outside, "secret.txt"), "nope")

	tests := []struct {
		name    string
		subpath string
		wantErr error
	}{
		{"plain file", "index.html", nil},
		{"nested file", "sub/page.html", nil},
		{"dotdot traversal clamped", "../../../../../../etc/passwd", errOutsideRoot},
		{"dotdot mid path", "sub/../../etc/passwd", errOutsideRoot},
		{"dotfile blocked", ".scrim.json", errOutsideRoot},
		{"dotfile in subdir blocked", "sub/.hidden", errOutsideRoot},
		{"absolute path style clamped under root", "/etc/passwd", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveStaticPath(root, tt.subpath)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("resolveStaticPath(%q) error = %v, want %v", tt.subpath, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveStaticPath(%q) unexpected error = %v", tt.subpath, err)
			}
			rootAbs, _ := filepath.Abs(root)
			if !isWithin(got, rootAbs) {
				t.Fatalf("resolveStaticPath(%q) = %q, escapes root %q", tt.subpath, got, rootAbs)
			}
		})
	}

	t.Run("symlink escape rejected", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlink creation requires elevated privileges on windows")
		}
		linkPath := filepath.Join(root, "escape")
		if err := os.Symlink(outside, linkPath); err != nil {
			t.Fatal(err)
		}
		_, err := resolveStaticPath(root, "escape/secret.txt")
		if !errors.Is(err, errOutsideRoot) {
			t.Fatalf("resolveStaticPath(symlink escape) error = %v, want errOutsideRoot", err)
		}
	})
}

func TestResolveServablePathSymlinkedIndexRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevated privileges on windows")
	}

	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	mustWriteFile(t, secret, "top secret")

	canvasDir := filepath.Join(root, "escape-dir")
	if err := os.MkdirAll(canvasDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// index.html is itself a symlink pointing outside canvasRoot. The
	// directory path ("escape-dir") is legitimate; only the file the
	// directory-fallback synthesizes escapes.
	if err := os.Symlink(secret, filepath.Join(canvasDir, "index.html")); err != nil {
		t.Fatal(err)
	}

	_, _, err := resolveServablePath(root, "escape-dir")
	if err == nil {
		t.Fatal("resolveServablePath(symlinked index.html) error = nil, want error (escape rejected)")
	}
	if !errors.Is(err, errOutsideRoot) {
		t.Fatalf("resolveServablePath(symlinked index.html) error = %v, want errOutsideRoot", err)
	}
}

func TestResolveServablePathSymlinkedIndexMdRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevated privileges on windows")
	}

	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	mustWriteFile(t, secret, "top secret")

	canvasDir := filepath.Join(root, "escape-dir-md")
	if err := os.MkdirAll(canvasDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// index.md is itself a symlink pointing outside canvasRoot. The
	// directory path ("escape-dir-md") is legitimate; only the file the
	// index.md fallback synthesizes escapes -- mirrors the index.html
	// symlink-escape regression test above for the index.md fallback path
	// added alongside it.
	if err := os.Symlink(secret, filepath.Join(canvasDir, "index.md")); err != nil {
		t.Fatal(err)
	}

	_, _, err := resolveServablePath(root, "escape-dir-md")
	if err == nil {
		t.Fatal("resolveServablePath(symlinked index.md) error = nil, want error (escape rejected)")
	}
	if !errors.Is(err, errOutsideRoot) {
		t.Fatalf("resolveServablePath(symlinked index.md) error = %v, want errOutsideRoot", err)
	}
}

func TestResolveServablePath(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "index.html"), "<html>root</html>")
	if err := os.MkdirAll(filepath.Join(root, "withindex"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(root, "withindex", "index.html"), "<html>sub</html>")
	if err := os.MkdirAll(filepath.Join(root, "noindex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "mdonly"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(root, "mdonly", "index.md"), "# hi")
	if err := os.MkdirAll(filepath.Join(root, "both"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(root, "both", "index.html"), "<html>both-html</html>")
	mustWriteFile(t, filepath.Join(root, "both", "index.md"), "# both-md")

	tests := []struct {
		name        string
		subpath     string
		wantErr     bool
		wantSuffix  string
		wantViaIdx  bool
		checkResult bool
	}{
		{name: "root maps to index.html", subpath: "", wantSuffix: "index.html", wantViaIdx: true, checkResult: true},
		{name: "trailing slash root", subpath: "/", wantSuffix: "index.html", wantViaIdx: true, checkResult: true},
		{name: "directory with index.html", subpath: "withindex", wantSuffix: filepath.Join("withindex", "index.html"), wantViaIdx: true, checkResult: true},
		{name: "directory with index.html trailing slash", subpath: "withindex/", wantSuffix: filepath.Join("withindex", "index.html"), wantViaIdx: true, checkResult: true},
		{name: "directory without index.html or index.md 404s", subpath: "noindex", wantErr: true},
		{name: "missing file 404s", subpath: "does-not-exist.html", wantErr: true},
		{name: "directory falls back to index.md when no index.html", subpath: "mdonly", wantSuffix: filepath.Join("mdonly", "index.md"), wantViaIdx: true, checkResult: true},
		{name: "index.html preferred over index.md when both exist", subpath: "both", wantSuffix: filepath.Join("both", "index.html"), wantViaIdx: true, checkResult: true},
		{name: "direct index.md request is not viaIndex", subpath: "mdonly/index.md", wantSuffix: filepath.Join("mdonly", "index.md"), wantViaIdx: false, checkResult: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, viaIndex, err := resolveServablePath(root, tt.subpath)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveServablePath(%q) error = nil, want error", tt.subpath)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveServablePath(%q) unexpected error = %v", tt.subpath, err)
			}
			if tt.checkResult {
				if !strings.HasSuffix(target, tt.wantSuffix) {
					t.Errorf("resolveServablePath(%q) target = %q, want suffix %q", tt.subpath, target, tt.wantSuffix)
				}
				if viaIndex != tt.wantViaIdx {
					t.Errorf("resolveServablePath(%q) viaIndex = %v, want %v", tt.subpath, viaIndex, tt.wantViaIdx)
				}
			}
		})
	}
}

func isWithin(target, root string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel == "." || (len(rel) > 0 && rel[0] != '.' && !filepath.IsAbs(rel))
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
