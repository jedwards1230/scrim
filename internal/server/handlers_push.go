package server

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/jedwards1230/scrim/internal/canvas"
)

const (
	// maxPushBytes bounds the total uncompressed size of a pushed archive,
	// so a malicious or buggy client can't exhaust the hub's disk with one
	// request.
	maxPushBytes = 50 * 1024 * 1024 // 50 MiB
	// maxPushFiles bounds the total number of regular-file entries in a
	// pushed archive, independent of their size (a tar-bomb of many tiny
	// files is a separate attack from one huge file).
	maxPushFiles = 1000
)

// errPushTooLarge is returned by extractTar when an archive exceeds
// maxPushBytes or maxPushFiles.
var errPushTooLarge = errors.New("push: archive exceeds the hub's size or file-count limit")

// errPushBadEntry is returned by extractTar for a tar entry that would
// escape the staging root, or that isn't a plain file/directory (a
// symlink, hardlink, device, or fifo entry).
var errPushBadEntry = errors.New("push: unsafe or unsupported tar entry")

// handlePush serves POST /api/push/{id} (hub mode only, see routes.go): the
// request body is an uncompressed tar archive of a canvas's files, which is
// extracted into a staging directory, then atomically swapped into place as
// the canvas -- id/title/description/icon arrive as URL query params.
//
// Extraction always lands in a staging directory under the hub's data dir
// but OUTSIDE canvasesDir (so the filesystem watcher never fires on
// individual staged writes), and the swap into place
// (os.RemoveAll(canvasDir) + os.Rename(staging, canvasDir)) is the one
// filesystem event the watcher actually observes -- a single clean SSE
// reload, never a partial-serve of a canvas mid-extraction.
func (s *Server) handlePush(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := canvas.ValidateID(id); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	stagingRoot := filepath.Join(s.cfg.Dir, "push-staging")
	if err := os.MkdirAll(stagingRoot, 0o755); err != nil { //nolint:gosec // staging dir lives under the hub's owner-only data dir
		writeJSONError(w, http.StatusInternalServerError, "preparing staging area: "+err.Error())
		return
	}
	staging, err := os.MkdirTemp(stagingRoot, id+"-*")
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "creating staging directory: "+err.Error())
		return
	}
	// Cleared once the staging dir has been successfully renamed into place
	// below -- until then, every return path (including a panic-free error
	// return) must clean it up rather than leaking it under stagingRoot.
	stagingOwned := true
	defer func() {
		if stagingOwned {
			_ = os.RemoveAll(staging)
		}
	}()

	if err := extractTar(r.Body, staging, maxPushBytes, maxPushFiles); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errPushTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		writeJSONError(w, status, err.Error())
		return
	}

	if err := os.MkdirAll(s.canvasesDir, 0o755); err != nil { //nolint:gosec // canvases dir is a user-owned working directory
		writeJSONError(w, http.StatusInternalServerError, "preparing canvases directory: "+err.Error())
		return
	}
	canvasDir := canvas.Dir(s.canvasesDir, id)
	if err := os.RemoveAll(canvasDir); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "removing previous canvas contents: "+err.Error())
		return
	}
	if err := os.Rename(staging, canvasDir); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "swapping staged canvas into place: "+err.Error())
		return
	}
	// The staging directory no longer exists at its original path -- it
	// (or rather, its contents) now live at canvasDir, so there is nothing
	// left for the deferred cleanup above to remove.
	stagingOwned = false

	title := r.URL.Query().Get("title")
	description := r.URL.Query().Get("description")
	icon := r.URL.Query().Get("icon")
	if title != "" || description != "" || icon != "" {
		if _, err := canvas.Create(s.canvasesDir, s.metaDir, id, title, description, icon); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "writing canvas metadata: "+err.Error())
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"id":  id,
		"url": "/c/" + id + "/",
	})
}

// extractTar reads an uncompressed tar archive from r and extracts it into
// root, enforcing maxBytes (total uncompressed size across every regular
// file) and maxFiles (total regular-file count). Every entry's target path
// is validated to stay within root (see safeJoin); only regular files and
// directories are extracted -- symlinks, hardlinks, and device/fifo entries
// are rejected outright (errPushBadEntry) rather than silently skipped,
// since a hub is a shared, network-reachable service and any of those entry
// types could otherwise be used to escape root or clobber an arbitrary
// path.
func extractTar(r io.Reader, root string, maxBytes int64, maxFiles int) error {
	tr := tar.NewReader(r)
	var totalBytes int64
	var numFiles int
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading tar entry: %w", err)
		}

		target, err := safeJoin(root, hdr.Name)
		if err != nil {
			return err
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil { //nolint:gosec // target is validated by safeJoin to stay within root
				return fmt.Errorf("creating directory %q: %w", hdr.Name, err)
			}
		case tar.TypeReg:
			numFiles++
			if numFiles > maxFiles {
				return fmt.Errorf("%w: more than %d files", errPushTooLarge, maxFiles)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil { //nolint:gosec // target is validated by safeJoin to stay within root
				return fmt.Errorf("creating parent directory for %q: %w", hdr.Name, err)
			}
			n, err := writeTarFile(target, tr, maxBytes-totalBytes)
			if err != nil {
				return fmt.Errorf("writing %q: %w", hdr.Name, err)
			}
			totalBytes += n
		default:
			return fmt.Errorf("%w: entry %q has unsupported type %q", errPushBadEntry, hdr.Name, string(hdr.Typeflag))
		}
	}
}

// writeTarFile copies src (one tar entry's content) into a new regular file
// at target, permitting at most budget bytes -- one byte over that trips
// errPushTooLarge without ever buffering the (potentially huge) entry in
// memory first. target's final permission is explicitly set to 0o644
// regardless of the process umask.
func writeTarFile(target string, src io.Reader, budget int64) (int64, error) {
	if budget < 0 {
		budget = 0
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644) //nolint:gosec // target is validated by safeJoin to stay within root
	if err != nil {
		return 0, fmt.Errorf("creating file: %w", err)
	}

	n, copyErr := io.Copy(f, io.LimitReader(src, budget+1))
	closeErr := f.Close()
	if copyErr != nil {
		return n, fmt.Errorf("copying content: %w", copyErr)
	}
	if n > budget {
		return n, errPushTooLarge
	}
	if closeErr != nil {
		return n, fmt.Errorf("closing file: %w", closeErr)
	}
	if err := os.Chmod(target, 0o644); err != nil { //nolint:gosec // canvas content is not sensitive
		return n, fmt.Errorf("setting file permissions: %w", err)
	}
	return n, nil
}

// safeJoin resolves name (a tar entry's path) against root, rejecting
// anything that would place the result outside root -- an absolute path,
// a ".." that walks above root, or any mixture of the two. filepath.Join
// already applies filepath.Clean internally, so "a/../../escape.txt"
// collapses to a path one level above root exactly like "../escape.txt"
// does; either way, the prefix check below catches it. This deliberately
// REJECTS an escaping entry rather than silently rewriting it into
// somewhere else under root -- a pushed archive containing one is refused
// in its entirety (extractTar returns the error to its caller), not
// partially applied.
func safeJoin(root, name string) (string, error) {
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("%w: absolute path %q", errPushBadEntry, name)
	}
	target := filepath.Join(root, name)
	if target != root && !strings.HasPrefix(target, root+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q escapes the staging root", errPushBadEntry, name)
	}
	return target, nil
}
