package server

import (
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/scrim/internal/canvas"
)

// writeCanvas creates a canvas directory with an index.html holding body.
func writeCanvas(t *testing.T, s *Server, id, body string) {
	t.Helper()
	dir := filepath.Join(s.canvasesDir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestCanvasShellRendersAtRoot pins the shell's contract on the default
// daemon: the canvas root serves scrim's own chrome (title, menu, the
// always-available items) around an iframe pointing at __raw/.
func TestCanvasShellRendersAtRoot(t *testing.T) {
	s, ts := newTestServer(t)
	if _, err := canvas.Create(s.canvasesDir, s.metaDir, "shell1", "My Shell", "", "", ""); err != nil {
		t.Fatal(err)
	}
	writeCanvas(t, s, "shell1", "<html><body>inner content</body></html>")

	resp, err := http.Get(ts.URL + "/c/shell1/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /c/shell1/ status = %d, want 200", resp.StatusCode)
	}
	body := readBody(t, resp)

	for _, want := range []string{
		`<h1 title="My Shell">My Shell</h1>`, // the canvas title in the bar
		`src="/c/shell1/__raw/"`,             // the iframe points at the raw canvas
		`id="menu-btn"`,                      // a real button, not a div
		`aria-expanded="false"`,              // with announced state
		`>Version history<`,                  // always-present menu items
		`>Refresh<`,
		`href="/">All artifacts<`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("shell missing %q", want)
		}
	}

	// The canvas's own content must NOT be inlined -- it lives in the iframe.
	if strings.Contains(body, "inner content") {
		t.Error("shell inlined the canvas content; it must be framed, not embedded")
	}
}

// TestCanvasShellCarriesNoReloadScript is the whole point of rendering the
// shell outside writeHTML: if the shell also opened an EventSource, every
// viewer would hold two SSE connections to the same canvas (doubling the
// gallery's viewer count and the hub's per-canvas caps) and every agent write
// would reload the outer page out from under an open menu.
func TestCanvasShellCarriesNoReloadScript(t *testing.T) {
	s, ts := newTestServer(t)
	writeCanvas(t, s, "noreload", "<html><body>hi</body></html>")

	resp, err := http.Get(ts.URL + "/c/noreload/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body := readBody(t, resp)

	for _, forbidden := range []string{"__events", "EventSource"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("shell contains %q -- it must carry no live-reload script", forbidden)
		}
	}

	// ...while the framed canvas still does.
	rawResp, err := http.Get(ts.URL + "/c/noreload/__raw/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rawResp.Body.Close() }()
	rawBody := readBody(t, rawResp)
	if !strings.Contains(rawBody, "/c/noreload/__events") {
		t.Errorf("framed canvas missing its injected SSE script: %s", rawBody)
	}
	if !strings.Contains(rawBody, "hi") {
		t.Errorf("framed canvas missing its content: %s", rawBody)
	}
}

// TestCanvasRawServesWhatTheRootUsedTo proves the URL split moved the old
// behavior rather than changing it: __raw/ is byte-identical to what the
// canvas root returned before the shell existed.
func TestCanvasRawServesWhatTheRootUsedTo(t *testing.T) {
	s, ts := newTestServer(t)
	doc := "<!doctype html>\n<html><head><title>t</title></head><body><h1>Doc</h1></body></html>"
	writeCanvas(t, s, "same", doc)

	resp, err := http.Get(ts.URL + "/c/same/__raw/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if got, want := readBody(t, resp), string(injectReloadScript([]byte(doc), "same")); got != want {
		t.Errorf("__raw body = %q, want the pre-shell canvas-root body %q", got, want)
	}
}

// TestCanvasDeepPathsBypassTheShell pins the "only the exact root" rule: a
// deeper path is still served raw, with no chrome around it.
func TestCanvasDeepPathsBypassTheShell(t *testing.T) {
	s, ts := newTestServer(t)
	writeCanvas(t, s, "deep", "<html><body>root</body></html>")
	if err := os.WriteFile(filepath.Join(s.canvasesDir, "deep", "page2.html"),
		[]byte("<!doctype html><html><body>page two</body></html>"), 0o644); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(ts.URL + "/c/deep/page2.html")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body := readBody(t, resp)
	if !strings.Contains(body, "page two") {
		t.Errorf("deep path body = %q, want the raw file's content", body)
	}
	if strings.Contains(body, `id="menu-btn"`) {
		t.Error("a deep canvas path must not be wrapped in the shell")
	}
}

// TestCanvasShellDisabledItemsRenderDisabled pins the two menu entries with no
// backend behind them: they are visible (so the surface is discoverable) but
// inert and labelled as such.
func TestCanvasShellDisabledItemsRenderDisabled(t *testing.T) {
	s, ts := newTestServer(t)
	writeCanvas(t, s, "disabled", "<html><body>x</body></html>")

	resp, err := http.Get(ts.URL + "/c/disabled/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body := readBody(t, resp)

	for _, item := range []string{"Rename", "Pin"} {
		want := `disabled aria-disabled="true" title="Not yet supported">` + item + `<`
		if !strings.Contains(body, want) {
			t.Errorf("%s menu item is not rendered disabled (looking for %q)", item, want)
		}
	}
}

// TestCanvasShellNoIdentityChromeWithoutOIDC mirrors the gallery's rule (see
// TestIndexNoIdentityChromeWithoutOIDC): with no OIDC the shell still renders,
// but nothing identity-flavored appears.
func TestCanvasShellNoIdentityChromeWithoutOIDC(t *testing.T) {
	s, _ := newHubTestServer(t, []string{"127.0.0.0/8"}, "")
	writeCanvas(t, s, "plain", "<html><body>x</body></html>")

	req := httptest.NewRequest(http.MethodGet, "/c/plain/", nil)
	req.Header.Set("Accept", "text/html")
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /c/plain/ (non-OIDC hub) = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	// The non-identity chrome is still there.
	for _, want := range []string{`id="menu-btn"`, `>Refresh<`, `>Version history<`, `All artifacts`} {
		if !strings.Contains(body, want) {
			t.Errorf("non-OIDC shell missing %q", want)
		}
	}
	for _, unwanted := range []string{`class="chip"`, `data-share=`, `data-duplicate=`, `id="share-dialog"`, "Artifact by"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("non-OIDC shell unexpectedly contains %q", unwanted)
		}
	}
}

// shellGET fetches a canvas shell as a browser would, optionally carrying a
// session cookie.
func shellGET(t *testing.T, s *Server, id string, cookie *http.Cookie) (string, int) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/c/"+id+"/", nil)
	req.Header.Set("Accept", "text/html")
	req.RemoteAddr = "127.0.0.1:12345"
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	return rec.Body.String(), rec.Code
}

