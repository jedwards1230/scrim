package server

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/jedwards1230/scrim/internal/canvas"
)

// defaultDebounce coalesces bursts of filesystem events (editors/agents
// often produce write+rename bursts) into a single reload push.
const defaultDebounce = 200 * time.Millisecond

// canvasWatcher watches the canvases directory tree (recursively, since
// fsnotify itself is not recursive) and, per canvas ID, debounces bursts of
// changes into a single onReload call.
type canvasWatcher struct {
	fsw      *fsnotify.Watcher
	root     string
	debounce time.Duration
	onReload func(canvasID string)

	mu     sync.Mutex
	timers map[string]*time.Timer

	done chan struct{}
	wg   sync.WaitGroup
}

// newCanvasWatcher creates a watcher rooted at root (created if missing) and
// starts its event loop in the background.
func newCanvasWatcher(root string, debounce time.Duration, onReload func(string)) (*canvasWatcher, error) {
	if err := os.MkdirAll(root, 0o755); err != nil { //nolint:gosec // canvases dir is a user-owned working directory
		return nil, fmt.Errorf("creating canvases dir: %w", err)
	}
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("creating fsnotify watcher: %w", err)
	}

	w := &canvasWatcher{
		fsw:      fsw,
		root:     root,
		debounce: debounce,
		onReload: onReload,
		timers:   make(map[string]*time.Timer),
		done:     make(chan struct{}),
	}
	if err := w.addTree(root); err != nil {
		_ = fsw.Close()
		return nil, err
	}

	w.wg.Add(1)
	go w.loop()
	return w, nil
}

// addTree registers a watch on dir and every subdirectory beneath it.
// Unreadable subdirectories are skipped rather than failing the whole walk.
func (w *canvasWatcher) addTree(dir string) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // best-effort recursive watch registration
		}
		if d.IsDir() {
			_ = w.fsw.Add(path) // best-effort: permission errors shouldn't abort the walk
		}
		return nil
	})
}

func (w *canvasWatcher) loop() {
	defer w.wg.Done()
	for {
		select {
		case ev, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			w.handleEvent(ev)
		case _, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			// Best-effort: fsnotify errors (e.g. a watch on a since-deleted
			// path) don't stop the watcher.
		case <-w.done:
			return
		}
	}
}

func (w *canvasWatcher) handleEvent(ev fsnotify.Event) {
	if ev.Op&fsnotify.Create != 0 {
		if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
			_ = w.addTree(ev.Name) // new subdirectory: start watching it too
		}
	}
	id := canvasIDFromPath(w.root, ev.Name)
	if id == "" {
		return
	}
	w.scheduleReload(id)
}

// scheduleReload (re)starts a per-canvas debounce timer, so a burst of
// events for the same canvas within the debounce window collapses into one
// onReload call.
func (w *canvasWatcher) scheduleReload(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if t, ok := w.timers[id]; ok {
		t.Reset(w.debounce)
		return
	}
	w.timers[id] = time.AfterFunc(w.debounce, func() {
		w.mu.Lock()
		delete(w.timers, id)
		w.mu.Unlock()
		w.onReload(id)
	})
}

// Close stops the watcher and waits for its event loop to exit.
func (w *canvasWatcher) Close() error {
	close(w.done)
	w.mu.Lock()
	for _, t := range w.timers {
		t.Stop()
	}
	w.mu.Unlock()
	err := w.fsw.Close()
	w.wg.Wait()
	return err
}

// canvasIDFromPath returns the canvas ID that owns path (the first path
// component under root), or "" if path doesn't resolve to a valid canvas ID
// (e.g. it's outside root, or is root itself).
func canvasIDFromPath(root, eventPath string) string {
	rel, err := filepath.Rel(root, eventPath)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return ""
	}
	first, _, _ := strings.Cut(rel, string(os.PathSeparator))
	if canvas.ValidateID(first) != nil {
		return ""
	}
	return first
}
