package server

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"

	"github.com/jedwards1230/scrim/internal/canvas"
	"github.com/jedwards1230/scrim/internal/fileedit"
)

// maxFileBytes bounds a single file written through PUT
// /api/canvases/{id}/files/{path...}. Canvas files are agent-authored HTML/JS/
// CSS/markdown -- 2 MiB is generous headroom for any single one while still
// capping a malicious or buggy client's per-request write. It is deliberately
// far below maxPushBytes (a whole-canvas archive), since this endpoint writes
// exactly one file per call.
const maxFileBytes = 2 * 1024 * 1024 // 2 MiB

// maxEditBodyBytes bounds a PATCH edit's JSON body: old_string and new_string
// can EACH legitimately approach a whole file's size (maxFileBytes), and JSON
// string escaping inflates them further -- so budget two file-sized strings
// plus a whole extra file's worth of escaping/envelope headroom. The cap on
// the edited RESULT is separate and stays maxFileBytes (fileedit.Apply).
const maxEditBodyBytes = 3 * maxFileBytes // 6 MiB

// verifyResolvedWithin defends the file endpoints against symlinks planted
// inside a canvas directory: safeJoin's containment check is lexical only, so
// after it passes, the already-existing portion of the path is resolved with
// EvalSymlinks and re-checked against the resolved root -- the same
// defense-in-depth resolveStaticPath and resolveSnapshotDir apply. A p that
// doesn't exist yet is checked via its deepest existing ancestor, so a
// symlinked parent directory can't redirect a write outside the canvas.
// (Symlinks can't normally enter a canvas -- push extraction refuses them and
// PUT writes regular files -- so this guards out-of-band tampering.)
func verifyResolvedWithin(root, p string) error {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	// Walk up from p to the deepest component that exists, then resolve it.
	probe := p
	for {
		if _, err := os.Lstat(probe); err == nil {
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}
	resolved, err := filepath.EvalSymlinks(probe)
	if err != nil {
		return err
	}
	if resolved != resolvedRoot && !filepath.IsLocal(mustRel(resolvedRoot, resolved)) {
		return errPushBadEntry
	}
	return nil
}

// mustRel returns the relative path from base to target, or a marker that
// fails filepath.IsLocal when target is not under base.
func mustRel(base, target string) string {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return ".." // not relative -> fails IsLocal
	}
	return rel
}

// handleReadCanvasFile serves GET /api/canvases/{id}/files/{path...} (hub mode
// only, see routes.go): it returns one file's raw bytes from within a canvas
// directory. Bearer-gated by withHubGate like every hub machine endpoint --
// reads included -- so canvas content is never served to an unauthenticated
// caller. The {path...} is resolved under the canvas root via safeJoin (the
// same traversal guard handlePush uses), so it can never escape the canvas.
func (s *Server) handleReadCanvasFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := canvas.ValidateID(id); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	rel := r.PathValue("path")
	if rel == "" {
		writeJSONError(w, http.StatusBadRequest, "file path is required")
		return
	}

	root := canvas.Dir(s.canvasesDir, id)
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		writeJSONError(w, http.StatusNotFound, "canvas not found: "+id)
		return
	}

	target, err := safeJoin(root, rel)
	if err != nil {
		// A traversal/absolute-path attempt: refuse without disclosing the
		// resolved path.
		writeJSONError(w, http.StatusBadRequest, "invalid file path")
		return
	}
	if err := verifyResolvedWithin(root, target); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid file path")
		return
	}

	f, err := os.Open(target) //nolint:gosec // target is validated by safeJoin to stay within the canvas root
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeJSONError(w, http.StatusNotFound, "file not found")
			return
		}
		// Generic message on purpose: raw os errors can embed absolute
		// server-side paths, and this surface hides paths everywhere else.
		writeJSONError(w, http.StatusInternalServerError, "opening file failed")
		return
	}
	defer f.Close() //nolint:errcheck // read-only handle, close error not actionable

	fi, err := f.Stat()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "stat file failed")
		return
	}
	if fi.IsDir() {
		writeJSONError(w, http.StatusNotFound, "file not found")
		return
	}

	if ct := mime.TypeByExtension(filepath.Ext(target)); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Canvas content is agent-authored and potentially sensitive: keep it out
	// of every cache entirely, matching the static canvas handler's policy.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}

