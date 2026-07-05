package mcpserver

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jedwards1230/scrim/internal/config"
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
