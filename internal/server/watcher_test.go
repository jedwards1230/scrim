package server

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestCanvasIDFromPath(t *testing.T) {
	root := filepath.FromSlash("/canvases")

	tests := []struct {
		name string
		path string
		want string
	}{
		{"top-level file", filepath.Join(root, "report", "index.html"), "report"},
		{"nested file", filepath.Join(root, "report", "assets", "style.css"), "report"},
		{"root itself", root, ""},
		{"outside root", filepath.FromSlash("/somewhere/else"), ""},
		{"invalid id component", filepath.Join(root, ".hidden", "x"), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := canvasIDFromPath(root, tt.path); got != tt.want {
				t.Errorf("canvasIDFromPath(%q, %q) = %q, want %q", root, tt.path, got, tt.want)
			}
		})
	}
}

// recordingReloader records onReload calls with timestamps for asserting
// debounce/coalescing behavior.
type recordingReloader struct {
	mu    sync.Mutex
	calls []string
}

func (r *recordingReloader) record(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, id)
}

func (r *recordingReloader) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func TestScheduleReloadDebounce(t *testing.T) {
	const debounce = 30 * time.Millisecond
	rec := &recordingReloader{}
	w := &canvasWatcher{
		debounce: debounce,
		onReload: rec.record,
		timers:   make(map[string]*time.Timer),
	}

	// A burst of events within the debounce window should coalesce into a
	// single reload.
	for i := 0; i < 5; i++ {
		w.scheduleReload("report")
		time.Sleep(debounce / 4)
	}
	waitForCount(t, rec, 1, debounce*3)

	// A second, independent burst after the first fired should produce a
	// second reload.
	w.scheduleReload("report")
	waitForCount(t, rec, 2, debounce*3)

	// A different canvas ID debounces independently.
	w.scheduleReload("other")
	waitForCount(t, rec, 3, debounce*3)
}

// TestCloseWaitsForInFlightDebounceCallback asserts Close is a true
// quiescence barrier: a debounce callback that has already fired (so
// Timer.Stop can't cancel it) must finish running before Close returns,
// rather than being left to run concurrently after the caller believes
// shutdown is complete.
func TestCloseWaitsForInFlightDebounceCallback(t *testing.T) {
	root := t.TempDir()
	started := make(chan struct{})
	release := make(chan struct{})

	w, err := newCanvasWatcher(root, time.Millisecond, func(string) {
		close(started)
		<-release
	})
	if err != nil {
		t.Fatalf("newCanvasWatcher() error = %v", err)
	}

	w.scheduleReload("report")
	<-started // the debounce fired; onReload is now blocked on <-release

	closeDone := make(chan struct{})
	go func() {
		_ = w.Close()
		close(closeDone)
	}()

	select {
	case <-closeDone:
		t.Fatal("Close() returned before the in-flight debounce callback finished")
	case <-time.After(50 * time.Millisecond):
		// Still blocked waiting on the callback, as expected.
	}

	close(release) // let the callback finish
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("Close() did not return after the in-flight callback finished")
	}
}

func waitForCount(t *testing.T, rec *recordingReloader, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if rec.count() >= want {
			if got := rec.count(); got != want {
				t.Fatalf("reload count = %d, want exactly %d", got, want)
			}
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for reload count = %d, got %d", want, rec.count())
}