// TestCanvasShellIdentityChromeUnderOIDC proves the ownership line, the
// principal chip, Share, and Duplicate appear for a canvas's owner -- and that
// a viewer who only has a VIEW grant gets the ownership line naming the owner
// but neither write affordance (the copy endpoint would refuse them anyway).
func TestCanvasShellIdentityChromeUnderOIDC(t *testing.T) {
	s, auth, idp := newOIDCHub(t)
	ownedCanvas(t, s, "alices", "alice@example.com")
	if err := canvas.AddGrant(s.metaDir, "alices", canvas.Grant{Kind: canvas.GrantUser, Target: "bob@example.com"}); err != nil {
		t.Fatal(err)
	}

	alice := sessionFor(t, auth, idp, "sub-alice", "alice@example.com", nil)
	body, code := shellGET(t, s, "alices", alice)
	if code != http.StatusOK {
		t.Fatalf("GET /c/alices/ (alice) = %d, want 200", code)
	}
	for _, want := range []string{
		"Artifact by you",
		"Updated ",
		`class="chip"`,
		`data-share="alices"`,
		`data-duplicate="alices"`,
		`id="share-dialog"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("owner's shell missing %q", want)
		}
	}

	bob := sessionFor(t, auth, idp, "sub-bob", "bob@example.com", nil)
	bobBody, bobCode := shellGET(t, s, "alices", bob)
	if bobCode != http.StatusOK {
		t.Fatalf("GET /c/alices/ (bob, view grant) = %d, want 200", bobCode)
	}
	if !strings.Contains(bobBody, "Artifact by alice@example.com") {
		t.Error("a shared viewer's shell should name the owner")
	}
	for _, unwanted := range []string{`data-share="alices"`, `data-duplicate="alices"`} {
		if strings.Contains(bobBody, unwanted) {
			t.Errorf("a view-only viewer must not get %q", unwanted)
		}
	}
}

// TestCanvasShellRawPathIsPerCanvasGated proves the __raw route inherits the
// per-canvas visibility check, which is why it lives under /c/: a canvas bob
// can't see is a 404 through the frame path too, not a general authenticated
// read.
func TestCanvasShellRawPathIsPerCanvasGated(t *testing.T) {
	s, auth, idp := newOIDCHub(t)
	ownedCanvas(t, s, "secret", "alice@example.com")

	bob := sessionFor(t, auth, idp, "sub-bob", "bob@example.com", nil)
	req := httptest.NewRequest(http.MethodGet, "/c/secret/__raw/", nil)
	req.Header.Set("Accept", "text/html")
	req.AddCookie(bob)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("bob GET /c/secret/__raw/ = %d, want 404", rec.Code)
	}
}

// TestCanvasShellEscapesTitle proves a hostile canvas title cannot break out of
// the three attribute/text contexts the shell puts it in. html/template's
// contextual escaper is what makes that true; this pins it, since canvas.Create
// accepts an arbitrary title string and a canvas is agent-authored.
func TestCanvasShellEscapesTitle(t *testing.T) {
	s, ts := newTestServer(t)
	const evil = `" onload="alert(1)`
	if _, err := canvas.Create(s.canvasesDir, s.metaDir, "evil", evil, "", "", ""); err != nil {
		t.Fatal(err)
	}
	writeCanvas(t, s, "evil", "<html><body>x</body></html>")

	resp, err := http.Get(ts.URL + "/c/evil/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body := readBody(t, resp)

	if strings.Contains(body, `onload="alert(1)`) {
		t.Errorf("a quote-bearing title escaped its attribute context:\n%s", body)
	}
	if !strings.Contains(body, "&#34; onload=&#34;alert(1)") {
		t.Errorf("the title is not rendered in its escaped form:\n%s", body)
	}
}

