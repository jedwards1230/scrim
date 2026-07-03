package server

import (
	"archive/tar"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/scrim/internal/config"
)

// buildTar writes a tar archive from entries directly into a bytes.Buffer,
// bypassing safeJoin entirely, so a malicious entry (path traversal,
// absolute path, symlink) can be constructed exactly as an attacker would
// send it over the wire.
func buildTar(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: e.typeflag,
			Mode:     0o644,
			Size:     int64(len(e.body)),
			Linkname: e.linkname,
		}
		if e.typeflag == tar.TypeDir {
			hdr.Mode = 0o755
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("WriteHeader(%q): %v", e.name, err)
		}
		if len(e.body) > 0 {
			if _, err := tw.Write(e.body); err != nil {
				t.Fatalf("Write(%q): %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}
	return buf.Bytes()
}

type tarEntry struct {
	name     string
	typeflag byte
	body     []byte
	linkname string
}

func regFile(name, body string) tarEntry {
	return tarEntry{name: name, typeflag: tar.TypeReg, body: []byte(body)}
}

func dirEntry(name string) tarEntry {
	return tarEntry{name: name, typeflag: tar.TypeDir}
}

func TestExtractTarRejectsUnsafeEntries(t *testing.T) {
	tests := []struct {
		name    string
		entries []tarEntry
	}{
		{
			name:    "parent traversal",
			entries: []tarEntry{regFile("../escape.txt", "x")},
		},
		{
			name:    "nested parent traversal",
			entries: []tarEntry{regFile("a/../../escape.txt", "x")},
		},
		{
			name:    "absolute path",
			entries: []tarEntry{regFile("/etc/passwd", "x")},
		},
		{
			name: "symlink entry",
			entries: []tarEntry{
				{name: "link", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
			},
		},
		{
			name: "hardlink entry",
			entries: []tarEntry{
				{name: "link", typeflag: tar.TypeLink, linkname: "some-other-file"},
			},
		},
		{
			name: "fifo entry",
			entries: []tarEntry{
				{name: "fifo", typeflag: tar.TypeFifo},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			data := buildTar(t, tt.entries)
			err := extractTar(bytes.NewReader(data), root, maxPushBytes, maxPushEntries)
			if err == nil {
				t.Fatal("extractTar() error = nil, want an error for an unsafe entry")
			}
			// Nothing should have escaped root regardless of the specific
			// error returned.
			entries, _ := os.ReadDir(root)
			for _, e := range entries {
				if e.Name() == "escape.txt" {
					t.Errorf("extractTar() wrote %q, which should never have been created", e.Name())
				}
			}
		})
	}
}

func TestExtractTarEnforcesSizeCap(t *testing.T) {
	root := t.TempDir()
	data := buildTar(t, []tarEntry{regFile("big.txt", strings.Repeat("x", 100))})
	err := extractTar(bytes.NewReader(data), root, 50, maxPushEntries)
	if err == nil {
		t.Fatal("extractTar() error = nil, want an error when the archive exceeds the byte cap")
	}
}

func TestExtractTarEnforcesFileCountCap(t *testing.T) {
	root := t.TempDir()
	entries := make([]tarEntry, 0, 5)
	for i := 0; i < 5; i++ {
		entries = append(entries, regFile(strings.Repeat("f", i+1)+".txt", "x"))
	}
	data := buildTar(t, entries)
	err := extractTar(bytes.NewReader(data), root, maxPushBytes, 3)
	if err == nil {
		t.Fatal("extractTar() error = nil, want an error when the archive exceeds the file-count cap")
	}
}

// TestExtractTarCountsDirEntries proves the entry cap counts directory
// entries, not just regular files -- an archive of many empty directories
// must trip the same cap a file-bomb would (regression for the review
// finding that only regular files were counted, letting a dir-only archive
// bypass maxPushEntries entirely).
func TestExtractTarCountsDirEntries(t *testing.T) {
	root := t.TempDir()
	entries := make([]tarEntry, 0, 5)
	for i := 0; i < 5; i++ {
		entries = append(entries, dirEntry(strings.Repeat("d", i+1)))
	}
	data := buildTar(t, entries)
	err := extractTar(bytes.NewReader(data), root, maxPushBytes, 3)
	if err == nil {
		t.Fatal("extractTar() error = nil, want an error when directory entries exceed the entry-count cap")
	}
}

func TestExtractTarHappyPath(t *testing.T) {
	root := t.TempDir()
	entries := []tarEntry{
		dirEntry("assets"),
		regFile("index.html", "<html><body>hi</body></html>"),
		regFile("assets/app.js", "console.log('hi')"),
	}
	data := buildTar(t, entries)
	if err := extractTar(bytes.NewReader(data), root, maxPushBytes, maxPushEntries); err != nil {
		t.Fatalf("extractTar() error = %v", err)
	}

	got, err := os.ReadFile(filepath.Join(root, "index.html"))
	if err != nil {
		t.Fatalf("reading extracted index.html: %v", err)
	}
	if string(got) != "<html><body>hi</body></html>" {
		t.Errorf("index.html content = %q", got)
	}
	got, err = os.ReadFile(filepath.Join(root, "assets", "app.js"))
	if err != nil {
		t.Fatalf("reading extracted assets/app.js: %v", err)
	}
	if string(got) != "console.log('hi')" {
		t.Errorf("assets/app.js content = %q", got)
	}
}

// newHubTestServer returns a Server in hub mode wired for httptest, with a
// known push token and CIDR allowlist so tests can assert on both gates
// deterministically.
func newHubTestServer(t *testing.T, allow []string, readToken string) (*Server, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Config{
		Dir:         dir,
		Host:        "127.0.0.1",
		Port:        0,
		IdleTimeout: time.Hour,
		NoAuth:      true,
	}
	s, err := NewHub(cfg, HubOptions{
		PushToken:  "test-push-token",
		ReadToken:  readToken,
		AllowCIDRs: allow,
	})
	if err != nil {
		t.Fatalf("NewHub() error = %v", err)
	}
	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)
	return s, ts
}

func TestNewHubFailsClosedWithoutPushToken(t *testing.T) {
	_, err := NewHub(config.Config{Dir: t.TempDir()}, HubOptions{})
	if err == nil {
		t.Fatal("NewHub() with no push token error = nil, want an error")
	}
}

func TestNewHubRejectsMalformedCIDR(t *testing.T) {
	_, err := NewHub(config.Config{Dir: t.TempDir()}, HubOptions{
		PushToken: "tok", AllowCIDRs: []string{"not-a-cidr"},
	})
	if err == nil {
		t.Fatal("NewHub() with a malformed CIDR error = nil, want an error")
	}
}

func TestHubPushRoutePresentAndGated(t *testing.T) {
	_, ts := newHubTestServer(t, []string{"127.0.0.0/8", "::1/128"}, "")

	data := buildTar(t, []tarEntry{regFile("index.html", "hello hub")})

	// Missing bearer token: 401.
	resp, err := http.Post(ts.URL+"/api/push/report", "application/x-tar", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("push with no bearer token status = %d, want 401", resp.StatusCode)
	}

	// Wrong bearer token: 401.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/push/report", bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer wrong-token")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("push with wrong bearer token status = %d, want 401", resp.StatusCode)
	}

	// Valid bearer token: 200, and the canvas is actually created.
	req2, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/push/report?title=Hub+Test", bytes.NewReader(data))
	req2.Header.Set("Authorization", "Bearer test-push-token")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp2.Body.Close() }()
	body, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("push with valid bearer token status = %d, want 200, body: %s", resp2.StatusCode, body)
	}
	if !strings.Contains(string(body), `"/c/report/"`) {
		t.Errorf("push response body = %s, want it to contain the canvas URL", body)
	}

	// Now a read (GET) from an allowed CIDR (loopback, the httptest server
	// always connects from 127.0.0.1) should serve the pushed content.
	getResp, err := http.Get(ts.URL + "/c/report/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = getResp.Body.Close() }()
	getBody := readBody(t, getResp)
	if !strings.Contains(getBody, "hello hub") {
		t.Errorf("GET /c/report/ body = %q, want it to contain the pushed content", getBody)
	}
}

