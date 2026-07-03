package server

import (
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/jedwards1230/scrim/internal/canvas"
)

const sseHeartbeatInterval = 15 * time.Second

// handleSSE serves /c/<id>/__events: a per-canvas Server-Sent Events stream
// that emits a "reload" event whenever the filesystem watcher detects a
// (debounced) change under that canvas's directory.
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := canvas.ValidateID(id); err != nil {
		http.NotFound(w, r)
		return
	}
	root := canvas.Dir(s.canvasesDir, id)
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		http.NotFound(w, r)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	// Register with the hub *before* writing the response headers: once the
	// client observes a 200, it (and anything asserting on hub.clientCount,
	// e.g. the shutdown tests) must be able to assume the connection is
	// already tracked. Registering after the header write/flush leaves a
	// window where the client sees "connected" before the server has
	// actually recorded it -- a real, if narrow, TOCTOU race between the
	// two goroutines.
	ch, unregister := s.hub.register(id)
	defer unregister()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	// An open SSE connection counts as activity for its whole lifetime (via
	// the reaper's separate SSE-client-count check); touching activity
	// again on disconnect restarts the idle clock from the moment the
	// connection actually closes rather than from when it was opened.
	defer s.activity.touch()

	heartbeat := time.NewTicker(sseHeartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.hub.done():
			// The server is shutting down: return promptly instead of
			// waiting for this client to disconnect on its own, so a
			// graceful http.Server.Shutdown isn't blocked on a browser tab
			// left open indefinitely (see initiateShutdown).
			return
		case <-ch:
			if _, err := fmt.Fprint(w, "event: reload\ndata: reload\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
