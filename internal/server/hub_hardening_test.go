package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jedwards1230/scrim/internal/canvas"
	"github.com/jedwards1230/scrim/internal/config"
)

// hardeningCfg returns a config rooted at an isolated temp dir on a
// non-default port, matching the repo's test-isolation rule (never touch the
// real ~/.scrim or port 7777).
func hardeningCfg(t *testing.T) config.Config {
	t.Helper()
	return config.Config{
		Dir:         t.TempDir(),
		Host:        "127.0.0.1",
		Port:        0,
		IdleTimeout: time.Hour,
		NoAuth:      true,
	}
}

// TestNewHTTPServerTimeoutsByMode is the timeout half of the hard invariant:
// the local daemon gets ReadHeaderTimeout only, while a hub additionally gets
// ReadTimeout/IdleTimeout -- and WriteTimeout stays 0 (unlimited) in both so
// SSE responses are never truncated.
func TestNewHTTPServerTimeoutsByMode(t *testing.T) {
	t.Run("local daemon: ReadHeaderTimeout only", func(t *testing.T) {
		s := New(hardeningCfg(t))
		srv := s.newHTTPServer(nil)

		if got := srv.ReadHeaderTimeout; got != 10*time.Second {
			t.Errorf("ReadHeaderTimeout = %v, want 10s", got)
		}
		if got := srv.ReadTimeout; got != 0 {
			t.Errorf("ReadTimeout = %v, want 0 (local daemon must stay unchanged)", got)
		}
		if got := srv.IdleTimeout; got != 0 {
			t.Errorf("IdleTimeout = %v, want 0 (local daemon must stay unchanged)", got)
		}
		if got := srv.WriteTimeout; got != 0 {
			t.Errorf("WriteTimeout = %v, want 0 (unlimited: SSE has no upper bound)", got)
		}
	})

	t.Run("hub: adds ReadTimeout + IdleTimeout, WriteTimeout stays 0", func(t *testing.T) {
		s, err := NewHub(hardeningCfg(t), HubOptions{PushToken: "test-push-token"})
		if err != nil {
			t.Fatalf("NewHub: %v", err)
		}
		if !s.isHub() {
			t.Fatal("NewHub server reports isHub() = false, want true")
		}
		srv := s.newHTTPServer(nil)

		if got := srv.ReadHeaderTimeout; got != 10*time.Second {
			t.Errorf("ReadHeaderTimeout = %v, want 10s", got)
		}
		if got := srv.ReadTimeout; got != 60*time.Second {
			t.Errorf("ReadTimeout = %v, want 60s", got)
		}
		if got := srv.IdleTimeout; got != 120*time.Second {
			t.Errorf("IdleTimeout = %v, want 120s", got)
		}
		if got := srv.WriteTimeout; got != 0 {
			t.Errorf("WriteTimeout = %v, want 0 (unlimited: SSE has no upper bound)", got)
		}
	})
}

// TestHubRegisterGlobalCap exercises the global SSE ceiling directly on the
// hub type: register succeeds up to maxGlobal, the next open is rejected
// (ok=false), and freeing a slot via unregister lets the next open succeed
// again. clientCount tracks the live total throughout.
func TestHubRegisterGlobalCap(t *testing.T) {
	h := newHub()
	h.maxGlobal = 2 // maxPerCanvas left 0 (unlimited) to isolate the global cap

	// Two distinct canvases so the per-canvas cap can't be what rejects.
	_, un1, ok := h.register("a")
	if !ok {
		t.Fatal("register #1 ok = false, want true")
	}
	_, _, ok = h.register("b")
	if !ok {
		t.Fatal("register #2 ok = false, want true")
	}
	if got := h.clientCount(); got != 2 {
		t.Fatalf("clientCount = %d, want 2", got)
	}

	// Third exceeds the global cap.
	if ch, un, ok := h.register("c"); ok {
		t.Fatalf("register #3 ok = true, want false (global cap 2); ch=%v un=%v", ch, un != nil)
	}
	if got := h.clientCount(); got != 2 {
		t.Fatalf("clientCount after rejected register = %d, want 2 (rejection must not count)", got)
	}

	// Free a slot; the next open now fits.
	un1()
	if got := h.clientCount(); got != 1 {
		t.Fatalf("clientCount after unregister = %d, want 1", got)
	}
	if _, _, ok := h.register("c"); !ok {
		t.Fatal("register after freeing a slot ok = false, want true")
	}
	if got := h.clientCount(); got != 2 {
		t.Fatalf("clientCount = %d, want 2", got)
	}
}

// TestHubRegisterPerCanvasCap exercises the per-canvas ceiling: one canvas can
// only hold maxPerCanvas connections even when the global budget is free, and
// a different canvas is unaffected. Freeing a slot on the capped canvas admits
// the next open.
func TestHubRegisterPerCanvasCap(t *testing.T) {
	h := newHub()
	h.maxPerCanvas = 1 // maxGlobal left 0 (unlimited) to isolate the per-canvas cap

	_, un1, ok := h.register("a")
	if !ok {
		t.Fatal("register a#1 ok = false, want true")
	}
	// Second connection to the SAME canvas exceeds the per-canvas cap.
	if _, _, ok := h.register("a"); ok {
		t.Fatal("register a#2 ok = true, want false (per-canvas cap 1)")
	}
	// A different canvas is unaffected by a's per-canvas cap.
	if _, _, ok := h.register("b"); !ok {
		t.Fatal("register b#1 ok = false, want true (different canvas)")
	}
	if got := h.clientCount(); got != 2 {
		t.Fatalf("clientCount = %d, want 2", got)
	}

	// Free a's slot; a second a-connection now fits.
	un1()
	if got := h.canvasClientCount("a"); got != 0 {
		t.Fatalf("canvasClientCount(a) after unregister = %d, want 0", got)
	}
	if _, _, ok := h.register("a"); !ok {
		t.Fatal("register a after freeing its slot ok = false, want true")
	}
	if got := h.clientCount(); got != 2 {
		t.Fatalf("clientCount = %d, want 2", got)
	}
}

// TestHandleSSERejectsOverCap is the handler-level 503 path: with the hub at
// its global cap, GET /c/<id>/__events returns 503 instead of opening a
// stream. It stays deterministic by pre-filling the cap via direct register
// calls (no long-lived streams, no sleeps).
func TestHandleSSERejectsOverCap(t *testing.T) {
	s := New(hardeningCfg(t))
	// Create the canvas dir so handleSSE gets past its existence check and
	// reaches the register call (where the cap is enforced).
	id := "capcanvas"
	if err := os.MkdirAll(canvas.Dir(s.canvasesDir, id), 0o755); err != nil {
		t.Fatalf("creating canvas dir: %v", err)
	}

	// Saturate the global cap with one pre-registered client.
	s.hub.maxGlobal = 1
	_, _, ok := s.hub.register("other")
	if !ok {
		t.Fatal("priming register ok = false, want true")
	}

	req := httptest.NewRequest(http.MethodGet, "/c/"+id+"/__events", nil)
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	s.handleSSE(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("handleSSE over cap status = %d, want 503", rec.Code)
	}
}
