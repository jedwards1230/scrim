package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
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

	// Serialize with handlePush's swap sequence (and the other mutating
	// machine-API handlers) on the same canvas id: snapshotting walks the
	// canvas directory, and a concurrent push renaming it aside mid-walk
	// would yield a torn or empty snapshot.
	unlock := s.hubCfg.pushLocks.lock(id)
	defer unlock()

	entry, err := snapshot.Create(canvas.Dir(s.canvasesDir, id), s.cfg.VersionsDir(), id, body.Label)
	if err != nil {
		// Client errors get client statuses; bodies stay generic (raw
		// snapshot/os errors can embed server paths).
		switch {
		case errors.Is(err, snapshot.ErrInvalidLabel):
			writeJSONError(w, http.StatusBadRequest, "invalid snapshot label")
		case errors.Is(err, snapshot.ErrNotFound):
			writeJSONError(w, http.StatusNotFound, "canvas not found: "+id)
		default:
			writeJSONError(w, http.StatusInternalServerError, "creating snapshot failed")
		}
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"name": entry.Name, "dir": entry.Dir})
}

// handleRevertSnapshot serves POST /api/canvases/{id}/snapshots/{name}/revert
// (hub mode only): it reverts the canvas to the named snapshot via
// snapshot.RevertWithSafety -- the same resolve-target-first, then
// prerevert-safety-snapshot, then revert protocol cli.cmdRevert and the
// revert MCP tool run, so a typo'd name fails BEFORE any prerevert snapshot
// is taken. Bearer-gated via withHubGate. The snapshot name is required
// (it's a path segment); RevertWithSafety validates it as a bare path
// component before touching the filesystem.
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

	// Serialize with handlePush's swap sequence (and the other mutating
	// machine-API handlers) on the same canvas id: the revert's own
	// rename-aside/rename-in swap must not interleave with a push's.
	unlock := s.hubCfg.pushLocks.lock(id)
	defer unlock()

	entry, err := snapshot.RevertWithSafety(canvas.Dir(s.canvasesDir, id), s.cfg.VersionsDir(), id, name)
	if err != nil {
		// A missing snapshot is the client's error (404); everything else --
		// invalid name shapes included -- stays a generic 500 with no server
		// paths in the body.
		if errors.Is(err, snapshot.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "no such snapshot")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "revert failed (invalid name?)")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"reverted": id, "snapshot": entry.Name})
}
