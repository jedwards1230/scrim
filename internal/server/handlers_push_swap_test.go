package server

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests cover handlePush's swap/rollback failure branches -- the paths
// that fire when a push's atomic swap-into-place can't complete. The core
// invariant every one of them asserts is that a failed push never leaves the
// hub in a worse state than it found it: the previously-served canvas is
// still intact (never a 404-inducing stranded/empty canvas), and no staging
// or aside temp directory is leaked under push-staging.

// pushTar POSTs a one-file tar (index.html = content) to the hub's push
// endpoint for id, with a valid bearer token, and returns the response and
// its body. query, if non-empty, is appended as the raw query string (e.g.
// "title=T") so a caller can drive the metadata-write branch.
func pushTar(t *testing.T, ts *httptest.Server, id, content, query string) (int, string) {
	t.Helper()
	data := buildTar(t, []tarEntry{regFile("index.html", content)})
	target := ts.URL + "/api/push/" + id
	if query != "" {
		target += "?" + query
	}
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-push-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("push POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading push response: %v", err)
	}
	return resp.StatusCode, string(body)
}

// canvasFileContent reads index.html for a canvas straight off disk. The
// second return is false when the file (or the canvas dir) doesn't exist.
func canvasFileContent(t *testing.T, s *Server, id string) (string, bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(s.canvasesDir, id, "index.html"))
	if errors.Is(err, os.ErrNotExist) {
		return "", false
	}
	if err != nil {
		t.Fatalf("reading canvas file: %v", err)
	}
	return string(b), true
}

// assertNoStagingLeak asserts that push-staging holds no leftover staging or
// aside temp directories -- every failure path must clean up after itself.
func assertNoStagingLeak(t *testing.T, s *Server) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(s.cfg.Dir, "push-staging"))
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatalf("reading staging root: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("push-staging leaked %d entr(y/ies): %v, want none", len(entries), names)
	}
}

// TestHandlePushAsideRenameFailsLeavesCanvasIntact drives the "moving previous
// canvas aside" failure branch: an existing canvas can't be moved out of the
// way (the canvases dir is read-only, so removing its directory entry fails),
// so the push aborts before touching the served canvas at all. The previous
// content must remain fully intact.
func TestHandlePushAsideRenameFailsLeavesCanvasIntact(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses directory write permissions, so a read-only canvases dir can't fail the rename")
	}
	s, ts := newHubTestServer(t, []string{"127.0.0.0/8", "::1/128"}, "")

	// Seed an existing canvas so the swap has something to move aside.
	if code, body := pushTar(t, ts, "report", "v1", ""); code != http.StatusOK {
		t.Fatalf("seed push status = %d, want 200, body: %s", code, body)
	}

	// Make the canvases dir read-only: os.Rename(canvasDir, aside) needs write
	// on the parent to remove the directory entry, so the aside move fails.
	if err := os.Chmod(s.canvasesDir, 0o555); err != nil {
		t.Fatalf("chmod canvases dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(s.canvasesDir, 0o755) })

	code, body := pushTar(t, ts, "report", "v2", "")
	if code != http.StatusInternalServerError {
		t.Fatalf("push over read-only canvases dir status = %d, want 500, body: %s", code, body)
	}
	if !strings.Contains(body, "moving previous canvas aside") {
		t.Errorf("push error body = %q, want it to mention moving the previous canvas aside", body)
	}

	got, ok := canvasFileContent(t, s, "report")
	if !ok {
		t.Fatal("previous canvas is gone after a failed aside-rename, want it left intact")
	}
	if got != "v1" {
		t.Errorf("canvas content after failed push = %q, want the original %q", got, "v1")
	}
	assertNoStagingLeak(t, s)
}

// TestHandlePushStagedSwapFailsRollsBack drives the "swapping staged canvas
// into place" failure branch and, critically, its rollback: the previous
// canvas has already been moved aside when the staged-swap rename fails, so
// the handler must roll the aside copy back into place. Injected via the
// renameStagedSwap seam because this ordering can't be provoked through the
// filesystem alone (see the seam's doc comment in handlers_push.go).
func TestHandlePushStagedSwapFailsRollsBack(t *testing.T) {
	s, ts := newHubTestServer(t, []string{"127.0.0.0/8", "::1/128"}, "")

	if code, body := pushTar(t, ts, "report", "v1", ""); code != http.StatusOK {
		t.Fatalf("seed push status = %d, want 200, body: %s", code, body)
	}

	injected := errors.New("injected staged-swap rename failure")
	orig := renameStagedSwap
	renameStagedSwap = func(_, _ string) error { return injected }
	t.Cleanup(func() { renameStagedSwap = orig })

	code, body := pushTar(t, ts, "report", "v2", "")
	if code != http.StatusInternalServerError {
		t.Fatalf("push with a failing staged-swap rename status = %d, want 500, body: %s", code, body)
	}
	if !strings.Contains(body, "swapping staged canvas into place") {
		t.Errorf("push error body = %q, want it to mention swapping the staged canvas into place", body)
	}

	// The rollback must have restored the original canvas verbatim.
	got, ok := canvasFileContent(t, s, "report")
	if !ok {
		t.Fatal("canvas is gone after a rolled-back push, want the original restored")
	}
	if got != "v1" {
		t.Errorf("canvas content after rollback = %q, want the original %q", got, "v1")
	}
	assertNoStagingLeak(t, s)
}

// TestHandlePushMetadataWriteFailsAfterSwap drives the "writing canvas
// metadata" failure branch: the content swap has already succeeded (so the
// new canvas is served) but recording its metadata fails. The metadata dir is
// pre-created as a regular file, so canvas.Create's MkdirAll on it fails
// regardless of the running uid -- no permission trickery, no root skip.
func TestHandlePushMetadataWriteFailsAfterSwap(t *testing.T) {
	s, ts := newHubTestServer(t, []string{"127.0.0.0/8", "::1/128"}, "")

	// Occupy the metadata dir path with a plain file so writeMeta's MkdirAll
	// on it fails with a not-a-directory error.
	if err := os.RemoveAll(s.metaDir); err != nil {
		t.Fatalf("clearing metadata dir path: %v", err)
	}
	if err := os.WriteFile(s.metaDir, []byte("not a dir"), 0o644); err != nil {
		t.Fatalf("occupying metadata dir path with a file: %v", err)
	}

	// A title forces handlePush into the metadata-writing branch.
	code, body := pushTar(t, ts, "report", "v1", "title=Report")
	if code != http.StatusInternalServerError {
		t.Fatalf("push with an unwritable metadata dir status = %d, want 500, body: %s", code, body)
	}
	if !strings.Contains(body, "writing canvas metadata") {
		t.Errorf("push error body = %q, want it to mention writing canvas metadata", body)
	}

	// The content swap happened before the metadata write, so the canvas is
	// present on disk even though the push reported failure.
	got, ok := canvasFileContent(t, s, "report")
	if !ok {
		t.Fatal("canvas content is missing after a metadata-write failure, want the swapped-in content present")
	}
	if got != "v1" {
		t.Errorf("canvas content = %q, want the pushed %q", got, "v1")
	}
	assertNoStagingLeak(t, s)
}
