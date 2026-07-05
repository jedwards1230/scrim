package mcpserver

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// maxFileBytes bounds a single file read_file/write_file handles, matching the
// hub machine API's own per-file cap (server.maxFileBytes). Enforced
// client-side in both backends so an oversize write fails fast with a clear
// message before any bytes cross the wire (hub mode) or touch disk (local).
const maxFileBytes = 2 * 1024 * 1024 // 2 MiB

// backend is the seam between the MCP tool handlers and the two ways scrim can
// drive a canvas store: localBackend (the local daemon + on-disk canvas dir,
// the same primitives the CLI verbs use) and hubBackend (a remote hub's
// bearer-authenticated machine API over HTTP). Every tool handler calls only
// through this interface, so the tool surface and behaviour are identical
// across transports; the one exception is the local-only `path` tool, which is
// simply not registered in hub mode (a server-local path is meaningless to a
// remote client).
type backend interface {
	List(ctx context.Context) ([]CanvasInfo, error)
	Add(ctx context.Context, id, title, description, icon string) (CanvasInfo, error)
	Remove(ctx context.Context, id string) error
	Status(ctx context.Context) (StatusInfo, error)
	// Link returns the view URL(s) for a canvas, or the dashboard URL when id
	// is empty. URLs are returned as data -- no browser is ever launched.
	Link(ctx context.Context, id string) ([]string, error)
	Snap(ctx context.Context, id, label string) (SnapInfo, error)
	Snaps(ctx context.Context, id string) ([]SnapInfo, error)
	// Revert restores a canvas from a snapshot; an empty name selects the
	// latest. A safety snapshot of the pre-revert contents is taken first.
	Revert(ctx context.Context, id, name string) (RevertInfo, error)
	ReadFile(ctx context.Context, id, path string) ([]byte, error)
	WriteFile(ctx context.Context, id, path string, content []byte) error
	// EditFile applies an exact-string replacement (fileedit.Apply semantics)
	// to one existing file server-side -- the token-efficient alternative to
	// WriteFile: only the changed strings cross the wire in hub mode, never
	// the whole file.
	EditFile(ctx context.Context, id, path, oldStr, newStr string, replaceAll bool) (EditInfo, error)
}

// CanvasInfo is one canvas as returned by List/Add. URL is the view URL
// (token-qualified in local mode; hub-base-relative in hub mode). Dir is the
// canvas's on-disk directory on whichever machine hosts the store -- local in
// local mode, the hub's own filesystem in hub mode (informational only).
type CanvasInfo struct {
	ID         string
	Title      string
	URL        string
	Dir        string
	Icon       string
	Color      string
	ModifiedAt time.Time
	SSEClients int
}

// StatusInfo reports daemon/hub status. Running is false when no local daemon
// is healthy (local mode); hubBackend always reports Running true on a
// successful status call.
type StatusInfo struct {
	Running            bool
	PID                int
	Host               string
	Port               int
	Version            string
	UptimeSeconds      float64
	CanvasCount        int
	SSEClients         int
	IdleSeconds        float64
	IdleTimeoutSeconds float64
}

// SnapInfo is one snapshot. Name/Dir are set by Snap; Snaps additionally
// populates Timestamp and Label.
type SnapInfo struct {
	Name      string
	Dir       string
	Timestamp time.Time
	Label     string
}

// RevertInfo is the outcome of a revert.
type RevertInfo struct {
	Reverted string
	Snapshot string
}

// EditInfo is the outcome of an EditFile: the edited path and how many
// replacements were made (1 without replace_all; the occurrence count with).
type EditInfo struct {
	Path         string
	Replacements int
}

// safeJoinLocal resolves name (a caller-supplied relative file path) against
// root, rejecting anything that would escape root: an absolute path or a ".."
// that walks above it. It mirrors server.safeJoin exactly rather than
// importing it -- mcpserver deliberately does not depend on internal/server.
func safeJoinLocal(root, name string) (string, error) {
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("invalid file path %q: absolute paths are not allowed", name)
	}
	target := filepath.Join(root, name)
	if target != root && !strings.HasPrefix(target, root+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid file path %q: escapes the canvas directory", name)
	}
	return target, nil
}

// cleanRelPath is the shared validation/canonicalization layer BOTH backends
// run on a caller-supplied relative file path before touching disk (local) or
// building a request URL (hub), so the two use identical canonical paths. It
// rejects an empty path, an absolute path, or any ".." segment -- the
// client-side traversal guard, defense in depth alongside the hub's safeJoin
// -- then canonicalizes with path.Clean (collapsing "./" prefixes and
// duplicate slashes) and re-verifies the cleaned result.
//
// Canonicalizing client-side matters in hub mode: a non-canonical path like
// "./index.html" or "a//b" embedded in a URL draws a ServeMux 301 to the
// cleaned path, and Go's HTTP client follows a 301 as a body-less GET --
// which would silently degrade a PUT/PATCH into a read that "succeeds". The
// hub client's CheckRedirect refuses redirects outright for the same reason
// (see newHubBackend), so any future non-canonical case fails loudly.
func cleanRelPath(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("file path is required")
	}
	slashed := filepath.ToSlash(name)
	if filepath.IsAbs(name) || strings.HasPrefix(slashed, "/") {
		return "", fmt.Errorf("invalid file path %q: absolute paths are not allowed", name)
	}
	for _, seg := range strings.Split(slashed, "/") {
		if seg == ".." {
			return "", fmt.Errorf("invalid file path %q: must not contain %q", name, "..")
		}
	}
	cleaned := path.Clean(slashed)
	// Re-verify after Clean: a path that collapses to the root itself (".",
	// or all "./" segments) names no file.
	if cleaned == "." || cleaned == "/" || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("invalid file path %q", name)
	}
	return cleaned, nil
}
