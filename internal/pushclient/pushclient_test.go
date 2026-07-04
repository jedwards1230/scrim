package pushclient

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// unpack extracts a tar archive (as produced by Pack) into dir, for
// asserting Pack's output round-trips.
func unpack(t *testing.T, data []byte, dir string) {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatalf("reading tar entry: %v", err)
		}
		target := filepath.Join(dir, filepath.FromSlash(hdr.Name))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				t.Fatal(err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				t.Fatal(err)
			}
			f, err := os.Create(target) //nolint:gosec // test-only, target is under t.TempDir()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.Copy(f, tr); err != nil {
				t.Fatal(err)
			}
			_ = f.Close()
		default:
			t.Fatalf("unpack: unexpected tar entry type %q for %q", string(hdr.Typeflag), hdr.Name)
		}
	}
}

func TestPackRoundTrips(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "index.html"), "<html><body>hi</body></html>")
	writeFile(t, filepath.Join(src, "assets", "app.js"), "console.log('hi')")
	writeFile(t, filepath.Join(src, "assets", "css", "style.css"), "body{}")

	data, err := Pack(src)
	if err != nil {
		t.Fatalf("Pack() error = %v", err)
	}

	dst := t.TempDir()
	unpack(t, data, dst)

	for _, rel := range []string{"index.html", "assets/app.js", "assets/css/style.css"} {
		want, err := os.ReadFile(filepath.Join(src, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("reading source %s: %v", rel, err)
		}
		got, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("reading unpacked %s: %v", rel, err)
		}
		if string(got) != string(want) {
			t.Errorf("%s round-trip mismatch: got %q, want %q", rel, got, want)
		}
	}
}

func TestPackSkipsSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevated privileges on windows")
	}
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "real.txt"), "real content")
	if err := os.Symlink(filepath.Join(src, "real.txt"), filepath.Join(src, "link.txt")); err != nil {
		t.Fatalf("creating symlink: %v", err)
	}

	data, err := Pack(src)
	if err != nil {
		t.Fatalf("Pack() error = %v", err)
	}

	tr := tar.NewReader(bytes.NewReader(data))
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading tar entry: %v", err)
		}
		names = append(names, hdr.Name)
	}
	for _, n := range names {
		if n == "link.txt" {
			t.Errorf("Pack() included a symlink entry %q, want it skipped", n)
		}
	}
	found := false
	for _, n := range names {
		if n == "real.txt" {
			found = true
		}
	}
	if !found {
		t.Error("Pack() did not include the regular file real.txt")
	}
}

func TestPushSuccessReturnsHubURL(t *testing.T) {
	var gotAuth, gotTitle, gotMethod, gotPath string
	var gotBody []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotTitle = r.URL.Query().Get("title")
		gotMethod = r.Method
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "report", "url": "/c/report/"})
	}))
	defer ts.Close()

	data := []byte("fake tar bytes")
	url, err := Push(t.Context(), ts.URL, "report", "sekret", "My Title", "", "", data)
	if err != nil {
		t.Fatalf("Push() error = %v", err)
	}
	if want := ts.URL + "/c/report/"; url != want {
		t.Errorf("Push() = %q, want %q", url, want)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("request method = %q, want POST", gotMethod)
	}
	if gotPath != "/api/push/report" {
		t.Errorf("request path = %q, want /api/push/report", gotPath)
	}
	if gotAuth != "Bearer sekret" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer sekret")
	}
	if gotTitle != "My Title" {
		t.Errorf("title query param = %q, want %q", gotTitle, "My Title")
	}
	if string(gotBody) != "fake tar bytes" {
		t.Errorf("request body = %q, want %q", gotBody, "fake tar bytes")
	}
}

func TestPushNon2xxReturnsErrorWithBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized: missing or invalid push token", http.StatusUnauthorized)
	}))
	defer ts.Close()

	_, err := Push(t.Context(), ts.URL, "report", "wrong-token", "", "", "", []byte("x"))
	if err == nil {
		t.Fatal("Push() error = nil, want an error for a 401 response")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("missing or invalid push token")) {
		t.Errorf("Push() error = %v, want it to include the response body", err)
	}
}
