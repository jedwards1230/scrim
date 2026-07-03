package pushclient

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestWatchFiresOnFileChange(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls int32
	done := make(chan struct{})
	go func() {
		_ = Watch(ctx, dir, func() {
			if atomic.AddInt32(&calls, 1) == 1 {
				close(done)
			}
		})
	}()

	// Give the watcher a moment to register before triggering a change.
	time.Sleep(100 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case <-done:
		// onChange fired at least once.
	case <-time.After(5 * time.Second):
		t.Fatal("Watch() did not call onChange within 5s of a file change")
	}
}

func TestWatchAddsNewSubdirectories(t *testing.T) {
	dir := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls int32
	done := make(chan struct{})
	go func() {
		_ = Watch(ctx, dir, func() {
			if atomic.AddInt32(&calls, 1) == 1 {
				close(done)
			}
		})
	}()

	time.Sleep(100 * time.Millisecond)
	sub := filepath.Join(dir, "assets")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// Give the watcher a moment to pick up the new subdirectory before
	// writing into it.
	time.Sleep(200 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(sub, "app.js"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case <-done:
		// onChange fired for a change under the newly-created subdirectory.
	case <-time.After(5 * time.Second):
		t.Fatal("Watch() did not call onChange for a file created under a new subdirectory within 5s")
	}
}

func TestWatchStopsOnContextCancel(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- Watch(ctx, dir, func() {})
	}()

	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Watch() error = %v, want nil after context cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Watch() did not return within 5s of context cancellation")
	}
}