func TestHubReadGateCIDR(t *testing.T) {
	s, ts := newHubTestServer(t, []string{"10.0.0.0/8"}, "")
	_ = ts // ts.URL isn't used for the CIDR-mismatch assertion below; kept for cleanup registration

	// A loopback connection (what httptest.Server always uses) is NOT
	// inside 10.0.0.0/8, so a real request through ts should be 403.
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("GET / from loopback against a 10.0.0.0/8-only allowlist status = %d, want 403", resp.StatusCode)
	}

	// Directly exercise the gate with a synthetic in-range RemoteAddr to
	// prove the allowlist itself accepts a genuinely in-range client.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.1.2.3:5555"
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("GET / from an in-allowlist RemoteAddr status = %d, want 200", rec.Code)
	}
}

func TestHubReadGateOptionalReadToken(t *testing.T) {
	_, ts := newHubTestServer(t, []string{"127.0.0.0/8", "::1/128"}, "hub-read-token")

	// In-CIDR but no read token: 401.
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET / with no read token (read token configured) status = %d, want 401", resp.StatusCode)
	}

	// Valid read token as a query param: redirects (302), same UX as
	// withAuth's browser-facing flow.
	client := noRedirectClient()
	resp2, err := client.Get(ts.URL + "/?t=hub-read-token")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusFound {
		t.Errorf("GET /?t=<valid read token> status = %d, want 302", resp2.StatusCode)
	}
}

func TestHubReadGateNoReadTokenConfigured(t *testing.T) {
	_, ts := newHubTestServer(t, []string{"127.0.0.0/8", "::1/128"}, "")

	// In-CIDR, no read token configured at all: reads pass with no token
	// needed.
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET / with CIDR allowed and no read token configured status = %d, want 200", resp.StatusCode)
	}
}