// handleWriteCanvasFile serves PUT /api/canvases/{id}/files/{path...} (hub mode
// only): it writes one file's raw body into an EXISTING canvas directory. The
// canvas must already exist (the client adds it first via POST /api/canvases);
// a missing canvas is a 404 rather than being created implicitly. The body is
// capped at maxFileBytes via http.MaxBytesReader and the write is atomic (temp
// file in the same canvas dir, then rename over the target), so the fsnotify
// watcher observes one clean event and broadcasts a single SSE reload -- never
// a partial file.
func (s *Server) handleWriteCanvasFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := canvas.ValidateID(id); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	rel := r.PathValue("path")
	if rel == "" {
		writeJSONError(w, http.StatusBadRequest, "file path is required")
		return
	}

	// Read the body fully BEFORE taking the per-canvas lock (mirroring
	// handlePush, which extracts the tar into staging first), so a slow
	// client can't hold the lock across its upload.
	r.Body = http.MaxBytesReader(w, r.Body, maxFileBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSONError(w, http.StatusRequestEntityTooLarge,
				fmtFileTooLarge())
			return
		}
		writeJSONError(w, http.StatusBadRequest, "reading request body: "+err.Error())
		return
	}

	// Serialize with handlePush's rename-aside/swap sequence (and the other
	// mutating machine-API handlers) on the same canvas id: without the lock
	// a PUT landing between push's two renames would write into a directory
	// about to be swapped away, leaving the served canvas holding only the
	// PUT file (or losing the PUT entirely).
	unlock := s.hubCfg.pushLocks.lock(id)
	defer unlock()

	root := canvas.Dir(s.canvasesDir, id)
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		writeJSONError(w, http.StatusNotFound, "canvas not found: "+id+" (add it before writing files)")
		return
	}

	target, err := safeJoin(root, rel)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid file path")
		return
	}

	// safeJoin guarantees target stays lexically within root; the resolved
	// re-check refuses a path routed through a planted symlink before any
	// directory is created or byte written.
	if err := verifyResolvedWithin(root, target); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid file path")
		return
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil { //nolint:gosec // parent lives under the canvas working dir, within-root per safeJoin
		writeJSONError(w, http.StatusInternalServerError, "creating parent directory failed")
		return
	}
	// Re-verify after MkdirAll: the final parent now exists, so the resolved
	// check covers the exact directory the write lands in.
	if err := verifyResolvedWithin(root, filepath.Dir(target)); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid file path")
		return
	}

	if err := atomicWriteFile(target, filepath.Dir(target), data); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "writing file failed")
		return
	}

	// 204: the write succeeded and the fsnotify watcher will broadcast the
	// SSE reload; no body.
	w.WriteHeader(http.StatusNoContent)
}

