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
	// fireWG tracks in-flight debounce callbacks (the goroutine
	// time.AfterFunc spawns for each timer), so Close is a true quiescence
	// barrier: a callback that had already fired (and so couldn't be
	// canceled by Timer.Stop) is waited out before Close returns, instead
	// of being left to run after the caller thinks Close has finished.
	fireWG sync.WaitGroup

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
//
// Map presence alone can't tell us whether an existing entry's timer is
// still safely resettable: a timer's AfterFunc callback goroutine can start
// running (committing to call w.onReload exactly once) before that
// goroutine gets as far as acquiring w.mu to delete itself from w.timers.
// If scheduleReload ran in that window and just called t.Reset on the
// stale entry, per time.Timer's documented AfterFunc semantics that does
// NOT reschedule the in-flight callback -- it arms a *second*, independent
// run of the same closure, which only has one `defer w.fireWG.Done()` for
// the one `fireWG.Add(1)` already spent, driving fireWG negative on the
// second call (the reported crash).
//
// t.Stop() is the one operation that authoritatively answers "is this
// timer still provably pending" -- it inspects the runtime timer's fire
// state directly, independent of whether the spawned callback goroutine
// has been scheduled to run yet. Stop() == true means the callback was
// never dispatched and never will be for this timer generation, so Reset
// is safe. Stop() == false means it already fired (or is in the process
// of firing) and must never be Reset -- instead this treats the id as if
// there were no existing timer at all and starts a fresh debounce cycle,
// exactly per the task's required invariant.
func (w *canvasWatcher) scheduleReload(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if t, ok := w.timers[id]; ok {
		if t.Stop() {
			// Provably still pending: the same single invocation is just
			// being pushed back, not duplicated.
			t.Reset(w.debounce)
			return
		}
		// Already firing/fired: fall through and replace this id's entry
		// with a brand new timer + its own fireWG credit below. The old
		// callback's own cleanup (see the identity check below) will
		// no-op once it sees w.timers[id] no longer points at it, instead
		// of clobbering the fresh entry we're about to install.
	}
	w.fireWG.Add(1)
	var t *time.Timer
	t = time.AfterFunc(w.debounce, func() {
		defer w.fireWG.Done()
		w.mu.Lock()
		// Only delete this id's entry if it still refers to this exact
		// timer -- a concurrent scheduleReload may have already replaced
		// it (see above) after Stop() reported this one as already fired.
		if w.timers[id] == t {
			delete(w.timers, id)
		}
		w.mu.Unlock()
		w.onReload(id)
	})
	w.timers[id] = t
}

// Close stops the watcher and waits for its event loop to exit, as well as
// for any debounce callback that was already firing at the time of the call
// to finish (see fireWG).
//
// Ordering matters here: loop() must be provably finished before this
// touches fireWG at all. close(w.done) alone doesn't guarantee that --
// loop()'s select is also watching w.fsw.Events/Errors, and select can pick
// a ready Events case over the done case, calling handleEvent ->
// scheduleReload -> fireWG.Add(1) for a not-yet-seen canvas ID. If that
// raced with fireWG.Wait() below (before fsw.Close()/wg.Wait() had run),
// it would be an Add concurrent with Wait -- sync.WaitGroup explicitly
// documents that as a panic risk. Closing fsw (which closes its Events/
// Errors channels and makes loop()'s select take the !ok branches) and then
// joining wg makes loop()'s exit a hard fact before fireWG is ever touched.
func (w *canvasWatcher) Close() error {
	close(w.done)
	err := w.fsw.Close() // closes Events/Errors: unblocks loop()'s select
	w.wg.Wait()          // loop() has now returned; no more scheduleReload calls possible

	w.mu.Lock()
	for _, t := range w.timers {
		if t.Stop() {
			// Successfully canceled before it fired: its callback (and the
			// fireWG.Add(1) that goes with it) will never run.
			w.fireWG.Done()
		}
		// If Stop returns false, the callback already fired (or is in the
		// process of firing) and will call fireWG.Done() itself; waiting
		// below still catches it since it blocks on w.mu, released once
		// this loop finishes.
	}
	w.mu.Unlock()
	w.fireWG.Wait()
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
