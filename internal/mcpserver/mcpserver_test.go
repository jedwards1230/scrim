package mcpserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jedwards1230/scrim/internal/apiclient"
	"github.com/jedwards1230/scrim/internal/config"
	"github.com/jedwards1230/scrim/internal/gzipx"
	"github.com/jedwards1230/scrim/internal/snapshot"
	"github.com/jedwards1230/scrim/internal/state"
)

// testServer returns a *server backed by a fresh t.TempDir() so no test ever
// touches the real ~/.scrim. Host/Port are placeholders — filesystem handlers
// never use them, and daemon-backed tests override resolveDaemon entirely.
func testServer(t *testing.T) *server {
	t.Helper()
	cfg := config.Config{Dir: t.TempDir(), Host: "127.0.0.1", Port: 7799}
	return &server{backend: newLocalBackend(cfg), cfg: cfg, ver: "test", local: true}
}

// writeCanvasFile creates a canvas directory for id under cfg and drops a file
// into it, returning the canvas directory.
func writeCanvasFile(t *testing.T, cfg config.Config, id, name, content string) string {
	t.Helper()
	dir := filepath.Join(cfg.CanvasesDir(), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir canvas: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write canvas file: %v", err)
	}
	return dir
}

// withResolveDaemon overrides the package-level resolveDaemon seam for the
// duration of a test, restoring it on cleanup.
func withResolveDaemon(t *testing.T, fn func(config.Config, bool) (*apiclient.Client, *state.State, bool, error)) {
	t.Helper()
	orig := resolveDaemon
	resolveDaemon = fn
	t.Cleanup(func() { resolveDaemon = orig })
}

func mustText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) == 0 {
		t.Fatal("result has no content")
	}
	txt, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content[0] type = %T, want *mcp.TextContent", res.Content[0])
	}
	return txt.Text
}

// ── filesystem handlers ─────────────────────────────────────────────────────

func TestHandlePath(t *testing.T) {
	s := testServer(t)
	res, out, err := s.handlePath(context.Background(), nil, pathInput{ID: "report"})
	if err != nil {
		t.Fatalf("handlePath: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %q", mustText(t, res))
	}
	want := filepath.Join(s.cfg.CanvasesDir(), "report")
	if out.Path != want {
		t.Errorf("path = %q, want %q", out.Path, want)
	}
}

func TestHandlePathInvalidID(t *testing.T) {
	s := testServer(t)
	res, _, err := s.handlePath(context.Background(), nil, pathInput{ID: "../etc"})
	if err != nil {
		t.Fatalf("handlePath returned Go error, want tool error result: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected IsError for an invalid id")
	}
}

func TestHandleSnapAndSnaps(t *testing.T) {
	s := testServer(t)
	writeCanvasFile(t, s.cfg, "c1", "index.html", "<h1>v1</h1>")

	res, out, err := s.handleSnap(context.Background(), nil, snapInput{ID: "c1", Label: "first"})
	if err != nil {
		t.Fatalf("handleSnap: %v", err)
	}
	if res.IsError {
		t.Fatalf("snap error: %q", mustText(t, res))
	}
	if out.Name == "" || out.Dir == "" {
		t.Fatalf("snap output empty: %+v", out)
	}
	if fi, statErr := os.Stat(out.Dir); statErr != nil || !fi.IsDir() {
		t.Errorf("snapshot dir %q not created", out.Dir)
	}

	// A second snapshot so newest-first ordering is observable.
	if _, _, err := s.handleSnap(context.Background(), nil, snapInput{ID: "c1", Label: "second"}); err != nil {
		t.Fatalf("second handleSnap: %v", err)
	}

	lres, lout, err := s.handleSnaps(context.Background(), nil, snapsInput{ID: "c1"})
	if err != nil {
		t.Fatalf("handleSnaps: %v", err)
	}
	if lres.IsError {
		t.Fatalf("snaps error: %q", mustText(t, lres))
	}
	if len(lout.Snapshots) != 2 {
		t.Fatalf("got %d snapshots, want 2", len(lout.Snapshots))
	}
	// snaps returns newest-first: [0] must be at or after [1].
	if lout.Snapshots[0].Timestamp.Before(lout.Snapshots[1].Timestamp) {
		t.Errorf("snapshots not newest-first: [0]=%v [1]=%v", lout.Snapshots[0].Timestamp, lout.Snapshots[1].Timestamp)
	}
	if lout.Snapshots[0].Label != "second" {
		t.Errorf("newest label = %q, want %q", lout.Snapshots[0].Label, "second")
	}
}