// handleEditCanvasFile serves PATCH /api/canvases/{id}/files/{path...} (hub
// mode only): it applies an exact-string replacement (fileedit.Apply) to one
// EXISTING file server-side, so a remote MCP client's edit costs bytes
// proportional to the change, not the file. Same guards as the PUT handler
// (canvas-must-exist 404, safeJoin + resolved re-check), plus file-must-exist
// 404 -- an edit never creates a file. The write is the same atomic
// temp+rename, so fsnotify observes one clean event and broadcasts a single
// SSE reload. Conflict outcomes (old_string absent / ambiguous) are 409s with
// fileedit's path-free messages.
func (s *Server) handleEditCanvasFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := canvas.ValidateID(id); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	rel := r.PathValue("path")
	if rel == "" {
		writeJSONError(w, http.StatusBadRequest, "file path is required")
		return
	}

	// Decode the body fully BEFORE taking the per-canvas lock (mirroring
	// handlePush, which extracts the tar into staging first), so a slow
	// client can't hold the lock across its upload.
	r.Body = http.MaxBytesReader(w, r.Body, maxEditBodyBytes)
	var body struct {
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, fmtEditBodyTooLarge())
			return
		}
		writeJSONError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	// Serialize with handlePush's rename-aside/swap sequence (and the other
	// mutating machine-API handlers) on the same canvas id, so the
	// read-modify-write below can't interleave with a push swapping the
	// canvas directory out from under it.
	unlock := s.hubCfg.pushLocks.lock(id)
	defer unlock()

	root := canvas.Dir(s.canvasesDir, id)
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		writeJSONError(w, http.StatusNotFound, "canvas not found: "+id)
		return
	}

	target, err := safeJoin(root, rel)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid file path")
		return
	}
	if err := verifyResolvedWithin(root, target); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid file path")
		return
	}

	fi, err := os.Stat(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeJSONError(w, http.StatusNotFound, "file not found")
			return
		}
		// Generic message on purpose: raw os errors can embed absolute
		// server-side paths, and this surface hides paths everywhere else.
		writeJSONError(w, http.StatusInternalServerError, "stat file failed")
		return
	}
	if fi.IsDir() {
		writeJSONError(w, http.StatusNotFound, "file not found")
		return
	}
	// Mirror the read/write cap on the edit source: the whole file is read
	// into memory to apply the replacement.
	if fi.Size() > maxFileBytes {
		writeJSONError(w, http.StatusRequestEntityTooLarge, fmtFileTooLarge())
		return
	}

	data, err := os.ReadFile(target) //nolint:gosec // target is validated by safeJoin to stay within the canvas root
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "reading file failed")
		return
	}
	edited, replacements, err := fileedit.Apply(data, body.OldString, body.NewString, body.ReplaceAll, maxFileBytes)
	if err != nil {
		// fileedit messages are path-free by construction, so they are safe
		// to serve verbatim.
		writeJSONError(w, editApplyStatus(err), err.Error())
		return
	}

	if err := atomicWriteFile(target, filepath.Dir(target), edited); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "writing file failed")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"path": rel, "replacements": replacements})
}

// editApplyStatus maps a fileedit.Apply failure to its HTTP status: 409 for
// an edit conflict (old_string absent, or ambiguous without replace_all), 413
// when the edited result would exceed the per-file cap, and 400 for the pure
// input errors (empty old_string, old_string == new_string).
func editApplyStatus(err error) int {
	var multi *fileedit.MultipleMatchesError
	var large *fileedit.TooLargeError
	switch {
	case errors.Is(err, fileedit.ErrNotFound), errors.As(err, &multi):
		return http.StatusConflict
	case errors.As(err, &large):
		return http.StatusRequestEntityTooLarge
	default:
		return http.StatusBadRequest
	}
}

// atomicWriteFile writes data to a temp file created in dir (the target's own
// directory, so the final rename is same-filesystem and therefore atomic) and
// renames it over target. The temp file is removed on any error path so a
// failed write never leaves debris beside the canvas file.
func atomicWriteFile(target, dir string, data []byte) (err error) {
	tmp, err := os.CreateTemp(dir, ".scrim-write-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	// Owned until the rename succeeds: every early return removes it.
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Chmod(tmpPath, 0o644); err != nil { //nolint:gosec // canvas content is not sensitive
		return err
	}
	if err = os.Rename(tmpPath, target); err != nil {
		return err
	}
	renamed = true
	return nil
}

// fmtFileTooLarge is the 413 message for an oversize PUT body, stated in
// bytes so the caller can size a retry.
func fmtFileTooLarge() string {
	return "file exceeds the 2097152-byte (2 MiB) per-file limit"
}

// fmtEditBodyTooLarge is the 413 message for an oversize PATCH edit body --
// deliberately distinct from fmtFileTooLarge: it's the edit REQUEST that blew
// the maxEditBodyBytes cap, not a file exceeding the per-file limit.
func fmtEditBodyTooLarge() string {
	return "edit request body exceeds the 6291456-byte (6 MiB) limit"
}
