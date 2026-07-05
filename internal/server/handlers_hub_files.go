package server

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"

	"github.com/jedwards1230/scrim/internal/canvas"
)

// maxFileBytes bounds a single file written through PUT
// /api/canvases/{id}/files/{path...}. Canvas files are agent-authored HTML/JS/
// CSS/markdown -- 2 MiB is generous headroom for any single one while still
// capping a malicious or buggy client's per-request write. It is deliberately
// far below maxPushBytes (a whole-canvas archive), since this endpoint writes
// exactly one file per call.
const maxFileBytes = 2 * 1024 * 1024 // 2 MiB

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