func TestHandleSnapsEmpty(t *testing.T) {
	s := testServer(t)
	_, out, err := s.handleSnaps(context.Background(), nil, snapsInput{ID: "nope"})
	if err != nil {
		t.Fatalf("handleSnaps: %v", err)
	}
	if len(out.Snapshots) != 0 {
		t.Errorf("want no snapshots, got %d", len(out.Snapshots))
	}
}

func TestHandleRevertRestoresAndTakesSafetySnapshot(t *testing.T) {
	s := testServer(t)
	dir := writeCanvasFile(t, s.cfg, "c1", "index.html", "<h1>original</h1>")

	// Snapshot the original, then mutate the live canvas.
	if _, _, err := s.handleSnap(context.Background(), nil, snapInput{ID: "c1"}); err != nil {
		t.Fatalf("handleSnap: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>changed</h1>"), 0o644); err != nil {
		t.Fatalf("mutate canvas: %v", err)
	}

	res, out, err := s.handleRevert(context.Background(), nil, revertInput{ID: "c1"})
	if err != nil {
		t.Fatalf("handleRevert: %v", err)
	}
	if res.IsError {
		t.Fatalf("revert error: %q", mustText(t, res))
	}
	if out.Reverted != "c1" || out.Snapshot == "" {
		t.Fatalf("revert output = %+v", out)
	}

	got, err := os.ReadFile(filepath.Join(dir, "index.html"))
	if err != nil {
		t.Fatalf("read reverted file: %v", err)
	}
	if string(got) != "<h1>original</h1>" {
		t.Errorf("reverted content = %q, want original", string(got))
	}

	// A prerevert safety snapshot must now exist alongside the original.
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

func TestHandleRevertNoSnapshots(t *testing.T) {
	s := testServer(t)
	writeCanvasFile(t, s.cfg, "c1", "index.html", "x")
	res, _, err := s.handleRevert(context.Background(), nil, revertInput{ID: "c1"})
	if err != nil {
		t.Fatalf("handleRevert: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result reverting with no snapshots")
	}
}

func TestHandleRmFallbackDeletesCanvas(t *testing.T) {
	s := testServer(t)
	dir := writeCanvasFile(t, s.cfg, "c1", "index.html", "x")

	// No daemon running: resolveDaemon(selfStart=false) reports running=false,
	// so rm falls back to canvas.Delete.
	withResolveDaemon(t, func(config.Config, bool) (*apiclient.Client, *state.State, bool, error) {
		return nil, nil, false, nil
	})

	res, out, err := s.handleRm(context.Background(), nil, rmInput{ID: "c1"})
	if err != nil {
		t.Fatalf("handleRm: %v", err)
	}
	if res.IsError {
		t.Fatalf("rm error: %q", mustText(t, res))
	}
	if out.Removed != "c1" {
		t.Errorf("removed = %q, want c1", out.Removed)
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Errorf("canvas dir still exists after rm fallback")
	}
}

func TestHandlePush(t *testing.T) {
	s := testServer(t)
	writeCanvasFile(t, s.cfg, "c1", "index.html", "<h1>push me</h1>")

	var gotPath, gotAuth, gotCT string
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "c1", "url": "/c/c1/"})
	}))
	defer hub.Close()

	res, out, err := s.handlePush(context.Background(), nil, pushInput{ID: "c1", To: hub.URL, Token: "secret"})
	if err != nil {
		t.Fatalf("handlePush: %v", err)
	}
	if res.IsError {
		t.Fatalf("push error: %q", mustText(t, res))
	}
	if want := hub.URL + "/c/c1/"; out.URL != want {
		t.Errorf("push url = %q, want %q", out.URL, want)
	}
	if gotPath != "/api/push/c1" {
		t.Errorf("hub path = %q, want /api/push/c1", gotPath)
	}
	if gotAuth != "Bearer secret" {
		t.Errorf("auth header = %q, want Bearer secret", gotAuth)
	}
	if gotCT != "application/x-tar" {
		t.Errorf("content-type = %q, want application/x-tar", gotCT)
	}
}

