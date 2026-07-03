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
