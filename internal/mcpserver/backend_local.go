package mcpserver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jedwards1230/scrim/internal/canvas"
	"github.com/jedwards1230/scrim/internal/config"
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
	edited, replacements, err := fileedit.Apply(data, oldStr, newStr, replaceAll, maxFileBytes)
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
