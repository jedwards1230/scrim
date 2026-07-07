// Package dircopy recursively copies a canvas directory tree, shared by the
// hub machine API's copy route and the MCP server's local copy backend (see
// issue #43) so both duplicate a canvas with identical semantics. It is a pure
// leaf package (stdlib only), importable from both internal/server and
// internal/mcpserver -- neither imports the other.
//
// Unlike a general-purpose tree copy it copies REGULAR FILES AND DIRECTORIES
// ONLY: symlinks, hardlinks, devices, and fifos are refused (ErrUnsupported),
// the same stance the hub's tar-push extraction takes. A canvas is only ever
// built from pushed/PUT regular files, so anything else is out-of-band
// tampering a copy must not propagate (a symlink could point outside the
// destination tree). maxBytes/maxEntries bound a pathological source.
package dircopy

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// ErrUnsupported reports a source entry that isn't a regular file or directory
// (a symlink, device, fifo, etc.) -- the copy is refused rather than silently
// skipping or dereferencing it.
var ErrUnsupported = errors.New("dircopy: unsupported source entry (only regular files and directories are copied)")

// ErrTooLarge reports that the source exceeds maxBytes of total content or
// maxEntries of files+directories.
var ErrTooLarge = errors.New("dircopy: source exceeds the size or entry-count limit")

// Copy recursively copies src into dst. dst is created if absent (an existing
// empty dst -- e.g. a freshly-made staging temp dir -- is fine). Regular files
// are copied with 0o644 permissions and directories with 0o755; every other
// entry type fails with ErrUnsupported. Total copied content is bounded by
// maxBytes and total entries by maxEntries (ErrTooLarge past either), matching
// the caps the hub's push extraction enforces.
func Copy(src, dst string, maxBytes int64, maxEntries int) error {
	if err := os.MkdirAll(dst, 0o755); err != nil { //nolint:gosec // dst is a caller-owned staging/canvas dir, not sensitive
		return fmt.Errorf("creating copy destination: %w", err)
	}

	var totalBytes int64
	var numEntries int
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil // the src root itself maps to dst, already created
		}

		numEntries++
		if numEntries > maxEntries {
			return fmt.Errorf("%w: more than %d entries", ErrTooLarge, maxEntries)
		}
		target := filepath.Join(dst, rel)

		switch {
		case d.IsDir():
			if err := os.MkdirAll(target, 0o755); err != nil { //nolint:gosec // target is within the caller-owned dst tree
				return fmt.Errorf("creating directory %q: %w", rel, err)
			}
			return nil
		case d.Type().IsRegular():
			n, err := copyFile(path, target, maxBytes-totalBytes)
			if err != nil {
				return fmt.Errorf("copying %q: %w", rel, err)
			}
			totalBytes += n
			return nil
		default:
			// Symlink, device, fifo, socket: refuse the whole copy.
			return fmt.Errorf("%w: %q", ErrUnsupported, rel)
		}
	})
}

// copyFile copies src to a new regular file at target, permitting at most
// budget bytes -- one byte over trips ErrTooLarge without buffering the whole
// file in memory. The parent directory is created if the walk hasn't reached
// it yet. target's permission is 0o644 regardless of umask, matching the hub's
// PUT/push writes.
func copyFile(src, target string, budget int64) (int64, error) {
	if budget < 0 {
		budget = 0
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil { //nolint:gosec // target is within the caller-owned dst tree
		return 0, err
	}
	in, err := os.Open(src) //nolint:gosec // src is walked from a caller-provided canvas dir, not arbitrary input
	if err != nil {
		return 0, err
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644) //nolint:gosec // target is within the caller-owned dst tree
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(out, io.LimitReader(in, budget+1))
	closeErr := out.Close()
	if copyErr != nil {
		return n, copyErr
	}
	if n > budget {
		return n, ErrTooLarge
	}
	if closeErr != nil {
		return n, closeErr
	}
	return n, nil
}
