package mcpserver

import (
	"bytes"
	"context"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/scrim/internal/config"
	"github.com/jedwards1230/scrim/internal/fileedit"
	scrimserver "github.com/jedwards1230/scrim/internal/server"
)

// newHubBackendAgainstRealHub stands up a real in-process server.NewHub on a
// t.TempDir() behind httptest and returns a hubBackend pointed at it with the
// matching push token. This is the wire-contract test: it proves Step 1's
// machine API and Step 2's hubBackend agree on paths, methods, auth, and
// payload shapes -- a drift on either side fails here.
func newHubBackendAgainstRealHub(t *testing.T) *hubBackend {
	t.Helper()
	const token = "integration-push-token"
	cfg := config.Config{Dir: t.TempDir(), Host: "127.0.0.1", Port: 0, IdleTimeout: time.Hour, NoAuth: true}
	s, err := scrimserver.NewHub(cfg, scrimserver.HubOptions{PushToken: token, AllowCIDRs: nil})
	if err != nil {
		t.Fatalf("server.NewHub: %v", err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return newHubBackend(ts.URL, token)
}

func TestHubBackendRoundTrip(t *testing.T) {
	ctx := context.Background()
	b := newHubBackendAgainstRealHub(t)

	// Add.
	info, err := b.Add(ctx, "c1", "Title", "Desc", "")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if info.ID != "c1" {
		t.Fatalf("Add id = %q, want c1", info.ID)
	}
	if info.URL != b.baseURL+"/c/c1/" {
		t.Errorf("Add URL = %q, want %q", info.URL, b.baseURL+"/c/c1/")
	}

	// Write → Read round-trip, including a nested path.
	const content = "<h1>hello over the wire</h1>"
	if err := b.WriteFile(ctx, "c1", "index.html", []byte(content)); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := b.WriteFile(ctx, "c1", "assets/app.js", []byte("console.log(1)")); err != nil {
		t.Fatalf("WriteFile nested: %v", err)
	}
	got, err := b.ReadFile(ctx, "c1", "index.html")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != content {
		t.Errorf("ReadFile = %q, want %q", got, content)
	}
	nested, err := b.ReadFile(ctx, "c1", "assets/app.js")
	if err != nil {
		t.Fatalf("ReadFile nested: %v", err)
	}
	if string(nested) != "console.log(1)" {
		t.Errorf("nested ReadFile = %q", nested)
	}

	// List sees the canvas, with a client-reachable URL.
	list, err := b.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].ID != "c1" {
		t.Fatalf("List = %+v, want one canvas c1", list)
	}
	if list[0].URL != b.baseURL+"/c/c1/" {
		t.Errorf("List URL = %q, want %q", list[0].URL, b.baseURL+"/c/c1/")
	}

	// Snapshot the current content.
	snap, err := b.Snap(ctx, "c1", "first")
	if err != nil {
		t.Fatalf("Snap: %v", err)
	}
	if snap.Name == "" {
		t.Fatal("Snap name empty")
	}
	snaps, err := b.Snaps(ctx, "c1")
	if err != nil {
		t.Fatalf("Snaps: %v", err)
	}
	if len(snaps) != 1 || snaps[0].Name != snap.Name || snaps[0].Label != "first" {
		t.Fatalf("Snaps = %+v, want one entry %q labeled first", snaps, snap.Name)
	}

	// Mutate, then revert-to-latest (empty name resolves via Snaps).
	if err := b.WriteFile(ctx, "c1", "index.html", []byte("changed")); err != nil {
		t.Fatalf("WriteFile mutate: %v", err)
	}
	rev, err := b.Revert(ctx, "c1", "")
	if err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if rev.Reverted != "c1" || rev.Snapshot != snap.Name {
		t.Errorf("Revert = %+v, want reverted c1 to %q", rev, snap.Name)
	}
	after, err := b.ReadFile(ctx, "c1", "index.html")
	if err != nil {
		t.Fatalf("ReadFile after revert: %v", err)
	}
	if string(after) != content {
		t.Errorf("post-revert content = %q, want original %q", after, content)
	}

	// Status reports a running hub.
	st, err := b.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Running {
		t.Error("Status.Running = false, want true against a live hub")
	}

	// Link is a pure function of base + id.
	links, err := b.Link(ctx, "c1")
	if err != nil {
		t.Fatalf("Link: %v", err)
	}
	if len(links) != 1 || links[0] != b.baseURL+"/c/c1/" {
		t.Errorf("Link = %v, want [%s/c/c1/]", links, b.baseURL)
	}

	// Remove, then List is empty.
	if err := b.Remove(ctx, "c1"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	list, err = b.List(ctx)
	if err != nil {
		t.Fatalf("List after remove: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("List after remove = %+v, want empty", list)
	}
}

// TestHubBackendEditFileRoundTrip proves write → edit → read over the wire
// against a real hub, including replace_all, and that conflict errors (409:
// old_string absent / ambiguous) surface with the hub's path-free message.
func TestHubBackendEditFileRoundTrip(t *testing.T) {
	ctx := context.Background()
	b := newHubBackendAgainstRealHub(t)

	if _, err := b.Add(ctx, "c1", "", "", ""); err != nil {
		t.Fatalf("Add: %v", err)
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

	// Conflict errors surface the hub's helpful, path-free 409 messages.
	if _, err := b.EditFile(ctx, "c1", "index.html", "absent", "x", false); err == nil {
		t.Error("EditFile with absent old_string error = nil, want an error")
	} else if !strings.Contains(err.Error(), "old_string not found in file") {
		t.Errorf("absent old_string error = %q, want it to contain the not-found message", err)
	}
	if _, err := b.EditFile(ctx, "c1", "index.html", "delta", "x", false); err == nil {
		t.Error("EditFile with ambiguous old_string error = nil, want an error")
	} else if !strings.Contains(err.Error(), "occurs 2 times") {
		t.Errorf("ambiguous old_string error = %q, want it to name the count", err)
	}
}

// TestHubBackendWriteRequiresExistingCanvas confirms the wire error path: a
// write to a canvas that was never added surfaces the hub's 404 as an error.
func TestHubBackendWriteRequiresExistingCanvas(t *testing.T) {
	b := newHubBackendAgainstRealHub(t)
	if err := b.WriteFile(context.Background(), "ghost", "index.html", []byte("x")); err == nil {
		t.Fatal("WriteFile to missing canvas error = nil, want an error")
	}
}

// TestHubBackendReadMissingFile confirms a read of a non-existent file is an
// error, not empty content.
func TestHubBackendReadMissingFile(t *testing.T) {
	b := newHubBackendAgainstRealHub(t)
	if _, err := b.Add(context.Background(), "c1", "", "", ""); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := b.ReadFile(context.Background(), "c1", "nope.html"); err == nil {
		t.Fatal("ReadFile missing file error = nil, want an error")
	}
}

// TestHubBackendClientSideTraversalGuard confirms hubBackend refuses a
// traversal path locally, before any request is sent.
func TestHubBackendClientSideTraversalGuard(t *testing.T) {
	b := newHubBackend("http://127.0.0.1:1", "tok") // never actually dialed
	for _, p := range []string{"../escape.txt", "a/../../escape.txt", "/etc/passwd", ""} {
		if _, err := b.ReadFile(context.Background(), "c1", p); err == nil {
			t.Errorf("ReadFile(%q) error = nil, want a client-side rejection", p)
		}
		if err := b.WriteFile(context.Background(), "c1", p, []byte("x")); err == nil {
			t.Errorf("WriteFile(%q) error = nil, want a client-side rejection", p)
		}
		if _, err := b.EditFile(context.Background(), "c1", p, "a", "b", false); err == nil {
			t.Errorf("EditFile(%q) error = nil, want a client-side rejection", p)
		}
	}
}

// TestHubBackendWriteSizeCap confirms an oversize write is rejected client-side
// before crossing the wire.
func TestHubBackendWriteSizeCap(t *testing.T) {
	b := newHubBackend("http://127.0.0.1:1", "tok")
	big := make([]byte, maxFileBytes+1)
	if err := b.WriteFile(context.Background(), "c1", "big.txt", big); err == nil {
		t.Fatal("oversize WriteFile error = nil, want a client-side cap rejection")
	}
}

// TestBackendsCanonicalizeNonCanonicalPaths is the F-series regression test
// for the ServeMux-301 trap: a non-canonical path like "./index.html" or
// "a//b" must behave identically in local and hub mode -- canonicalized by
// cleanRelPath before it touches disk or a URL, so a hub-mode PUT/PATCH can
// never be silently degraded into a redirect-followed GET.
func TestBackendsCanonicalizeNonCanonicalPaths(t *testing.T) {
	ctx := context.Background()

	localCfg := config.Config{Dir: t.TempDir(), Host: "127.0.0.1", Port: 7799}
	if err := os.MkdirAll(filepath.Join(localCfg.CanvasesDir(), "c1"), 0o755); err != nil {
		t.Fatal(err)
	}
	local := newLocalBackend(localCfg)

	hub := newHubBackendAgainstRealHub(t)
	if _, err := hub.Add(ctx, "c1", "", "", ""); err != nil {
		t.Fatalf("hub Add: %v", err)
	}

	for name, b := range map[string]backend{"local": local, "hub": hub} {
		t.Run(name, func(t *testing.T) {
			// write via "./x.html" reads back at the canonical "x.html".
			if err := b.WriteFile(ctx, "c1", "./x.html", []byte("one")); err != nil {
				t.Fatalf("WriteFile ./x.html: %v", err)
			}
			got, err := b.ReadFile(ctx, "c1", "x.html")
			if err != nil {
				t.Fatalf("ReadFile x.html: %v", err)
			}
			if string(got) != "one" {
				t.Errorf("ReadFile x.html = %q, want one", got)
			}

			// write via "a//b.html" reads back at the canonical "a/b.html".
			if err := b.WriteFile(ctx, "c1", "a//b.html", []byte("two")); err != nil {
				t.Fatalf("WriteFile a//b.html: %v", err)
			}
			got, err = b.ReadFile(ctx, "c1", "a/b.html")
			if err != nil {
				t.Fatalf("ReadFile a/b.html: %v", err)
			}
			if string(got) != "two" {
				t.Errorf("ReadFile a/b.html = %q, want two", got)
			}

			// edit via the non-canonical spelling really lands (write_file's
			// silent-GET failure mode would have left "one" untouched), and
			// reports the canonical path back.
			info, err := b.EditFile(ctx, "c1", "./x.html", "one", "uno", false)
			if err != nil {
				t.Fatalf("EditFile ./x.html: %v", err)
			}
			if info.Path != "x.html" || info.Replacements != 1 {
				t.Errorf("EditFile = %+v, want canonical path x.html with 1 replacement", info)
			}
			got, err = b.ReadFile(ctx, "c1", "x.html")
			if err != nil {
				t.Fatalf("ReadFile after edit: %v", err)
			}
			if string(got) != "uno" {
				t.Errorf("edited content = %q, want uno", got)
			}
		})
	}
}

// TestHubBackendRefusesRedirects proves the CheckRedirect guard: a hub (or
// intermediary) answering with a redirect is an error, never a silently
// followed body-less GET.
func TestHubBackendRefusesRedirects(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusMovedPermanently)
	}))
	t.Cleanup(ts.Close)

	b := newHubBackend(ts.URL, "tok")
	if err := b.WriteFile(context.Background(), "c1", "x.html", []byte("x")); err == nil {
		t.Error("WriteFile through a redirecting hub error = nil, want a refusal")
	} else if !strings.Contains(err.Error(), "redirect") {
		t.Errorf("WriteFile redirect error = %q, want it to name the refused redirect", err)
	}
	if _, err := b.EditFile(context.Background(), "c1", "x.html", "a", "b", false); err == nil {
		t.Error("EditFile through a redirecting hub error = nil, want a refusal")
	}
}

