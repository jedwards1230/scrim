package server

import (
	"io"
	"net/http"
	"testing"

	"github.com/jedwards1230/scrim/internal/canvas"
	"github.com/jedwards1230/scrim/internal/snapshot"
)

// TestHubCopyCanvas proves POST /api/canvases/{id}/copy duplicates a canvas's
// files into a new one, and 409s when the target exists without overwrite.
func TestHubCopyCanvas(t *testing.T) {
	s, ts := newHubTestServer(t, nil, "")

	resp := hubDo(t, http.MethodPost, ts.URL+"/api/canvases", hubToken, []byte(`{"id":"src","title":"Original"}`))
	_ = resp.Body.Close()
	for _, f := range []struct{ path, body string }{
		{"index.html", "<h1>hi</h1>"},
		{"assets/app.js", "x=1"},
	} {
		r := hubDo(t, http.MethodPut, ts.URL+"/api/canvases/src/files/"+f.path, hubToken, []byte(f.body))
		_ = r.Body.Close()
	}

	copyResp := hubDo(t, http.MethodPost, ts.URL+"/api/canvases/src/copy", hubToken, []byte(`{"to":"dst"}`))
	cbody, _ := io.ReadAll(copyResp.Body)
	_ = copyResp.Body.Close()
	if copyResp.StatusCode != http.StatusOK {
		t.Fatalf("copy status = %d, want 200, body: %s", copyResp.StatusCode, cbody)
	}

	// The destination has both files, byte-for-byte.
	for _, f := range []struct{ path, want string }{
		{"index.html", "<h1>hi</h1>"},
		{"assets/app.js", "x=1"},
	} {
		got := hubDo(t, http.MethodGet, ts.URL+"/api/canvases/dst/files/"+f.path, hubToken, nil)
		gb, _ := io.ReadAll(got.Body)
		_ = got.Body.Close()
		if got.StatusCode != http.StatusOK || string(gb) != f.want {
			t.Errorf("dst %s = %q (status %d), want %q", f.path, gb, got.StatusCode, f.want)
		}
	}
	// Metadata carried across.
	info, err := canvas.Get(s.canvasesDir, s.metaDir, "dst")
	if err != nil {
		t.Fatalf("get dst: %v", err)
	}
	if info.Title != "Original" {
		t.Errorf("dst title = %q, want Original (metadata copied)", info.Title)
	}

	// Copy onto an existing target without overwrite -> 409.
	conflict := hubDo(t, http.MethodPost, ts.URL+"/api/canvases/src/copy", hubToken, []byte(`{"to":"dst"}`))
	_ = conflict.Body.Close()
	if conflict.StatusCode != http.StatusConflict {
		t.Errorf("copy onto existing target status = %d, want 409", conflict.StatusCode)
	}
}

// TestHubCopyCanvasOverwriteSnapshotsTarget proves overwrite replaces the
// target and snapshots it first (so the overwrite is undoable).
func TestHubCopyCanvasOverwriteSnapshotsTarget(t *testing.T) {
	s, ts := newHubTestServer(t, nil, "")

	// Source and a pre-existing target with different content.
	for _, id := range []string{"src", "dst"} {
		r := hubDo(t, http.MethodPost, ts.URL+"/api/canvases", hubToken, []byte(`{"id":"`+id+`"}`))
		_ = r.Body.Close()
	}
	w := hubDo(t, http.MethodPut, ts.URL+"/api/canvases/src/files/index.html", hubToken, []byte("SOURCE"))
	_ = w.Body.Close()
	w = hubDo(t, http.MethodPut, ts.URL+"/api/canvases/dst/files/index.html", hubToken, []byte("TARGET-OLD"))
	_ = w.Body.Close()

	over := hubDo(t, http.MethodPost, ts.URL+"/api/canvases/src/copy", hubToken, []byte(`{"to":"dst","overwrite":true}`))
	ob, _ := io.ReadAll(over.Body)
	_ = over.Body.Close()
	if over.StatusCode != http.StatusOK {
		t.Fatalf("overwrite copy status = %d, body: %s", over.StatusCode, ob)
	}

	// Target now holds the source content.
	got := hubDo(t, http.MethodGet, ts.URL+"/api/canvases/dst/files/index.html", hubToken, nil)
	gb, _ := io.ReadAll(got.Body)
	_ = got.Body.Close()
	if string(gb) != "SOURCE" {
		t.Errorf("dst after overwrite = %q, want SOURCE", gb)
	}

	// A precopy snapshot of the old target content exists.
	snaps, err := snapshot.List(s.cfg.VersionsDir(), "dst")
	if err != nil {
		t.Fatalf("list snapshots: %v", err)
	}
	found := false
	for _, sn := range snaps {
		if sn.Label == "precopy" {
			found = true
		}
	}
	if !found {
		t.Errorf("no precopy snapshot for dst; snaps = %+v", snaps)
	}
}

// TestHubCopyCanvasErrors covers the input-validation error paths.
func TestHubCopyCanvasErrors(t *testing.T) {
	_, ts := newHubTestServer(t, nil, "")
	r := hubDo(t, http.MethodPost, ts.URL+"/api/canvases", hubToken, []byte(`{"id":"src"}`))
	_ = r.Body.Close()

	cases := []struct {
		name string
		from string
		body string
		want int
	}{
		{"missing source", "nope", `{"to":"dst"}`, http.StatusNotFound},
		{"invalid target id", "src", `{"to":"../escape"}`, http.StatusBadRequest},
		{"same source and target", "src", `{"to":"src"}`, http.StatusBadRequest},
		{"empty target", "src", `{}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := hubDo(t, http.MethodPost, ts.URL+"/api/canvases/"+tc.from+"/copy", hubToken, []byte(tc.body))
			_ = resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}
