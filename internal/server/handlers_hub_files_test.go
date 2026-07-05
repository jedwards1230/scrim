package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hubDo issues an authenticated (or, when token=="", unauthenticated) request
// against a hub test server and returns the response. body may be nil.
func hubDo(t *testing.T, method, url, token string, body []byte) *http.Response {
	t.Helper()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

const hubToken = "test-push-token"

// TestHubMachineAPIBearerGated is the CRITICAL fail-closed test: with
// AllowCIDRs nil (deny-all browser reads -- loopback would otherwise be
// allowed by a permissive allowlist and mask the gate), EVERY machine
// endpoint, reads included, must reject a request with no bearer token and
// accept one that carries the push token. This proves the machine API is
// bearer-gated end to end, not just for writes.
func TestHubMachineAPIBearerGated(t *testing.T) {
	_, ts := newHubTestServer(t, nil, "")

	// Seed one canvas + file + snapshot so the read endpoints have something
	// to return once authorized (all via the bearer path).
	resp := hubDo(t, http.MethodPost, ts.URL+"/api/canvases", hubToken, []byte(`{"id":"c1"}`))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("seed create canvas status = %d, want 201", resp.StatusCode)
	}
	resp = hubDo(t, http.MethodPut, ts.URL+"/api/canvases/c1/files/index.html", hubToken, []byte("<h1>hi</h1>"))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("seed write file status = %d, want 204", resp.StatusCode)
	}
	resp = hubDo(t, http.MethodPost, ts.URL+"/api/canvases/c1/snapshots", hubToken, []byte(`{"label":"seed"}`))
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("seed snapshot status = %d, want 201, body: %s", resp.StatusCode, body)
	}
	snapName := extractJSONField(t, body, "name")

	endpoints := []struct {
		name   string
		method string
		path   string
		body   []byte
		wantOK int
	}{
		{"status", http.MethodGet, "/api/status", nil, http.StatusOK},
		{"list canvases", http.MethodGet, "/api/canvases", nil, http.StatusOK},
		{"read file", http.MethodGet, "/api/canvases/c1/files/index.html", nil, http.StatusOK},
		{"write file", http.MethodPut, "/api/canvases/c1/files/notes.txt", []byte("x"), http.StatusNoContent},
		{"list snapshots", http.MethodGet, "/api/canvases/c1/snapshots", nil, http.StatusOK},
		{"create snapshot", http.MethodPost, "/api/canvases/c1/snapshots", []byte(`{}`), http.StatusCreated},
		{"revert snapshot", http.MethodPost, "/api/canvases/c1/snapshots/" + snapName + "/revert", nil, http.StatusOK},
	}

	for _, ep := range endpoints {
		t.Run(ep.name, func(t *testing.T) {
			// No bearer: fail closed. AllowCIDRs is nil, so a GET falls to the
			// browser read gate's CIDR check and is 403'd; a write with no
			// bearer is 401'd. Either way it's NOT served.
			noAuth := hubDo(t, ep.method, ts.URL+ep.path, "", ep.body)
			_ = noAuth.Body.Close()
			if noAuth.StatusCode != http.StatusUnauthorized && noAuth.StatusCode != http.StatusForbidden {
				t.Errorf("%s %s without bearer status = %d, want 401 or 403 (fail closed)", ep.method, ep.path, noAuth.StatusCode)
			}

			// With bearer: served.
			ok := hubDo(t, ep.method, ts.URL+ep.path, hubToken, ep.body)
			okBody, _ := io.ReadAll(ok.Body)
			_ = ok.Body.Close()
			if ok.StatusCode != ep.wantOK {
				t.Errorf("%s %s with bearer status = %d, want %d, body: %s", ep.method, ep.path, ok.StatusCode, ep.wantOK, okBody)
			}
		})
	}
}

