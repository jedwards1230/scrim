package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/jedwards1230/scrim/internal/canvas"
	"github.com/jedwards1230/scrim/internal/snapshot"
)

// snapshotResponse is one snapshot entry as returned by the hub machine API.
type snapshotResponse struct {
	Name      string    `json:"name"`
	Timestamp time.Time `json:"timestamp"`
	Label     string    `json:"label,omitempty"`
}

// handleListSnapshots serves GET /api/canvases/{id}/snapshots (hub mode only):
// every snapshot for the canvas, newest-first (matching `scrim snaps` and the
// snaps MCP tool). Bearer-gated via withHubGate. A canvas with no snapshots
// returns an empty list, not an error.
func (s *Server) handleListSnapshots(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := canvas.ValidateID(id); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	entries, err := snapshot.List(s.cfg.VersionsDir(), id)
	if err != nil {
		// Generic on purpose: raw snapshot/os errors can embed server paths.
		writeJSONError(w, http.StatusInternalServerError, "listing snapshots failed")
		return
	}
	// snapshot.List returns oldest-first; present newest-first.
	out := make([]snapshotResponse, 0, len(entries))
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		out = append(out, snapshotResponse{Name: e.Name, Timestamp: e.Timestamp, Label: e.Label})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleCreateSnapshot serves POST /api/canvases/{id}/snapshots (hub mode
// only): it snapshots the canvas's current contents, with an optional {label}
// in the JSON body. Bearer-gated via withHubGate.
func (s *Server) handleCreateSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := canvas.ValidateID(id); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	var body struct {
		Label string `json:"label"`
	}
	// An empty body is valid (no label); only a malformed non-empty body is an
	// error. json.Decode returns io.EOF for an empty body, which we treat as
	// "no label".
	if r.Body != nil {
		// The only expected body is a tiny {"label": ...}; cap it so a client
		// can't stream an arbitrarily large body into the decoder.
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeJSONError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
			return
		}
	}

	entry, err := snapshot.Create(canvas.Dir(s.canvasesDir, id), s.cfg.VersionsDir(), id, body.Label)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "creating snapshot failed (missing canvas or invalid label)")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"name": entry.Name, "dir": entry.Dir})
}

// handleRevertSnapshot serves POST /api/canvases/{id}/snapshots/{name}/revert
// (hub mode only): it reverts the canvas to the named snapshot, taking a
// "prerevert" safety snapshot of the current contents first (exactly like
// cli.cmdRevert and the revert MCP tool). Bearer-gated via withHubGate. The
// snapshot name is required (it's a path segment); snapshot.Revert validates
// it as a bare path component before touching the filesystem.
func (s *Server) handleRevertSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := canvas.ValidateID(id); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	name := r.PathValue("name")
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "snapshot name is required")
		return
	}

	canvasDir := canvas.Dir(s.canvasesDir, id)
	versionsDir := s.cfg.VersionsDir()

	// Safety snapshot of the live contents first, so the revert is itself
	// undoable -- but only if the canvas dir actually exists (a revert onto a
	// never-created canvas has nothing to preserve).
	if fi, statErr := os.Stat(canvasDir); statErr == nil && fi.IsDir() {
		if _, err := snapshot.Create(canvasDir, versionsDir, id, "prerevert"); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "prerevert snapshot failed")
			return
		}
	}

	entry, err := snapshot.Revert(canvasDir, versionsDir, id, name)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "revert failed (no such snapshot, or invalid name)")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"reverted": id, "snapshot": entry.Name})
}