func TestHandlePushMissingCanvas(t *testing.T) {
	s := testServer(t)
	res, _, err := s.handlePush(context.Background(), nil, pushInput{ID: "ghost", To: "http://127.0.0.1:1", Token: "t"})
	if err != nil {
		t.Fatalf("handlePush: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected error result for a missing canvas")
	}
}

func TestHandlePushRequiredArgs(t *testing.T) {
	s := testServer(t)
	writeCanvasFile(t, s.cfg, "c1", "index.html", "x")
	for _, tc := range []struct {
		name string
		in   pushInput
	}{
		{"missing to", pushInput{ID: "c1", Token: "t"}},
		{"missing token", pushInput{ID: "c1", To: "http://127.0.0.1:1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, _, err := s.handlePush(context.Background(), nil, tc.in)
			if err != nil {
				t.Fatalf("handlePush: %v", err)
			}
			if !res.IsError {
				t.Fatal("expected an error result")
			}
		})
	}
}

func TestHandleRmViaDaemon(t *testing.T) {
	s := testServer(t)
	st := &state.State{Host: "127.0.0.1", Port: 7799, Token: "tok"}
	var deleted string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleted = r.URL.Path
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(ts.Close)
	client := apiclient.NewWithToken(ts.URL, st.Token)
	// running=true so rm deletes via the daemon rather than the fs fallback.
	withResolveDaemon(t, func(config.Config, bool) (*apiclient.Client, *state.State, bool, error) {
		return client, st, true, nil
	})

	res, out, err := s.handleRm(context.Background(), nil, rmInput{ID: "c1"})
	if err != nil {
		t.Fatalf("handleRm: %v", err)
	}
	if res.IsError {
		t.Fatalf("rm error: %q", mustText(t, res))
	}
	if out.Removed != "c1" {
		t.Errorf("removed = %q, want c1", out.Removed)
	}
	if deleted != "/api/canvases/c1" {
		t.Errorf("daemon delete path = %q, want /api/canvases/c1", deleted)
	}
}

func TestHandleAddErrors(t *testing.T) {
	s := testServer(t)

	t.Run("invalid id", func(t *testing.T) {
		res, _, err := s.handleAdd(context.Background(), nil, addInput{ID: "../x"})
		if err != nil {
			t.Fatalf("handleAdd: %v", err)
		}
		if !res.IsError {
			t.Fatal("expected error result for invalid id")
		}
	})

	t.Run("daemon resolve failure", func(t *testing.T) {
		withResolveDaemon(t, func(config.Config, bool) (*apiclient.Client, *state.State, bool, error) {
			return nil, nil, false, errWiring
		})
		res, _, err := s.handleAdd(context.Background(), nil, addInput{ID: "ok"})
		if err != nil {
			t.Fatalf("handleAdd: %v", err)
		}
		if !res.IsError {
			t.Fatal("expected error result when the daemon can't be resolved")
		}
	})

	t.Run("create failure", func(t *testing.T) {
		st := &state.State{Host: "127.0.0.1", Port: 7799, Token: "tok"}
		mockDaemon(t, st, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		})
		res, _, err := s.handleAdd(context.Background(), nil, addInput{ID: "ok"})
		if err != nil {
			t.Fatalf("handleAdd: %v", err)
		}
		if !res.IsError {
			t.Fatal("expected error result when CreateCanvas fails")
		}
	})
}

func TestHandleSnapInvalidID(t *testing.T) {
	s := testServer(t)
	res, _, err := s.handleSnap(context.Background(), nil, snapInput{ID: "../x"})
	if err != nil {
		t.Fatalf("handleSnap: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected error result for invalid id")
	}
}

// TestServeStdioReturnsOnCancel confirms the stdio Serve entrypoint wires up
// and returns once its context is cancelled, without touching stdout.
func TestServeStdioReturnsOnCancel(t *testing.T) {
	cfg := config.Config{Dir: t.TempDir(), Host: "127.0.0.1", Port: 7799}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: Run should observe it and return promptly.

	done := make(chan error, 1)
	go func() { done <- Serve(ctx, cfg, "test", nil, io.Discard) }()
	select {
	case <-done:
		// Returned (nil or a context error) — either is a clean stop.
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after ctx cancel")
	}
}

