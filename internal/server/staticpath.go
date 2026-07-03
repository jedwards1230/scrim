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

// resolveServablePath resolves subpath against canvasRoot and applies
// static-file directory semantics: a directory request serves that
// directory's index.html if present, and 404s (never a directory listing)
// otherwise.
func resolveServablePath(canvasRoot, subpath string) (string, error) {
	target, err := resolveStaticPath(canvasRoot, subpath)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(target)
	if err != nil {
		return "", err
	}
	if fi.IsDir() {
		indexPath := filepath.Join(target, "index.html")
		if _, err := os.Stat(indexPath); err != nil {
			return "", os.ErrNotExist
		}
		return indexPath, nil
	}
	return target, nil
}
