package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jedwards1230/scrim/internal/config"
	"github.com/jedwards1230/scrim/internal/snapshot"
)

// TestHubSnapshotCreateListRevert exercises the full snapshot machine-API
// lifecycle over the wire: create a canvas + file, snapshot it, mutate the
// file, revert to the snapshot, and confirm the content is restored AND a
// "prerevert" safety snapshot was taken.
func TestHubSnapshotCreateListRevert(t *testing.T) {
	s, ts := newHubTestServer(t, nil, "")

	// Create canvas + original file.
	resp := hubDo(t, http.MethodPost, ts.URL+"/api/canvases", hubToken, []byte(`{"id":"c1"}`))
	_ = resp.Body.Close()
	resp = hubDo(t, http.MethodPut, ts.URL+"/api/canvases/c1/files/index.html", hubToken, []byte("original"))
	_ = resp.Body.Close()

	// Snapshot the original.
	resp = hubDo(t, http.MethodPost, ts.URL+"/api/canvases/c1/snapshots", hubToken, []byte(`{"label":"first"}`))
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create snapshot status = %d, want 201, body: %s", resp.StatusCode, body)
	}
	snapName := extractJSONField(t, body, "name")
	if snapName == "" {
		t.Fatal("snapshot name empty")
	}

	// List: newest-first, must include our snapshot.
	resp = hubDo(t, http.MethodGet, ts.URL+"/api/canvases/c1/snapshots", hubToken, nil)
	listBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var listed []snapshotResponse
	if err := json.Unmarshal(listBody, &listed); err != nil {
		t.Fatalf("decode snapshot list: %v (body: %s)", err, listBody)
	}
	if len(listed) != 1 || listed[0].Name != snapName || listed[0].Label != "first" {
		t.Fatalf("snapshot list = %+v, want one entry named %q labeled first", listed, snapName)
	}

	// Mutate the live file.
	resp = hubDo(t, http.MethodPut, ts.URL+"/api/canvases/c1/files/index.html", hubToken, []byte("changed"))
	_ = resp.Body.Close()

	// Revert to the snapshot.
	resp = hubDo(t, http.MethodPost, ts.URL+"/api/canvases/c1/snapshots/"+snapName+"/revert", hubToken, nil)
	revertBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revert status = %d, want 200, body: %s", resp.StatusCode, revertBody)
	}
	if got := extractJSONField(t, revertBody, "reverted"); got != "c1" {
		t.Errorf("reverted = %q, want c1", got)
	}

	// The live file is back to the original.
	resp = hubDo(t, http.MethodGet, ts.URL+"/api/canvases/c1/files/index.html", hubToken, nil)
	fileBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(fileBody) != "original" {
		t.Errorf("reverted file = %q, want original", fileBody)
	}

	// A prerevert safety snapshot now exists alongside the first one.
	entries, err := snapshot.List(s.cfg.VersionsDir(), "c1")
	if err != nil {
		t.Fatalf("snapshot.List: %v", err)
	}
	var sawPrerevert bool
	for _, e := range entries {
		if e.Label == "prerevert" {
			sawPrerevert = true
		}
	}
	if !sawPrerevert {
		t.Error("expected a prerevert safety snapshot after revert")
	}
}

// TestHubSnapshotListEmpty asserts a canvas with no snapshots lists as an
// empty array, not an error.
func TestHubSnapshotListEmpty(t *testing.T) {
	_, ts := newHubTestServer(t, nil, "")
	resp := hubDo(t, http.MethodPost, ts.URL+"/api/canvases", hubToken, []byte(`{"id":"c1"}`))
	_ = resp.Body.Close()

	resp = hubDo(t, http.MethodGet, ts.URL+"/api/canvases/c1/snapshots", hubToken, nil)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200", resp.StatusCode)
	}
	var listed []snapshotResponse
	if err := json.Unmarshal(body, &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed) != 0 {
		t.Errorf("want empty snapshot list, got %d", len(listed))
	}
}

// TestLocalDaemonHasNoMachineAPIRoutes reinforces the hub_test.go zero-impact
// invariant for the new machine-API routes specifically: a default (non-hub)
// server registers none of them, so each returns 404.
func TestLocalDaemonHasNoMachineAPIRoutes(t *testing.T) {
	cfg := config.Config{Dir: t.TempDir(), Host: "127.0.0.1", Port: 0, IdleTimeout: time.Hour, NoAuth: true}
	s := New(cfg)
	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)

	for _, ep := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/canvases/c1/files/index.html"},
		{http.MethodPut, "/api/canvases/c1/files/index.html"},
		{http.MethodPatch, "/api/canvases/c1/files/index.html"},
		{http.MethodGet, "/api/canvases/c1/snapshots"},
		{http.MethodPost, "/api/canvases/c1/snapshots"},
		{http.MethodPost, "/api/canvases/c1/snapshots/somesnap/revert"},
	} {
		req, _ := http.NewRequest(ep.method, ts.URL+ep.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", ep.method, ep.path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s on local daemon status = %d, want 404 (route must not exist)", ep.method, ep.path, resp.StatusCode)
		}
	}
}

// TestHubSnapshotClientErrorStatuses pins the client-vs-server error split on
// the snapshot machine API: invalid label -> 400, missing canvas or missing
// snapshot -> 404 (never a blanket 500), and a failed revert to a typo'd name
// leaves NO prerevert snapshot behind.
func TestHubSnapshotClientErrorStatuses(t *testing.T) {
	s, ts := newHubTestServer(t, nil, "")

	// Seed a canvas with one file and one snapshot.
	resp := hubDo(t, http.MethodPost, ts.URL+"/api/canvases", hubToken, []byte(`{"id":"c1"}`))
	_ = resp.Body.Close()
	resp = hubDo(t, http.MethodPut, ts.URL+"/api/canvases/c1/files/index.html", hubToken, []byte("v1"))
	_ = resp.Body.Close()
	resp = hubDo(t, http.MethodPost, ts.URL+"/api/canvases/c1/snapshots", hubToken, []byte(`{"label":"seed"}`))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("seed snapshot status = %d, want 201", resp.StatusCode)
	}

	t.Run("invalid label is 400", func(t *testing.T) {
		resp := hubDo(t, http.MethodPost, ts.URL+"/api/canvases/c1/snapshots", hubToken, []byte(`{"label":"no spaces allowed"}`))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("invalid label status = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("snapshot of missing canvas is 404", func(t *testing.T) {
		resp := hubDo(t, http.MethodPost, ts.URL+"/api/canvases/ghost/snapshots", hubToken, []byte(`{}`))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("missing canvas status = %d, want 404", resp.StatusCode)
		}
	})

	t.Run("revert to unknown snapshot is 404 and takes no prerevert", func(t *testing.T) {
		resp := hubDo(t, http.MethodPost, ts.URL+"/api/canvases/c1/snapshots/20200101-000000.000000000-nope/revert", hubToken, nil)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("unknown snapshot revert status = %d, want 404, body: %s", resp.StatusCode, body)
		}

		// The failed revert must not have taken a prerevert safety snapshot --
		// that would poison a later bare revert-to-latest.
		entries, err := snapshot.List(s.cfg.VersionsDir(), "c1")
		if err != nil {
			t.Fatalf("snapshot.List: %v", err)
		}
		if len(entries) != 1 {
			t.Fatalf("snapshots after failed revert = %d, want 1 (seed only)", len(entries))
		}
		if entries[0].Label == "prerevert" {
			t.Error("failed revert left a prerevert snapshot behind")
		}
	})
}