// TestHubWriteReadRoundTrip proves an atomic write is durable and reads back
// byte-for-byte, and that a write creates nested parent directories under the
// canvas root.
func TestHubWriteReadRoundTrip(t *testing.T) {
	_, ts := newHubTestServer(t, nil, "")

	resp := hubDo(t, http.MethodPost, ts.URL+"/api/canvases", hubToken, []byte(`{"id":"c1"}`))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create canvas status = %d, want 201", resp.StatusCode)
	}

	const content = "console.log('nested')"
	resp = hubDo(t, http.MethodPut, ts.URL+"/api/canvases/c1/files/assets/js/app.js", hubToken, []byte(content))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("nested write status = %d, want 204", resp.StatusCode)
	}

	got := hubDo(t, http.MethodGet, ts.URL+"/api/canvases/c1/files/assets/js/app.js", hubToken, nil)
	gotBody, _ := io.ReadAll(got.Body)
	_ = got.Body.Close()
	if got.StatusCode != http.StatusOK {
		t.Fatalf("read back status = %d, want 200", got.StatusCode)
	}
	if string(gotBody) != content {
		t.Errorf("round-trip content = %q, want %q", gotBody, content)
	}
	if cc := got.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
}

// TestHubWriteRequiresExistingCanvas asserts a write to a canvas that was
// never added is a 404, not an implicit create.
func TestHubWriteRequiresExistingCanvas(t *testing.T) {
	_, ts := newHubTestServer(t, nil, "")
	resp := hubDo(t, http.MethodPut, ts.URL+"/api/canvases/ghost/files/index.html", hubToken, []byte("x"))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("write to missing canvas status = %d, want 404", resp.StatusCode)
	}
}

// TestHubReadMissingFile asserts a read of a non-existent file within an
// existing canvas is a 404.
func TestHubReadMissingFile(t *testing.T) {
	_, ts := newHubTestServer(t, nil, "")
	resp := hubDo(t, http.MethodPost, ts.URL+"/api/canvases", hubToken, []byte(`{"id":"c1"}`))
	_ = resp.Body.Close()

	resp = hubDo(t, http.MethodGet, ts.URL+"/api/canvases/c1/files/nope.html", hubToken, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("read missing file status = %d, want 404", resp.StatusCode)
	}
}

// TestHubWriteSizeCap asserts a body over maxFileBytes is rejected with 413.
func TestHubWriteSizeCap(t *testing.T) {
	_, ts := newHubTestServer(t, nil, "")
	resp := hubDo(t, http.MethodPost, ts.URL+"/api/canvases", hubToken, []byte(`{"id":"c1"}`))
	_ = resp.Body.Close()

	big := bytes.Repeat([]byte("x"), maxFileBytes+1)
	resp = hubDo(t, http.MethodPut, ts.URL+"/api/canvases/c1/files/big.txt", hubToken, big)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize write status = %d, want 413", resp.StatusCode)
	}
}

// TestHubFilePathTraversalOverWire proves a "..%2f"-encoded traversal payload,
// which decodes to "../" and DOES reach the handler with the path segment
// intact (unlike a literal "../" that the HTTP client collapses before
// sending), is rejected by safeJoin with a 400 -- and never escapes the canvas
// root onto disk.
func TestHubFilePathTraversalOverWire(t *testing.T) {
	s, ts := newHubTestServer(t, nil, "")
	resp := hubDo(t, http.MethodPost, ts.URL+"/api/canvases", hubToken, []byte(`{"id":"c1"}`))
	_ = resp.Body.Close()

	for _, method := range []string{http.MethodGet, http.MethodPut} {
		t.Run(method, func(t *testing.T) {
			var body []byte
			if method == http.MethodPut {
				body = []byte("payload")
			}
			resp := hubDo(t, method, ts.URL+"/api/canvases/c1/files/..%2f..%2fescape.txt", hubToken, body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("%s ..%%2f traversal status = %d, want 400", method, resp.StatusCode)
			}
		})
	}

	// And the escape never landed on disk, at either level the payload aimed
	// for (canvases root and the data dir above it).
	for _, p := range []string{
		filepath.Join(s.canvasesDir, "escape.txt"),
		filepath.Join(s.canvasesDir, "..", "escape.txt"),
	} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("traversal artifact exists at %s (stat err=%v), want absent", p, err)
		}
	}
}