var errWiring = errTest("wiring failure")

type errTest string

func (e errTest) Error() string { return string(e) }

// ── daemon-backed handlers (resolveDaemon seam) ────────────────────────────--

// mockDaemon returns an httptest server answering the daemon control API used
// by the daemon-backed handlers, plus a resolveDaemon replacement pointing at
// it with the given synthetic state.
func mockDaemon(t *testing.T, st *state.State, handler http.HandlerFunc) {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	client := apiclient.NewWithToken(ts.URL, st.Token)
	withResolveDaemon(t, func(_ config.Config, selfStart bool) (*apiclient.Client, *state.State, bool, error) {
		return client, st, true, nil
	})
}

func TestHandleAdd(t *testing.T) {
	s := testServer(t)
	st := &state.State{Host: "127.0.0.1", Port: 7799, Token: "tok"}
	mockDaemon(t, st, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/canvases" {
			http.NotFound(w, r)
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		resp := apiclient.CanvasResponse{
			ID:  body["id"],
			Dir: "/data/canvases/" + body["id"],
			URL: "http://127.0.0.1:7799/c/" + body["id"] + "/?t=tok",
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	res, out, err := s.handleAdd(context.Background(), nil, addInput{ID: "hello", Title: "Hi"})
	if err != nil {
		t.Fatalf("handleAdd: %v", err)
	}
	if res.IsError {
		t.Fatalf("add error: %q", mustText(t, res))
	}
	if out.ID != "hello" || out.Dir == "" || out.URL == "" {
		t.Errorf("add output = %+v", out)
	}
}

func TestHandleList(t *testing.T) {
	s := testServer(t)
	st := &state.State{Host: "127.0.0.1", Port: 7799, Token: "tok"}
	mockDaemon(t, st, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/canvases" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode([]apiclient.CanvasResponse{
			{ID: "a", Title: "Alpha", URL: "http://127.0.0.1:7799/c/a/?t=tok", Dir: "/d/a", Icon: "A", Color: "#111"},
			{ID: "b", URL: "http://127.0.0.1:7799/c/b/?t=tok", Dir: "/d/b", Icon: "B", Color: "#222"},
		})
	})

	_, out, err := s.handleList(context.Background(), nil, listInput{})
	if err != nil {
		t.Fatalf("handleList: %v", err)
	}
	if len(out.Canvases) != 2 {
		t.Fatalf("got %d canvases, want 2", len(out.Canvases))
	}
	if out.Canvases[0].ID != "a" || out.Canvases[0].Title != "Alpha" {
		t.Errorf("canvas[0] = %+v", out.Canvases[0])
	}
}

func TestHandleLinkCanvas(t *testing.T) {
	s := testServer(t)
	st := &state.State{Host: "127.0.0.1", Port: 7799, Token: "tok"}
	const canvasURL = "http://127.0.0.1:7799/c/a/?t=tok"
	mockDaemon(t, st, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]apiclient.CanvasResponse{{ID: "a", URL: canvasURL}})
	})

	_, out, err := s.handleLink(context.Background(), nil, linkInput{ID: "a"})
	if err != nil {
		t.Fatalf("handleLink: %v", err)
	}
	if len(out.URLs) != 1 || out.URLs[0] != canvasURL {
		t.Errorf("link urls = %v, want [%s]", out.URLs, canvasURL)
	}
}

func TestHandleLinkCanvasNotFound(t *testing.T) {
	s := testServer(t)
	st := &state.State{Host: "127.0.0.1", Port: 7799, Token: "tok"}
	mockDaemon(t, st, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]apiclient.CanvasResponse{})
	})

	res, _, err := s.handleLink(context.Background(), nil, linkInput{ID: "ghost"})
	if err != nil {
		t.Fatalf("handleLink: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result for a missing canvas")
	}
}