// TestCanvasShellForwardsLinkSecretToTheFrame proves a link-grant viewer's
// iframe actually resolves: the shell request authenticated with ?k=<secret>,
// and the framed request is gated per-canvas the same way, so the secret has to
// ride along -- exactly once, not double-encoded.
func TestCanvasShellForwardsLinkSecretToTheFrame(t *testing.T) {
	s, _, _ := newOIDCHub(t)
	ownedCanvas(t, s, "linked", "alice@example.com")

	// A secret with characters that must be percent-encoded in a query string,
	// so a missing or doubled encoding both show up.
	const secret = "abc+def/ghi=" //nolint:gosec // a test fixture, not a credential
	if err := canvas.AddGrant(s.metaDir, "linked", canvas.Grant{
		Kind:           canvas.GrantLink,
		LinkID:         "l1",
		LinkSecretHash: canvas.HashLinkSecret(secret),
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/c/linked/?k="+url.QueryEscape(secret), nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("link-grant GET /c/linked/ = %d, want 200", rec.Code)
	}

	// Pull the iframe src back out of the rendered page and request it: if the
	// secret were dropped or double-encoded, the gate would 302/404 this.
	body := rec.Body.String()
	const marker = `<iframe id="canvas-frame" class="frame" src="`
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("no iframe in the shell:\n%s", body)
	}
	rest := body[i+len(marker):]
	rawSrc := html.UnescapeString(rest[:strings.IndexByte(rest, '"')])

	frameReq := httptest.NewRequest(http.MethodGet, rawSrc, nil)
	frameReq.Header.Set("Accept", "text/html")
	frameRec := httptest.NewRecorder()
	s.routes().ServeHTTP(frameRec, frameReq)
	if frameRec.Code != http.StatusOK {
		t.Fatalf("framed GET %s = %d, want 200 (the link secret must reach the frame)", rawSrc, frameRec.Code)
	}
}

// TestCanvasRawServesNestedPaths pins the route-precedence claim: a literal
// __raw segment beats the /c/{id}/{rest...} wildcard for nested paths too, and
// a traversal attempt out of __raw is still refused.
func TestCanvasRawServesNestedPaths(t *testing.T) {
	s, ts := newTestServer(t)
	writeCanvas(t, s, "nested", "<html><body>root</body></html>")
	if err := os.MkdirAll(filepath.Join(s.canvasesDir, "nested", "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.canvasesDir, "nested", "assets", "app.js"),
		[]byte("console.log('hi')"), 0o644); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(ts.URL + "/c/nested/__raw/assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if body := readBody(t, resp); body != "console.log('hi')" {
		t.Errorf("nested __raw asset body = %q, want the file's content", body)
	}

	// Traversal out of the canvas root is refused on the __raw route exactly as
	// it is on the plain one (resolveServablePath is the same guard).
	esc, err := http.Get(ts.URL + "/c/nested/__raw/..%2f..%2f..%2fetc%2fpasswd")
	if err != nil {
		t.Fatal(err)
	}
	_ = esc.Body.Close()
	if esc.StatusCode != http.StatusNotFound {
		t.Errorf("traversal through __raw status = %d, want 404", esc.StatusCode)
	}
}

// TestRelativeAge is a table check on the shell's coarse "updated" label.
func TestRelativeAge(t *testing.T) {
	tests := []struct {
		name string
		age  time.Duration
		want string
	}{
		{"future mtime clamps to zero", -5 * time.Hour, "just now"},
		{"seconds", 30 * time.Second, "just now"},
		{"minutes", 90 * time.Second, "1m ago"},
		{"hours", 3 * time.Hour, "3h ago"},
		{"days", 50 * time.Hour, "2d ago"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := relativeAge(tt.age); got != tt.want {
				t.Errorf("relativeAge(%v) = %q, want %q", tt.age, got, tt.want)
			}
		})
	}
}
