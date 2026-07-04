package pushclient

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// debounceInterval coalesces a burst of filesystem events (an editor/agent
// commonly produces write+rename bursts) into a single onChange call.
const debounceInterval = 200 * time.Millisecond

// Watch watches dir recursively (fsnotify itself is not recursive, so new
// subdirectories created after Watch starts are added automatically on
// their own Create event) and calls onChange once per debounce window --
// after 200ms of quiescence following the last observed change -- until ctx
// is canceled.
//
// This is deliberately a self-contained implementation, not a reuse of
// internal/server's canvasWatcher: that type is keyed per-canvas-ID against
// a shared canvases root, which doesn't fit watching a single arbitrary
// local directory, and pushclient must not import internal/server at all
// (it would pull the daemon's HTTP engine into a client-only CLI verb).
func Watch(ctx context.Context, dir string, onChange func()) error {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("creating fsnotify watcher: %w", err)
	}
	defer func() { _ = fsw.Close() }()

	if err := addWatchTree(fsw, dir); err != nil {
		return err
	}

	fire := make(chan struct{}, 1)
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-fsw.Events:
			if !ok {
				return nil
			}
			if ev.Op&fsnotify.Create != 0 {
				if fi, statErr := os.Stat(ev.Name); statErr == nil && fi.IsDir() {
					_ = addWatchTree(fsw, ev.Name) // best-effort: watch the new subtree too
				}
			}
			if timer == nil {
				timer = time.AfterFunc(debounceInterval, func() {
					select {
					case fire <- struct{}{}:
					default:
					}
				})
			} else {
				timer.Reset(debounceInterval)
			}
		case _, ok := <-fsw.Errors:
			if !ok {
				return nil
			}
			// Best-effort: an fsnotify error (e.g. a watch on a
			// since-deleted path) doesn't stop the watch loop.
		case <-fire:
			onChange()
		}
	}
}

// addWatchTree registers a watch on dir and every subdirectory beneath it.
// Unreadable subdirectories are skipped rather than failing the whole walk.
func addWatchTree(fsw *fsnotify.Watcher, dir string) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // best-effort recursive watch registration
		}
		if d.IsDir() {
			_ = fsw.Add(path) // best-effort: permission errors shouldn't abort the walk
		}
		return nil
	})
}