func TestHandleLinkDashboard(t *testing.T) {
	s := testServer(t)
	cases := []struct {
		name   string
		st     *state.State
		wanted string
	}{
		{"auth enabled", &state.State{Host: "127.0.0.1", Port: 7799, Token: "tok"}, "http://127.0.0.1:7799/?t=tok"},
		{"no auth", &state.State{Host: "127.0.0.1", Port: 7799, NoAuth: true}, "http://127.0.0.1:7799/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withResolveDaemon(t, func(config.Config, bool) (*apiclient.Client, *state.State, bool, error) {
				return apiclient.NewWithToken("http://unused", tc.st.Token), tc.st, true, nil
			})
			_, out, err := s.handleLink(context.Background(), nil, linkInput{})
			if err != nil {
				t.Fatalf("handleLink: %v", err)
			}
			if len(out.URLs) != 1 || out.URLs[0] != tc.wanted {
				t.Errorf("dashboard urls = %v, want [%s]", out.URLs, tc.wanted)
			}
		})
	}
}

func TestHandleStatusRunning(t *testing.T) {
	s := testServer(t)
	st := &state.State{Host: "127.0.0.1", Port: 7799, Token: "tok"}
	mockDaemon(t, st, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/status" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(apiclient.StatusResponse{
			PID: 4242, Host: "127.0.0.1", Port: 7799, Version: "test", CanvasCount: 3,
		})
	})

	_, out, err := s.handleStatus(context.Background(), nil, statusInput{})
	if err != nil {
		t.Fatalf("handleStatus: %v", err)
	}
	if !out.Running || out.PID != 4242 || out.CanvasCount != 3 {
		t.Errorf("status output = %+v", out)
	}
}

func TestHandleStatusNotRunning(t *testing.T) {
	s := testServer(t)
	withResolveDaemon(t, func(config.Config, bool) (*apiclient.Client, *state.State, bool, error) {
		return nil, nil, false, nil
	})
	res, out, err := s.handleStatus(context.Background(), nil, statusInput{})
	if err != nil {
		t.Fatalf("handleStatus: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %q", mustText(t, res))
	}
	if out.Running {
		t.Error("running = true, want false when no daemon")
	}
}

// ── end-to-end wiring over the in-memory transport ─────────────────────────--

func connectInMemory(t *testing.T, srv *mcp.Server) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatalf("server Connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0"}, nil)
	session, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func TestNewServerRegistersAllTools(t *testing.T) {
	cfg := config.Config{Dir: t.TempDir(), Host: "127.0.0.1", Port: 7799}
	srv := NewServer(cfg, "test", nil) // local mode
	session := connectInMemory(t, srv)

	got := map[string]bool{}
	for tool, err := range session.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatalf("Tools iteration: %v", err)
		}
		if tool.InputSchema == nil {
			t.Errorf("tool %q has a nil InputSchema", tool.Name)
		}
		got[tool.Name] = true
	}
	// Local mode registers every tool, including path (local-only).
	for _, want := range []string{"add", "list", "link", "copy_canvas", "path", "rm", "snap", "snaps", "revert", "status", "list_files", "read_file", "write_file", "edit_file", "push"} {
		if !got[want] {
			t.Errorf("tool %q not registered", want)
		}
	}
	if len(got) != 15 {
		t.Errorf("registered %d tools, want 15: %v", len(got), got)
	}
}

// TestToolSurfacePerMode asserts the self-describing tool surface: `path` is
// present in local mode and ABSENT in hub mode, while
// read_file/write_file/edit_file are present in both.
func TestToolSurfacePerMode(t *testing.T) {
	cfg := config.Config{Dir: t.TempDir(), Host: "127.0.0.1", Port: 7799}
	cases := []struct {
		name     string
		hub      *HubTarget
		wantPath bool
	}{
		{"local", nil, true},
		{"hub", &HubTarget{BaseURL: "http://127.0.0.1:7788", Token: "tok"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := NewServer(cfg, "test", tc.hub)
			session := connectInMemory(t, srv)
			got := map[string]bool{}
			for tool, err := range session.Tools(context.Background(), nil) {
				if err != nil {
					t.Fatalf("Tools iteration: %v", err)
				}
				got[tool.Name] = true
			}
			if got["path"] != tc.wantPath {
				t.Errorf("path present = %v, want %v (%s mode)", got["path"], tc.wantPath, tc.name)
			}
			for _, want := range []string{"read_file", "write_file", "edit_file"} {
				if !got[want] {
					t.Errorf("tool %q missing in %s mode", want, tc.name)
				}
			}
		})
	}
}