// TestHubFileSymlinkEscapeRejected proves the resolved-containment check: a
// symlink planted inside a canvas dir (out-of-band -- push refuses symlink
// entries and PUT writes regular files) cannot be used to read or write
// outside the canvas root through the files API.
func TestHubFileSymlinkEscapeRejected(t *testing.T) {
	s, ts := newHubTestServer(t, nil, "")
	resp := hubDo(t, http.MethodPost, ts.URL+"/api/canvases", hubToken, []byte(`{"id":"c1"}`))
	_ = resp.Body.Close()

	outside := t.TempDir() // a directory entirely outside the hub data dir
	secret := filepath.Join(outside, "secret.html")
	if err := os.WriteFile(secret, []byte("outside"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	canvasDir := filepath.Join(s.canvasesDir, "c1")

	// A symlinked FILE inside the canvas pointing outside: reads must refuse.
	if err := os.Symlink(secret, filepath.Join(canvasDir, "leak.html")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	resp = hubDo(t, http.MethodGet, ts.URL+"/api/canvases/c1/files/leak.html", hubToken, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("GET through symlinked file status = %d, want 400", resp.StatusCode)
	}

	// A symlinked DIRECTORY inside the canvas pointing outside: writes through
	// it must refuse, and nothing may land in the outside dir.
	if err := os.Symlink(outside, filepath.Join(canvasDir, "evil")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	resp = hubDo(t, http.MethodPut, ts.URL+"/api/canvases/c1/files/evil/planted.txt", hubToken, []byte("x"))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("PUT through symlinked dir status = %d, want 400", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(outside, "planted.txt")); !os.IsNotExist(err) {
		t.Errorf("write escaped through symlink to %s, want absent", filepath.Join(outside, "planted.txt"))
	}
}

// TestHubFilePathTraversalAtHandler exercises safeJoin's rejection directly at
// the handler for the traversal payloads an HTTP client would otherwise
// collapse before they reach the wire (a literal "../" and an absolute path),
// by setting the path value the way ServeMux would.
func TestHubFilePathTraversalAtHandler(t *testing.T) {
	s, _ := newHubTestServer(t, nil, "")
	for _, tc := range []struct {
		name string
		path string
	}{
		{"parent traversal", "../escape.txt"},
		{"nested parent traversal", "a/../../escape.txt"},
		{"absolute path", "/etc/passwd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut, "/api/canvases/c1/files/x", bytes.NewReader([]byte("x")))
			req.SetPathValue("id", "c1")
			req.SetPathValue("path", tc.path)
			rec := httptest.NewRecorder()
			s.handleWriteCanvasFile(rec, req)
			if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
				t.Errorf("write %q status = %d, want 400 (or 404 for a missing canvas)", tc.path, rec.Code)
			}
		})
	}
}

// TestHubEmptyFilePathRejected asserts an empty path value is a 400 at the
// handler (the route can't match an empty {path...} over the wire, but the
// handler guards it explicitly).
func TestHubEmptyFilePathRejected(t *testing.T) {
	s, _ := newHubTestServer(t, nil, "")
	req := httptest.NewRequest(http.MethodGet, "/api/canvases/c1/files/", nil)
	req.SetPathValue("id", "c1")
	req.SetPathValue("path", "")
	rec := httptest.NewRecorder()
	s.handleReadCanvasFile(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty path status = %d, want 400", rec.Code)
	}
}

// extractJSONField pulls a top-level string field out of a small JSON object
// body without a struct, for tests that only need one value.
func extractJSONField(t *testing.T, body []byte, field string) string {
	t.Helper()
	key := `"` + field + `":"`
	i := strings.Index(string(body), key)
	if i < 0 {
		t.Fatalf("field %q not in body: %s", field, body)
	}
	rest := string(body)[i+len(key):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		t.Fatalf("unterminated field %q in body: %s", field, body)
	}
	return rest[:j]
}
