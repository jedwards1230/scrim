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
	// maxPushEntries bounds the total number of entries -- regular files AND
	// directories -- in a pushed archive, independent of their size. A
	// tar-bomb of many tiny files, or of many empty directories, is a
	// separate attack from one huge file: every entry costs an inode and a
	// syscall (MkdirAll/OpenFile) regardless of its content length, so a
	// directory entry has to count against this cap exactly like a file does.
	maxPushEntries = 1000
	// maxPushBodyBytes is a coarse ceiling on the ENTIRE request body (tar
	// structure + file contents), enforced via http.MaxBytesReader as
	// defense-in-depth on top of the per-content maxPushBytes and per-entry
	// maxPushEntries caps that extractTar applies. It's maxPushBytes plus
	// generous headroom for tar's 512-byte entry headers and padding.
	maxPushBodyBytes = maxPushBytes + 8*1024*1024 // 50 MiB content + 8 MiB tar overhead
)

// renameStagedSwap performs the one failure-prone step of handlePush's swap
// sequence: renaming the freshly staged canvas directory into place over the
// (already moved-aside) previous one. It's a package-level seam -- os.Rename
// in production -- solely so a test can force that rename to fail and exercise
// the rollback path that restores the moved-aside canvas. That failure can't
// be provoked portably through the filesystem alone: the aside-move and the
// staged-swap renames operate within the same parent directory, so no
// permission or path-collision setup fails the swap without also failing the
// aside move that must succeed first. The rollback restore itself deliberately
// stays on os.Rename (not this seam), so the rollback exercises the real
// filesystem.
var renameStagedSwap = os.Rename

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
// individual staged writes), and the swap into place (move any existing
// canvas aside, rename the staged copy in, then delete the aside copy --
// see the swap-then-delete sequence below) is what the watcher observes:
// a single clean SSE reload, never a partial-serve of a canvas
// mid-extraction, and never a stranded/empty canvas if the critical rename
// fails. Concurrent pushes to the SAME id are serialized by a per-id lock.
func (s *Server) handlePush(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := canvas.ValidateID(id); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Coarse outer ceiling on the whole request body, independent of the
	// finer per-content/per-entry caps extractTar enforces: a client can't
	// stream an unbounded body at the hub even before extractTar gets to
	// reason about individual entries.
	r.Body = http.MaxBytesReader(w, r.Body, maxPushBodyBytes)

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

	if err := extractTar(r.Body, staging, maxPushBytes, maxPushEntries); err != nil {
		status := http.StatusBadRequest
		var maxErr *http.MaxBytesError
		if errors.Is(err, errPushTooLarge) || errors.As(err, &maxErr) {
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

	// Serialize concurrent pushes to the same canvas id so their swap
	// sequences can't interleave (two clients racing on the aside/rename/
	// delete steps would otherwise silently discard one's content). Pushes
	// to DIFFERENT ids proceed in parallel.
	unlock := s.hubCfg.pushLocks.lock(id)
	defer unlock()

	// Swap-then-delete: move any existing canvas aside, rename the freshly
	// staged copy into place, then delete the aside copy. This ordering
	// means the one failure-prone step (rename staging -> canvasDir) can be
	// rolled back by restoring the aside copy, so a failed push can never
	// leave the canvas stranded as a 404 the way delete-then-rename would if
	// its second step failed. The aside copy lives under stagingRoot
	// (outside canvasesDir), so moving it there doesn't itself register a
	// servable canvas.
	var aside string
	if _, err := os.Stat(canvasDir); err == nil {
		tmp, err := os.MkdirTemp(stagingRoot, id+"-old-*")
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "preparing canvas swap: "+err.Error())
			return
		}
		// os.Rename needs the destination not to exist; MkdirTemp created it
		// only to reserve a unique name, so remove it before renaming into it.
		if err := os.RemoveAll(tmp); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "preparing canvas swap: "+err.Error())
			return
		}
		if err := os.Rename(canvasDir, tmp); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "moving previous canvas aside: "+err.Error())
			return
		}
		aside = tmp
	} else if !errors.Is(err, os.ErrNotExist) {
		writeJSONError(w, http.StatusInternalServerError, "checking existing canvas: "+err.Error())
		return
	}

	if err := renameStagedSwap(staging, canvasDir); err != nil {
		// Roll back: put the previous canvas back where it was so this failed
		// push leaves the canvas exactly as it found it rather than missing.
		if aside != "" {
			_ = os.Rename(aside, canvasDir)
		}
		writeJSONError(w, http.StatusInternalServerError, "swapping staged canvas into place: "+err.Error())
		return
	}
	// The staging directory no longer exists at its original path -- its
	// contents now live at canvasDir, so there is nothing left for the
	// deferred cleanup above to remove.
	stagingOwned = false
	// The new canvas is in place; drop the previous copy. Best-effort: a
	// failure here leaves an orphan under stagingRoot but doesn't affect the
	// served canvas.
	if aside != "" {
		_ = os.RemoveAll(aside)
	}

	title := r.URL.Query().Get("title")
	description := r.URL.Query().Get("description")
	icon := r.URL.Query().Get("icon")
	// Attribute ownership to the pushing principal (admin for the push token;
	// #51 will carry a CF-forwarded actor), but never clobber an existing owner
	// -- a later claim/transfer (#55) must win over a re-push. Passing owner ""
	// to canvas.Create leaves any existing owner in place and preserves grants,
	// so this also records metadata even when no title/description/icon is given.
	owner := ownerFromClaims(claimsFrom(r.Context()))
	existingOwner, _, _ := canvas.GetOwnerGrants(s.metaDir, id)
	if existingOwner != "" {
		owner = existingOwner
	}
	if _, err := canvas.Create(s.canvasesDir, s.metaDir, id, title, description, icon, owner); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "writing canvas metadata: "+err.Error())
		return
	}
	// Apply the pushing token's auto-share grants, but only when this push
	// created the canvas (no prior owner) -- a re-push must never re-add them.
	if existingOwner == "" {
		s.applyAutoShare(id, tokenFrom(r.Context()))
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"id":  id,
		"url": "/c/" + id + "/",
	})
}

// extractTar reads an uncompressed tar archive from r and extracts it into
// root, enforcing maxBytes (total uncompressed size across every regular
// file) and maxEntries (total entry count -- files and directories alike).
// Every entry's target path is validated to stay within root (see
// safeJoin); only regular files and directories are extracted -- symlinks,
// hardlinks, and device/fifo entries are rejected outright (errPushBadEntry)
// rather than silently skipped, since a hub is a shared, network-reachable
// service and any of those entry types could otherwise be used to escape
// root or clobber an arbitrary path.
func extractTar(r io.Reader, root string, maxBytes int64, maxEntries int) error {
	tr := tar.NewReader(r)
	var totalBytes int64
	var numEntries int
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading tar entry: %w", err)
		}

		// Count every entry, not just regular files: an archive of a million
		// empty directories drives a million MkdirAll calls and inodes just
		// as surely as a million tiny files would, so both count against the
		// same cap.
		numEntries++
		if numEntries > maxEntries {
			return fmt.Errorf("%w: more than %d entries", errPushTooLarge, maxEntries)
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
