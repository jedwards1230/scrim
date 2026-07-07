package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/jedwards1230/scrim/internal/canvas"
	"github.com/jedwards1230/scrim/internal/dircopy"
	"github.com/jedwards1230/scrim/internal/snapshot"
)

// handleCopyCanvas serves POST /api/canvases/{id}/copy (hub mode only, see
// routes.go): it duplicates canvas {id} into a new canvas named by the JSON
// body's "to" field. The copy is staged in a directory OUTSIDE canvasesDir
// (so the filesystem watcher never fires on individual staged writes) and
// atomically renamed into place -- the same staged-then-swapped discipline
// handlePush uses, so a client sees one clean SSE reload, never a half-copied
// canvas. A target that already exists is a 409 UNLESS "overwrite": true, in
// which case the existing target is snapshotted first (so the overwrite is
// itself undoable) and then replaced. Bearer-gated via withHubGate.
func (s *Server) handleCopyCanvas(w http.ResponseWriter, r *http.Request) {
	from := r.PathValue("id")
	if err := canvas.ValidateID(from); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	// The only expected body is a small {"to": ..., "overwrite": ...}; cap it.
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var body struct {
		To        string `json:"to"`
		Overwrite bool   `json:"overwrite"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeJSONError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	to := body.To
	if err := canvas.ValidateID(to); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid target id: "+err.Error())
		return
	}
	if from == to {
		writeJSONError(w, http.StatusBadRequest, "source and target are the same canvas")
		return
	}

	sourceDir := canvas.Dir(s.canvasesDir, from)
	if fi, err := os.Stat(sourceDir); err != nil || !fi.IsDir() {
		writeJSONError(w, http.StatusNotFound, "canvas not found: "+from)
		return
	}

	// Lock BOTH canvas ids so the copy can't interleave with a concurrent push
	// to either the source (which would tear the read mid-walk) or the target
	// (which races the swap). Acquire in a fixed lexical order so two copies
	// touching the same pair can't deadlock. from != to is guaranteed above.
	firstKey, secondKey := from, to
	if firstKey > secondKey {
		firstKey, secondKey = secondKey, firstKey
	}
	unlockFirst := s.hubCfg.pushLocks.lock(firstKey)
	defer unlockFirst()
	unlockSecond := s.hubCfg.pushLocks.lock(secondKey)
	defer unlockSecond()

	targetDir := canvas.Dir(s.canvasesDir, to)
	targetExists := false
	if fi, err := os.Stat(targetDir); err == nil && fi.IsDir() {
		targetExists = true
	}
	if targetExists && !body.Overwrite {
		writeJSONError(w, http.StatusConflict, "target canvas already exists: "+to+" (set overwrite to replace it)")
		return
	}

	stagingRoot := filepath.Join(s.cfg.Dir, "push-staging")
	if err := os.MkdirAll(stagingRoot, 0o755); err != nil { //nolint:gosec // staging dir lives under the hub's owner-only data dir
		writeJSONError(w, http.StatusInternalServerError, "preparing staging area: "+err.Error())
		return
	}
	staging, err := os.MkdirTemp(stagingRoot, to+"-copy-*")
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "creating staging directory: "+err.Error())
		return
	}
	stagingOwned := true
	defer func() {
		if stagingOwned {
			_ = os.RemoveAll(staging)
		}
	}()

	if err := dircopy.Copy(sourceDir, staging, maxPushBytes, maxPushEntries); err != nil {
		if errors.Is(err, dircopy.ErrTooLarge) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "source canvas exceeds the copy size or file-count limit")
			return
		}
		// ErrUnsupported (a symlink/device in the source) and any I/O error
		// stay a generic 500: canvas.Files/push never create such entries, so
		// this is server-side tampering the client can't act on, and the body
		// must not leak paths.
		writeJSONError(w, http.StatusInternalServerError, "copying canvas failed")
		return
	}

	if err := os.MkdirAll(s.canvasesDir, 0o755); err != nil { //nolint:gosec // canvases dir is a user-owned working directory
		writeJSONError(w, http.StatusInternalServerError, "preparing canvases directory: "+err.Error())
		return
	}

	// Overwrite: snapshot the soon-to-be-replaced target first, so the copy is
	// undoable via revert. Taken under the lock, after staging succeeded, so a
	// failed copy never leaves a spurious snapshot behind.
	if targetExists {
		if _, err := snapshot.Create(targetDir, s.cfg.VersionsDir(), to, "precopy"); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "snapshotting target before overwrite failed")
			return
		}
	}

	// Swap-then-delete, mirroring handlePush: move any existing target aside,
	// rename the staged copy into place, then delete the aside copy. A failed
	// rename rolls the aside copy back, so the target is never left stranded.
	var aside string
	if targetExists {
		tmp, err := os.MkdirTemp(stagingRoot, to+"-copy-old-*")
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "preparing canvas swap: "+err.Error())
			return
		}
		if err := os.RemoveAll(tmp); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "preparing canvas swap: "+err.Error())
			return
		}
		if err := os.Rename(targetDir, tmp); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "moving previous canvas aside: "+err.Error())
			return
		}
		aside = tmp
	}

	if err := os.Rename(staging, targetDir); err != nil {
		if aside != "" {
			_ = os.Rename(aside, targetDir) // best-effort restore
		}
		writeJSONError(w, http.StatusInternalServerError, "swapping copied canvas into place: "+err.Error())
		return
	}
	stagingOwned = false
	if aside != "" {
		_ = os.RemoveAll(aside)
	}

	// Carry the source's authored metadata (title/description/icon) onto the
	// copy. CopyMeta duplicates only explicit metadata -- a derived default
	// icon stays derived from the target's own id -- and clears stale target
	// metadata on an overwrite. The content swap is already committed here, so
	// a metadata failure returns 500 with the files already copied; metadata is
	// non-critical and the overwrite path already snapshotted the old target.
	if err := canvas.CopyMeta(s.metaDir, from, to); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "copying canvas metadata failed")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"from": from,
		"to":   to,
		"url":  "/c/" + to + "/",
	})
}
