package mcpserver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jedwards1230/scrim/internal/canvas"
	"github.com/jedwards1230/scrim/internal/config"
	"github.com/jedwards1230/scrim/internal/dircopy"
	"github.com/jedwards1230/scrim/internal/fileedit"
	"github.com/jedwards1230/scrim/internal/snapshot"
)

// localBackend drives the local scrim daemon and the on-disk canvas directory,
// using exactly the primitives the daemon-backed CLI verbs use
// (daemon/apiclient for the control surface, canvas/snapshot for filesystem
// operations). It is the default backend when `scrim mcp` runs without --hub.
type localBackend struct {
	cfg config.Config
}

func newLocalBackend(cfg config.Config) *localBackend { return &localBackend{cfg: cfg} }

func (b *localBackend) List(ctx context.Context) ([]CanvasInfo, error) {
	client, _, _, err := resolveDaemon(b.cfg, true)
	if err != nil {
		return nil, err
	}
	canvases, err := client.ListCanvases(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]CanvasInfo, 0, len(canvases))
	for _, c := range canvases {
		out = append(out, CanvasInfo{
			ID:         c.ID,
			Title:      c.Title,
			URL:        c.URL,
			Dir:        c.Dir,
			Icon:       c.Icon,
			Color:      c.Color,
			ModifiedAt: c.ModifiedAt,
			SSEClients: c.SSEClients,
		})
	}
	return out, nil
}

func (b *localBackend) Add(ctx context.Context, id, title, description, icon string) (CanvasInfo, error) {
	client, _, _, err := resolveDaemon(b.cfg, true)
	if err != nil {
		return CanvasInfo{}, err
	}
	info, err := client.CreateCanvas(ctx, id, title, description, icon)
	if err != nil {
		return CanvasInfo{}, err
	}
	return CanvasInfo{ID: info.ID, Title: info.Title, URL: info.URL, Dir: info.Dir, Icon: info.Icon, Color: info.Color}, nil
}

func (b *localBackend) Remove(ctx context.Context, id string) error {
	// rm never self-starts: delete via the daemon when one is already healthy,
	// otherwise straight off disk (mirrors cli.cmdRm).
	client, _, running, err := resolveDaemon(b.cfg, false)
	if err != nil {
		return err
	}
	if running {
		return client.DeleteCanvas(ctx, id)
	}
	return canvas.Delete(b.cfg.CanvasesDir(), b.cfg.MetaDir(), id)
}

func (b *localBackend) Status(ctx context.Context) (StatusInfo, error) {
	// status is the daemon health-check; it must never self-start one.
	client, _, running, err := resolveDaemon(b.cfg, false)
	if err != nil {
		return StatusInfo{}, err
	}
	if !running {
		return StatusInfo{Running: false}, nil
	}
	resp, err := client.Status(ctx)
	if err != nil {
		return StatusInfo{}, err
	}
	return StatusInfo{
		Running:            true,
		PID:                resp.PID,
		Host:               resp.Host,
		Port:               resp.Port,
		Version:            resp.Version,
		UptimeSeconds:      resp.UptimeSeconds,
		CanvasCount:        resp.CanvasCount,
		SSEClients:         resp.SSEClients,
		IdleSeconds:        resp.IdleSeconds,
		IdleTimeoutSeconds: resp.IdleTimeoutSeconds,
	}, nil
}

