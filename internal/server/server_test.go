package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/scrim/internal/apiclient"
	"github.com/jedwards1230/scrim/internal/config"
)

func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Config{
		Dir:         dir,
		Host:        "127.0.0.1",
		Port:        0,
		IdleTimeout: time.Hour,
	}
	s := New(cfg)
	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)
	return s, ts
}

func TestHandleCanvasServesFileAndInjectsScript(t *testing.T) {
	s, ts := newTestServer(t)
	if _, err := os.Stat(s.canvasesDir); err != nil {
		if err := os.MkdirAll(s.canvasesDir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	canvasDir := filepath.Join(s.canvasesDir, "report")
	if err := os.MkdirAll(canvasDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canvasDir, "index.html"), []byte("<html><body>hello</body></html>"), 0o644); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(ts.URL + "/c/report/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	body := readBody(t, resp)
	if !strings.Contains(body, "hello") {
		t.Errorf("response missing original content: %s", body)
	}
	if !strings.Contains(body, "/c/report/__events") {
		t.Errorf("response missing injected SSE script pointing at the canvas events endpoint: %s", body)
	}
	if !strings.Contains(body, "<script>") {
		t.Errorf("response missing injected <script> tag: %s", body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html prefix", ct)
	}
}

func TestHandleCanvasPathTraversalReturns404(t *testing.T) {
	_, ts := newTestServer(t)

	tests := []string{
		"/c/report/../../../etc/passwd",
		"/c/report/..%2f..%2f..%2fetc%2fpasswd",
	}
	for _, path := range tests {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404", path, resp.StatusCode)
		}
	}
}

func TestHandleCanvasNoDirectoryListing(t *testing.T) {
	s, ts := newTestServer(t)
	canvasDir := filepath.Join(s.canvasesDir, "empty")
	if err := os.MkdirAll(canvasDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canvasDir, "notes.txt"), []byte("secret notes"), 0o644); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(ts.URL + "/c/empty/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /c/empty/ (no index.html) status = %d, want 404", resp.StatusCode)
	}
}

func TestAPICanvasLifecycle(t *testing.T) {
	_, ts := newTestServer(t)
	client := apiclient.New(ts.URL)
	ctx := t.Context()

	created, err := client.CreateCanvas(ctx, "report", "My Report")
	if err != nil {
		t.Fatalf("CreateCanvas() error = %v", err)
	}
	if created.ID != "report" || created.Title != "My Report" {
		t.Errorf("CreateCanvas() = %+v", created)
	}

	list, err := client.ListCanvases(ctx)
	if err != nil {
		t.Fatalf("ListCanvases() error = %v", err)
	}
	if len(list) != 1 || list[0].ID != "report" {
		t.Fatalf("ListCanvases() = %+v", list)
	}

	if err := client.DeleteCanvas(ctx, "report"); err != nil {
		t.Fatalf("DeleteCanvas() error = %v", err)
	}

	list, err = client.ListCanvases(ctx)
	if err != nil {
		t.Fatalf("ListCanvases() after delete error = %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("ListCanvases() after delete = %+v, want empty", list)
	}

	if err := client.DeleteCanvas(ctx, "report"); !apiclient.IsNotFound(err) {
		t.Errorf("DeleteCanvas() on missing canvas error = %v, want 404", err)
	}
}

func TestAPICreateCanvasRejectsInvalidID(t *testing.T) {
	_, ts := newTestServer(t)
	client := apiclient.New(ts.URL)
	_, err := client.CreateCanvas(t.Context(), "../escape", "")
	if err == nil {
		t.Fatal("CreateCanvas() with traversal id error = nil, want error")
	}
}

func TestAPIStatus(t *testing.T) {
	_, ts := newTestServer(t)
	client := apiclient.New(ts.URL)

	resp, err := client.Status(t.Context())
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if resp.CanvasCount != 0 {
		t.Errorf("Status().CanvasCount = %d, want 0", resp.CanvasCount)
	}
	if !resp.Active {
		t.Error("Status().Active = false immediately after startup, want true")
	}
}

// TestAPIStatusActiveWithReapingDisabled guards against Active going
// spuriously false whenever --idle-timeout <= 0 (reaping disabled): with no
// idle timeout to compare against, the daemon should always report active
// rather than "idleSeconds < a non-positive timeout", which is never true.
func TestAPIStatusActiveWithReapingDisabled(t *testing.T) {
	cfg := config.Config{
		Dir:         t.TempDir(),
		Host:        "127.0.0.1",
		Port:        0,
		IdleTimeout: 0,
	}
	s := New(cfg)
	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)

	client := apiclient.New(ts.URL)
	resp, err := client.Status(t.Context())
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if !resp.Active {
		t.Error("Status().Active = false with idle-timeout disabled, want true")
	}
}

func TestIndexPageListsCanvases(t *testing.T) {
	_, ts := newTestServer(t)
	client := apiclient.New(ts.URL)
	if _, err := client.CreateCanvas(t.Context(), "report", "My Report"); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body := readBody(t, resp)
	if !strings.Contains(body, "report") {
		t.Errorf("index page missing canvas id: %s", body)
	}
	if !strings.Contains(body, "My Report") {
		t.Errorf("index page missing canvas title: %s", body)
	}
}

func TestCanvasRedirectAddsTrailingSlash(t *testing.T) {
	s, ts := newTestServer(t)
	canvasDir := filepath.Join(s.canvasesDir, "report")
	if err := os.MkdirAll(canvasDir, 0o755); err != nil {
		t.Fatal(err)
	}

	httpClient := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := httpClient.Get(ts.URL + "/c/report")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMovedPermanently {
		t.Errorf("GET /c/report status = %d, want 301", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/c/report/" {
		t.Errorf("Location = %q, want /c/report/", loc)
	}
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return sb.String()
}

func TestSSEEndpointTakesPrecedenceOverStaticWildcard(t *testing.T) {
	// "__events" is a literal final segment, which net/http's ServeMux
	// treats as more specific than the "/c/{id}/{rest...}" wildcard route,
	// so it must dispatch to the SSE handler rather than trying (and
	// 404ing) to serve a static file named "__events".
	s, ts := newTestServer(t)
	canvasDir := filepath.Join(s.canvasesDir, "report")
	if err := os.MkdirAll(canvasDir, 0o755); err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/c/report/__events", nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream (SSE handler should win over static wildcard)", ct)
	}
}

func TestWriteJSONHelpers(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSONError(rec, http.StatusBadRequest, "bad input")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "bad input" {
		t.Errorf("body = %+v", body)
	}
}
