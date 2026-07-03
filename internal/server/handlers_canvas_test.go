package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHandleCanvasRendersIndexMarkdown(t *testing.T) {
	s, ts := newTestServer(t)
	canvasDir := filepath.Join(s.canvasesDir, "notes")
	if err := os.MkdirAll(canvasDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canvasDir, "index.md"), []byte("# Hello Markdown\n\nSome *body* text."), 0o644); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(ts.URL + "/c/notes/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /c/notes/ status = %d, want 200", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "<h1>Hello Markdown</h1>") {
		t.Errorf("response missing rendered markdown heading: %s", body)
	}
	if !strings.Contains(body, "<em>body</em>") {
		t.Errorf("response missing rendered markdown emphasis: %s", body)
	}
	if !strings.Contains(body, "scrim:skeleton") {
		t.Errorf("response missing skeleton wrapper: %s", body)
	}
	if !strings.Contains(body, "/c/notes/__events") {
		t.Errorf("response missing injected SSE script: %s", body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html prefix", ct)
	}
}

func TestHandleCanvasDirectMarkdownRequestServedRaw(t *testing.T) {
	// A direct request for a non-index .md file is not rendered -- it's
	// served as a raw static file, same as today, per the non-goal in the
	// design (only index.md-as-directory-index renders).
	s, ts := newTestServer(t)
	canvasDir := filepath.Join(s.canvasesDir, "notes2")
	if err := os.MkdirAll(canvasDir, 0o755); err != nil {
		t.Fatal(err)
	}
	source := "# Raw Please\n"
	if err := os.WriteFile(filepath.Join(canvasDir, "notes.md"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(ts.URL + "/c/notes2/notes.md")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	body := readBody(t, resp)
	if body != source {
		t.Errorf("direct .md request body = %q, want raw file content %q", body, source)
	}
	if strings.Contains(body, "scrim:skeleton") {
		t.Errorf("direct .md request should not be wrapped in skeleton: %s", body)
	}
}

func TestHandleCanvasWrapsHTMLFragment(t *testing.T) {
	s, ts := newTestServer(t)
	canvasDir := filepath.Join(s.canvasesDir, "fragment")
	if err := os.MkdirAll(canvasDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fragment := "<h1>Just a fragment</h1><p>no doctype or html tag here</p>"
	if err := os.WriteFile(filepath.Join(canvasDir, "index.html"), []byte(fragment), 0o644); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(ts.URL + "/c/fragment/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	body := readBody(t, resp)
	if !strings.Contains(body, "Just a fragment") {
		t.Errorf("response missing fragment content: %s", body)
	}
	if !strings.Contains(body, "scrim:skeleton") {
		t.Errorf("response missing skeleton wrapper: %s", body)
	}
	if !strings.Contains(body, `name="viewport"`) {
		t.Errorf("response missing viewport meta tag: %s", body)
	}
	if !strings.Contains(body, "/c/fragment/__events") {
		t.Errorf("response missing injected SSE script: %s", body)
	}
}

func TestHandleCanvasCompleteDocumentNotWrapped(t *testing.T) {
	s, ts := newTestServer(t)
	canvasDir := filepath.Join(s.canvasesDir, "complete")
	if err := os.MkdirAll(canvasDir, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := "<!doctype html>\n<html><head><title>t</title></head><body><h1>Complete</h1></body></html>"
	if err := os.WriteFile(filepath.Join(canvasDir, "index.html"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(ts.URL + "/c/complete/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	body := readBody(t, resp)

	// A complete document must be byte-identical to what the pre-existing
	// (pre-skeleton-wrapping) reload-script injection alone would produce
	// from the raw file -- no skeleton wrapping, no goldmark involvement,
	// nothing else touching it.
	want := string(injectReloadScript([]byte(doc), "complete"))
	if body != want {
		t.Errorf("complete document body = %q, want exactly injectReloadScript(original, id) = %q", body, want)
	}
}