func (b *localBackend) Link(ctx context.Context, id string) ([]string, error) {
	client, st, _, err := resolveDaemon(b.cfg, true)
	if err != nil {
		return nil, err
	}
	if id == "" {
		return []string{dashboardURL(st)}, nil
	}
	canvases, err := client.ListCanvases(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range canvases {
		if c.ID == id {
			// c.URL already carries the ?t=<token> query when auth is enabled
			// (the daemon bakes it in server-side).
			return []string{c.URL}, nil
		}
	}
	return nil, fmt.Errorf("canvas %q not found", id)
}

func (b *localBackend) Snap(_ context.Context, id, label string) (SnapInfo, error) {
	entry, err := snapshot.Create(canvas.Dir(b.cfg.CanvasesDir(), id), b.cfg.VersionsDir(), id, label)
	if err != nil {
		return SnapInfo{}, err
	}
	return SnapInfo{Name: entry.Name, Dir: entry.Dir, Timestamp: entry.Timestamp, Label: entry.Label}, nil
}

func (b *localBackend) Snaps(_ context.Context, id string) ([]SnapInfo, error) {
	entries, err := snapshot.List(b.cfg.VersionsDir(), id)
	if err != nil {
		return nil, err
	}
	// snapshot.List returns oldest-first; present newest-first, matching
	// cli.cmdSnaps and the hub machine API.
	out := make([]SnapInfo, 0, len(entries))
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		out = append(out, SnapInfo{Name: e.Name, Dir: e.Dir, Timestamp: e.Timestamp, Label: e.Label})
	}
	return out, nil
}

func (b *localBackend) Revert(_ context.Context, id, name string) (RevertInfo, error) {
	// snapshot.RevertWithSafety is the exact protocol cli.cmdRevert runs:
	// resolve (and verify) the target BEFORE taking the prerevert safety
	// snapshot, so a bare revert doesn't restore the canvas to its own current
	// state and a typo'd name leaves no spurious prerevert behind.
	entry, err := snapshot.RevertWithSafety(canvas.Dir(b.cfg.CanvasesDir(), id), b.cfg.VersionsDir(), id, name)
	if err != nil {
		return RevertInfo{}, err
	}
	return RevertInfo{Reverted: id, Snapshot: entry.Name}, nil
}

func (b *localBackend) ListFiles(_ context.Context, id string) ([]FileEntry, error) {
	// canvas.Files validates id, requires the directory to exist, and returns
	// the same shape the hub route serves -- so local and hub list identically.
	metas, err := canvas.Files(b.cfg.CanvasesDir(), id)
	if err != nil {
		return nil, err
	}
	out := make([]FileEntry, 0, len(metas))
	for _, m := range metas {
		out = append(out, FileEntry{Path: m.Path, Size: m.Size, ModifiedAt: m.ModifiedAt})
	}
	return out, nil
}

func (b *localBackend) ReadFile(_ context.Context, id, path string) ([]byte, error) {
	// cleanRelPath first, so local and hub mode accept and canonicalize the
	// exact same path shapes (e.g. "./x.html", "a//b").
	path, err := cleanRelPath(path)
	if err != nil {
		return nil, err
	}
	root := canvas.Dir(b.cfg.CanvasesDir(), id)
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("canvas %q not found", id)
	}
	target, err := safeJoinLocal(root, path)
	if err != nil {
		return nil, err
	}
	// Mirror the write cap on the read side: read_file returns inline content
	// to an MCP client, so an oversize file is an error, not a full buffer.
	if fi, err := os.Stat(target); err == nil && fi.Size() > maxFileBytes {
		return nil, fmt.Errorf("file %q is %d bytes, over the %d-byte read_file cap", path, fi.Size(), maxFileBytes)
	}
	data, err := os.ReadFile(target) //nolint:gosec // target is validated by safeJoinLocal to stay within the canvas root
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("file %q not found in canvas %q", path, id)
		}
		return nil, err
	}
	return data, nil
}

func (b *localBackend) WriteFile(_ context.Context, id, path string, content []byte) error {
	path, err := cleanRelPath(path)
	if err != nil {
		return err
	}
	if len(content) > maxFileBytes {
		return fmt.Errorf("file exceeds the %d-byte (2 MiB) per-file limit", maxFileBytes)
	}
	root := canvas.Dir(b.cfg.CanvasesDir(), id)
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return fmt.Errorf("canvas %q not found (add it before writing files)", id)
	}
	target, err := safeJoinLocal(root, path)
	if err != nil {
		return err
	}
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // dir lives under the canvas working directory, within-root per safeJoinLocal
		return fmt.Errorf("creating parent directory: %w", err)
	}
	return atomicWriteFileLocal(target, dir, content)
}

