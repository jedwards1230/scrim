package server

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// errOutsideRoot is returned by resolveStaticPath when the requested
// subpath would resolve outside the canvas root, whether via "..",
// absolute-path tricks, or a symlink escape.
var errOutsideRoot = errors.New("server: resolved path escapes canvas root")

// resolveStaticPath resolves subpath (the wildcard portion of a request
// after /c/<id>/) against canvasRoot, guaranteeing the result cannot escape
// canvasRoot. It:
//
//   - rejects any path component starting with "." (blocks dotfiles like
//     the canvas metadata sidecar, and neutralizes ".."/"." components),
//   - clamps ".."-style traversal by treating subpath as rooted,
//   - rejects the result if, after resolving symlinks, it still falls
//     outside canvasRoot.
//
// It does not decide directory-vs-file or existence — see
// resolveServablePath for that.
func resolveStaticPath(canvasRoot, subpath string) (string, error) {
	for _, part := range strings.Split(subpath, "/") {
		if part != "" && strings.HasPrefix(part, ".") {
			return "", errOutsideRoot
		}
	}

	// Treating the subpath as absolute makes path.Clean clamp any leading
	// ".." at the root instead of walking above it.
	cleaned := path.Clean("/" + subpath)

	rootAbs, err := filepath.Abs(canvasRoot)
	if err != nil {
		return "", err
	}
	target := filepath.Join(rootAbs, filepath.FromSlash(cleaned))

	if target != rootAbs && !strings.HasPrefix(target, rootAbs+string(os.PathSeparator)) {
		return "", errOutsideRoot
	}

	resolvedRoot, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", err // canvas root itself doesn't exist / unreadable
	}
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		if os.IsNotExist(err) {
			return target, nil // caller maps missing paths to 404
		}
		return "", err
	}
	if resolvedTarget != resolvedRoot && !strings.HasPrefix(resolvedTarget, resolvedRoot+string(os.PathSeparator)) {
		return "", errOutsideRoot
	}
	return target, nil
}

// indexFileNames lists directory-index candidates in preference order:
// index.html wins when both are present, index.md is the fallback (see
// resolveServablePath).
var indexFileNames = []string{"index.html", "index.md"}

// resolveServablePath resolves subpath against canvasRoot and applies
// static-file directory semantics: a directory request serves that
// directory's index.html if present, falling back to index.md if not, and
// 404s (never a directory listing) if neither exists.
//
// The second return value reports whether the returned path was reached via
// this directory-index fallback, as opposed to a direct file request --
// handleCanvas uses it to decide whether an index.md's content should be
// rendered as markdown, without also rendering a directly-requested
// notes.md the same way.
func resolveServablePath(canvasRoot, subpath string) (target string, viaIndex bool, err error) {
	resolved, err := resolveStaticPath(canvasRoot, subpath)
	if err != nil {
		return "", false, err
	}
	fi, err := os.Stat(resolved)
	if err != nil {
		return "", false, err
	}
	if !fi.IsDir() {
		return resolved, false, nil
	}

	for _, name := range indexFileNames {
		// Route the synthesized index path back through resolveStaticPath
		// rather than a raw filepath.Join+os.Stat: the directory itself was
		// already validated against a symlink escape, but a symlink named
		// index.html/index.md *inside* an otherwise-legitimate directory
		// (e.g. pointing at /etc/passwd) is a separate escape that only
		// resolving+checking the final file path catches.
		indexPath, err := resolveStaticPath(canvasRoot, path.Join(subpath, name))
		if err != nil {
			return "", false, err
		}
		indexFi, statErr := os.Stat(indexPath)
		if statErr != nil || indexFi.IsDir() {
			continue
		}
		return indexPath, true, nil
	}
	return "", false, os.ErrNotExist
}