func TestCallToolPathEndToEnd(t *testing.T) {
	cfg := config.Config{Dir: t.TempDir(), Host: "127.0.0.1", Port: 7799}
	srv := NewServer(cfg, "test", nil)
	session := connectInMemory(t, srv)

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "path",
		Arguments: map[string]any{"id": "report"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("path tool error: %v", res.Content)
	}
	want := filepath.Join(cfg.CanvasesDir(), "report")
	txt := mustText(t, res)
	if txt != want {
		t.Errorf("path text = %q, want %q", txt, want)
	}
	// The typed Out is marshalled into StructuredContent by the SDK.
	sc, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("StructuredContent type = %T, want map", res.StructuredContent)
	}
	if sc["path"] != want {
		t.Errorf("structured path = %v, want %q", sc["path"], want)
	}
}

// TestCallToolEditFileEndToEnd drives edit_file over the in-memory transport
// in local mode: the replacement lands on disk and the structured result
// reports {path, replacements}.
func TestCallToolEditFileEndToEnd(t *testing.T) {
	cfg := config.Config{Dir: t.TempDir(), Host: "127.0.0.1", Port: 7799}
	srv := NewServer(cfg, "test", nil)
	session := connectInMemory(t, srv)

	dir := filepath.Join(cfg.CanvasesDir(), "c1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir canvas: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>hello</h1>"), 0o644); err != nil {
		t.Fatalf("seed canvas file: %v", err)
	}

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "edit_file",
		Arguments: map[string]any{
			"id": "c1", "path": "index.html",
			"old_string": "hello", "new_string": "goodbye",
		},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("edit_file tool error: %v", res.Content)
	}
	sc, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("StructuredContent type = %T, want map", res.StructuredContent)
	}
	if sc["path"] != "index.html" || sc["replacements"] != float64(1) {
		t.Errorf("structured content = %v, want path index.html, replacements 1", sc)
	}
	got, err := os.ReadFile(filepath.Join(dir, "index.html"))
	if err != nil {
		t.Fatalf("read edited file: %v", err)
	}
	if string(got) != "<h1>goodbye</h1>" {
		t.Errorf("edited content = %q, want <h1>goodbye</h1>", got)
	}
}

func TestCallToolInvalidIDIsToolError(t *testing.T) {
	cfg := config.Config{Dir: t.TempDir(), Host: "127.0.0.1", Port: 7799}
	srv := NewServer(cfg, "test", nil)
	session := connectInMemory(t, srv)

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "path",
		Arguments: map[string]any{"id": "../escape"},
	})
	if err != nil {
		t.Fatalf("CallTool protocol error (want tool-level error): %v", err)
	}
	if !res.IsError {
		t.Error("expected IsError for an invalid id")
	}
}

func TestCallToolListFilesEndToEnd(t *testing.T) {
	cfg := config.Config{Dir: t.TempDir(), Host: "127.0.0.1", Port: 7799}
	srv := NewServer(cfg, "test", nil)
	session := connectInMemory(t, srv)

	dir := filepath.Join(cfg.CanvasesDir(), "c1")
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>hi</h1>"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "assets", "app.js"), []byte("x=1"), 0o644)

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "list_files",
		Arguments: map[string]any{"id": "c1"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("list_files tool error: %v", res.Content)
	}
	sc, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("StructuredContent type = %T", res.StructuredContent)
	}
	files, ok := sc["files"].([]any)
	if !ok || len(files) != 2 {
		t.Fatalf("files = %v, want 2 entries", sc["files"])
	}
	first := files[0].(map[string]any)
	if first["path"] != "assets/app.js" {
		t.Errorf("first path = %v, want assets/app.js", first["path"])
	}
}

func TestCallToolEditFileBatchEndToEnd(t *testing.T) {
	cfg := config.Config{Dir: t.TempDir(), Host: "127.0.0.1", Port: 7799}
	srv := NewServer(cfg, "test", nil)
	session := connectInMemory(t, srv)
	dir := filepath.Join(cfg.CanvasesDir(), "c1")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "index.html"), []byte("alpha beta gamma"), 0o644)

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "edit_file",
		Arguments: map[string]any{
			"id": "c1", "path": "index.html",
			"edits": []any{
				map[string]any{"old_string": "alpha", "new_string": "one"},
				map[string]any{"old_string": "gamma", "new_string": "three"},
			},
		},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("batch edit_file tool error: %v", res.Content)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "index.html"))
	if string(got) != "one beta three" {
		t.Errorf("content = %q, want %q", got, "one beta three")
	}

	// Mutually exclusive: both edits and single fields -> tool error.
	bad, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "edit_file",
		Arguments: map[string]any{
			"id": "c1", "path": "index.html",
			"old_string": "beta", "new_string": "x",
			"edits": []any{map[string]any{"old_string": "beta", "new_string": "y"}},
		},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !bad.IsError {
		t.Error("both single + edits: want a tool error")
	}
}