func (b *localBackend) EditFile(_ context.Context, id, path, oldStr, newStr string, replaceAll bool) (EditInfo, error) {
	return b.editFile(id, path, func(data []byte) ([]byte, int, error) {
		return fileedit.Apply(data, oldStr, newStr, replaceAll, maxFileBytes)
	})
}

func (b *localBackend) EditFileBatch(_ context.Context, id, path string, edits []fileedit.Edit) (EditInfo, error) {
	return b.editFile(id, path, func(data []byte) ([]byte, int, error) {
		return fileedit.ApplyBatch(data, edits, maxFileBytes)
	})
}

// editFile is the shared read-modify-write path behind both EditFile (single)
// and EditFileBatch: it resolves+caps+reads the file, applies the caller's
// transform (fileedit.Apply or fileedit.ApplyBatch), and atomically writes the
// result back. Keeping the filesystem plumbing in one place means the single
// and batch paths can only differ in which fileedit function runs.
func (b *localBackend) editFile(id, path string, apply func([]byte) ([]byte, int, error)) (EditInfo, error) {
	path, err := cleanRelPath(path)
	if err != nil {
		return EditInfo{}, err
	}
	root := canvas.Dir(b.cfg.CanvasesDir(), id)
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return EditInfo{}, fmt.Errorf("canvas %q not found", id)
	}
	target, err := safeJoinLocal(root, path)
	if err != nil {
		return EditInfo{}, err
	}
	// Mirror ReadFile's cap on the source: an edit reads the whole file into
	// memory first, so an oversize file is an error, not a full buffer.
	if fi, err := os.Stat(target); err == nil && fi.Size() > maxFileBytes {
		return EditInfo{}, fmt.Errorf("file %q is %d bytes, over the %d-byte edit_file cap", path, fi.Size(), maxFileBytes)
	}
	data, err := os.ReadFile(target) //nolint:gosec // target is validated by safeJoinLocal to stay within the canvas root
	if err != nil {
		if os.IsNotExist(err) {
			return EditInfo{}, fmt.Errorf("file %q not found in canvas %q", path, id)
		}
		return EditInfo{}, err
	}
	edited, replacements, err := apply(data)
	if err != nil {
		return EditInfo{}, err
	}
	// The file exists, so its parent directory does too -- no MkdirAll. The
	// same atomic temp+rename path WriteFile uses keeps the daemon's fsnotify
	// watcher seeing one clean event per edit.
	if err := atomicWriteFileLocal(target, filepath.Dir(target), edited); err != nil {
		return EditInfo{}, err
	}
	return EditInfo{Path: path, Replacements: replacements}, nil
}