func TestHubBackendListFiles(t *testing.T) {
	ctx := context.Background()
	b := newHubBackendAgainstRealHub(t)
	if _, err := b.Add(ctx, "c1", "", "", ""); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := b.WriteFile(ctx, "c1", "index.html", []byte("<h1>hi</h1>")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := b.WriteFile(ctx, "c1", "assets/app.js", []byte("x=1")); err != nil {
		t.Fatalf("WriteFile nested: %v", err)
	}
	files, err := b.ListFiles(ctx, "c1")
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(files) != 2 || files[0].Path != "assets/app.js" || files[1].Path != "index.html" {
		t.Errorf("files = %+v, want sorted [assets/app.js, index.html]", files)
	}
}

func TestHubBackendEditFileBatch(t *testing.T) {
	ctx := context.Background()
	b := newHubBackendAgainstRealHub(t)
	if _, err := b.Add(ctx, "c1", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := b.WriteFile(ctx, "c1", "index.html", []byte("alpha beta gamma")); err != nil {
		t.Fatal(err)
	}
	info, err := b.EditFileBatch(ctx, "c1", "index.html", []fileedit.Edit{
		{OldString: "alpha", NewString: "one"},
		{OldString: "gamma", NewString: "three"},
	})
	if err != nil {
		t.Fatalf("EditFileBatch: %v", err)
	}
	if info.Replacements != 2 {
		t.Errorf("replacements = %d, want 2", info.Replacements)
	}
	got, err := b.ReadFile(ctx, "c1", "index.html")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "one beta three" {
		t.Errorf("content = %q, want %q", got, "one beta three")
	}

	// A failing batch surfaces a 409-derived error and leaves the file alone.
	if _, err := b.EditFileBatch(ctx, "c1", "index.html", []fileedit.Edit{
		{OldString: "one", NewString: "X"},
		{OldString: "missing", NewString: "Y"},
	}); err == nil {
		t.Fatal("failing batch err = nil, want an error")
	}
	got, _ = b.ReadFile(ctx, "c1", "index.html")
	if string(got) != "one beta three" {
		t.Errorf("file changed after failed batch = %q", got)
	}
}

func TestHubBackendCopyCanvas(t *testing.T) {
	ctx := context.Background()
	b := newHubBackendAgainstRealHub(t)
	if _, err := b.Add(ctx, "src", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := b.WriteFile(ctx, "src", "index.html", []byte("SRC")); err != nil {
		t.Fatal(err)
	}
	info, err := b.CopyCanvas(ctx, "src", "dst", false)
	if err != nil {
		t.Fatalf("CopyCanvas: %v", err)
	}
	if info.To != "dst" || info.URL != b.baseURL+"/c/dst/" {
		t.Errorf("info = %+v, want To=dst URL=%s/c/dst/", info, b.baseURL)
	}
	got, err := b.ReadFile(ctx, "dst", "index.html")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "SRC" {
		t.Errorf("dst content = %q, want SRC", got)
	}
	// Conflict without overwrite.
	if _, err := b.CopyCanvas(ctx, "src", "dst", false); err == nil {
		t.Error("copy onto existing target: err = nil, want a 409-derived error")
	}
}

// TestHubBackendGzipWireRoundTrip proves a large file survives the gzip wire
// path (Content-Encoding on PUT, Accept-Encoding on GET) byte-for-byte.
func TestHubBackendGzipWireRoundTrip(t *testing.T) {
	ctx := context.Background()
	b := newHubBackendAgainstRealHub(t)
	if _, err := b.Add(ctx, "c1", "", "", ""); err != nil {
		t.Fatal(err)
	}
	raw := []byte(strings.Repeat("<section>data</section>", 2000)) // well over the gzip threshold
	if err := b.WriteFile(ctx, "c1", "index.html", raw); err != nil {
		t.Fatalf("WriteFile (gzip PUT): %v", err)
	}
	got, err := b.ReadFile(ctx, "c1", "index.html")
	if err != nil {
		t.Fatalf("ReadFile (gzip GET): %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Errorf("gzip wire round-trip mismatch (%d vs %d bytes)", len(got), len(raw))
	}
}

// TestHubBackendNearCapIncompressibleRoundTrip is a regression test for the
// gzip-expansion bug: the hub gzips any file over its threshold even when
// compression doesn't help, so an at-cap incompressible file comes back
// slightly larger than the plain cap. The client's compressed-read budget must
// allow that expansion instead of truncating the stream.
func TestHubBackendNearCapIncompressibleRoundTrip(t *testing.T) {
	ctx := context.Background()
	b := newHubBackendAgainstRealHub(t)
	if _, err := b.Add(ctx, "c1", "", "", ""); err != nil {
		t.Fatal(err)
	}
	// Exactly at the per-file cap, and incompressible (random) so gzip expands
	// it rather than shrinking it.
	raw := make([]byte, maxFileBytes)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	if err := b.WriteFile(ctx, "c1", "blob.bin", raw); err != nil {
		t.Fatalf("WriteFile at cap: %v", err)
	}
	got, err := b.ReadFile(ctx, "c1", "blob.bin")
	if err != nil {
		t.Fatalf("ReadFile at cap: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Errorf("near-cap incompressible round-trip mismatch (%d vs %d bytes)", len(got), len(raw))
	}
}