func TestCallToolWriteReadGzipBase64RoundTrip(t *testing.T) {
	cfg := config.Config{Dir: t.TempDir(), Host: "127.0.0.1", Port: 7799}
	srv := NewServer(cfg, "test", nil)
	session := connectInMemory(t, srv)
	dir := filepath.Join(cfg.CanvasesDir(), "c1")
	_ = os.MkdirAll(dir, 0o755)

	raw := []byte(strings.Repeat("<div>x</div>", 1000))
	encoded := base64.StdEncoding.EncodeToString(gzipx.Deflate(raw))

	wres, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "write_file",
		Arguments: map[string]any{
			"id": "c1", "path": "index.html",
			"content": encoded, "encoding": "gzip+base64",
		},
	})
	if err != nil {
		t.Fatalf("write CallTool: %v", err)
	}
	if wres.IsError {
		t.Fatalf("write_file gzip+base64 error: %v", wres.Content)
	}
	// The DECODED bytes landed on disk.
	onDisk, _ := os.ReadFile(filepath.Join(dir, "index.html"))
	if !bytes.Equal(onDisk, raw) {
		t.Errorf("on-disk bytes mismatch (%d vs %d)", len(onDisk), len(raw))
	}

	// Read it back compressed and verify it inflates to the original.
	rres, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "read_file",
		Arguments: map[string]any{"id": "c1", "path": "index.html", "encoding": "gzip+base64"},
	})
	if err != nil {
		t.Fatalf("read CallTool: %v", err)
	}
	sc := rres.StructuredContent.(map[string]any)
	if sc["encoding"] != "gzip+base64" {
		t.Errorf("read encoding = %v, want gzip+base64", sc["encoding"])
	}
	compressed, err := base64.StdEncoding.DecodeString(sc["content"].(string))
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	back, err := gzipx.Inflate(compressed, maxFileBytes)
	if err != nil {
		t.Fatalf("inflate: %v", err)
	}
	if !bytes.Equal(back, raw) {
		t.Errorf("gzip+base64 read round-trip mismatch")
	}
}

func TestCallToolWriteFileBadEncoding(t *testing.T) {
	cfg := config.Config{Dir: t.TempDir(), Host: "127.0.0.1", Port: 7799}
	srv := NewServer(cfg, "test", nil)
	session := connectInMemory(t, srv)
	_ = os.MkdirAll(filepath.Join(cfg.CanvasesDir(), "c1"), 0o755)

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "write_file",
		Arguments: map[string]any{
			"id": "c1", "path": "index.html",
			"content": "not base64 gzip", "encoding": "gzip+base64",
		},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError {
		t.Error("invalid gzip+base64 content: want a tool error")
	}
}

func TestCallToolCopyCanvasEndToEnd(t *testing.T) {
	cfg := config.Config{Dir: t.TempDir(), Host: "127.0.0.1", Port: 7799}
	srv := NewServer(cfg, "test", nil)
	session := connectInMemory(t, srv)
	dir := filepath.Join(cfg.CanvasesDir(), "src")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>src</h1>"), 0o644)

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "copy_canvas",
		Arguments: map[string]any{"from": "src", "to": "dst"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("copy_canvas tool error: %v", res.Content)
	}
	got, _ := os.ReadFile(filepath.Join(cfg.CanvasesDir(), "dst", "index.html"))
	if string(got) != "<h1>src</h1>" {
		t.Errorf("dst content = %q", got)
	}

	// Copy onto the existing target without overwrite -> tool error.
	conflict, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "copy_canvas",
		Arguments: map[string]any{"from": "src", "to": "dst"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !conflict.IsError {
		t.Error("copy onto existing target: want a tool error")
	}
}