func (b *localBackend) CopyCanvas(_ context.Context, from, to string, overwrite bool) (CopyInfo, error) {
	if err := canvas.ValidateID(from); err != nil {
		return CopyInfo{}, err
	}
	if err := canvas.ValidateID(to); err != nil {
		return CopyInfo{}, err
	}
	if from == to {
		return CopyInfo{}, fmt.Errorf("source and target are the same canvas")
	}

	canvasesDir := b.cfg.CanvasesDir()
	sourceDir := canvas.Dir(canvasesDir, from)
	if fi, err := os.Stat(sourceDir); err != nil || !fi.IsDir() {
		return CopyInfo{}, fmt.Errorf("canvas %q not found", from)
	}
	targetDir := canvas.Dir(canvasesDir, to)
	targetExists := false
	if fi, err := os.Stat(targetDir); err == nil && fi.IsDir() {
		targetExists = true
	}
	if targetExists && !overwrite {
		return CopyInfo{}, fmt.Errorf("target canvas %q already exists (set overwrite to replace it)", to)
	}

	// Stage the copy under the data dir but OUTSIDE canvasesDir (so the
	// daemon's watcher never fires on individual staged writes), then swap it
	// into place with an atomic rename -- a copy is pure filesystem work, so
	// like snap/revert it never self-starts the daemon.
	if err := os.MkdirAll(b.cfg.Dir, 0o755); err != nil { //nolint:gosec // data dir is user-owned working state
		return CopyInfo{}, fmt.Errorf("preparing staging area: %w", err)
	}
	staging, err := os.MkdirTemp(b.cfg.Dir, ".scrim-copy-*")
	if err != nil {
		return CopyInfo{}, fmt.Errorf("creating staging directory: %w", err)
	}
	stagingOwned := true
	defer func() {
		if stagingOwned {
			_ = os.RemoveAll(staging)
		}
	}()

	if err := dircopy.Copy(sourceDir, staging, copyMaxBytes, copyMaxEntries); err != nil {
		return CopyInfo{}, fmt.Errorf("copying canvas: %w", err)
	}
	if err := os.MkdirAll(canvasesDir, 0o755); err != nil { //nolint:gosec // canvases dir is user-owned working state
		return CopyInfo{}, fmt.Errorf("preparing canvases directory: %w", err)
	}
	if targetExists {
		if _, err := snapshot.Create(targetDir, b.cfg.VersionsDir(), to, "precopy"); err != nil {
			return CopyInfo{}, fmt.Errorf("snapshotting target before overwrite: %w", err)
		}
	}

	// Move the existing target aside UNDER the data dir (not under canvasesDir),
	// so a running daemon watching canvasesDir never sees the aside dir appear
	// and vanish -- mirroring the hub handler's use of the staging root.
	var aside string
	if targetExists {
		tmp, err := os.MkdirTemp(b.cfg.Dir, "scrim-copy-old-*")
		if err != nil {
			return CopyInfo{}, fmt.Errorf("preparing canvas swap: %w", err)
		}
		if err := os.RemoveAll(tmp); err != nil {
			return CopyInfo{}, fmt.Errorf("preparing canvas swap: %w", err)
		}
		if err := os.Rename(targetDir, tmp); err != nil {
			return CopyInfo{}, fmt.Errorf("moving previous canvas aside: %w", err)
		}
		aside = tmp
	}
	if err := os.Rename(staging, targetDir); err != nil {
		if aside != "" {
			_ = os.Rename(aside, targetDir) // best-effort restore
		}
		return CopyInfo{}, fmt.Errorf("swapping copied canvas into place: %w", err)
	}
	stagingOwned = false
	if aside != "" {
		_ = os.RemoveAll(aside)
	}
	// The content swap is now committed. Metadata is non-critical and best-
	// effort by comparison: a CopyMeta failure surfaces as an error even though
	// the copied files are already in place (on overwrite, the old target was
	// snapshotted first, so nothing is lost).
	if err := canvas.CopyMeta(b.cfg.MetaDir(), from, to); err != nil {
		return CopyInfo{}, err
	}

	return CopyInfo{From: from, To: to, URL: b.canvasViewURL(to)}, nil
}

// canvasViewURL returns the best-effort local view URL for canvas id: the
// token-qualified /c/<id>/ URL when a daemon is already healthy, or "" when
// none is running. It never self-starts one -- copy stays a pure filesystem
// operation -- so the URL is a convenience, not a guarantee.
func (b *localBackend) canvasViewURL(id string) string {
	_, st, running, err := resolveDaemon(b.cfg, false)
	if err != nil || !running {
		return ""
	}
	url := st.BaseURL() + "/c/" + id + "/"
	if st.AuthEnabled() {
		url += "?t=" + st.Token
	}
	return url
}

// atomicWriteFileLocal writes content to a temp file in dir and renames it over
// target (same-dir rename is atomic, so the daemon's fsnotify watcher observes
// one clean event and broadcasts a single SSE reload). The temp file is
// cleaned up on any error path. It mirrors server.atomicWriteFile without
// importing internal/server.
func atomicWriteFileLocal(target, dir string, content []byte) (err error) {
	tmp, err := os.CreateTemp(dir, ".scrim-mcp-write-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err = tmp.Write(content); err != nil {
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
