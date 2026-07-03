package server

import (
	"context"
	"testing"
	"time"

	"github.com/jedwards1230/scrim/internal/config"
)

func TestShouldReap(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name        string
		now         time.Time
		lastActive  time.Time
		idleTimeout time.Duration
		sseClients  int
		want        bool
	}{
		{
			name:        "well within idle timeout",
			now:         base.Add(10 * time.Second),
			lastActive:  base,
			idleTimeout: time.Minute,
			sseClients:  0,
			want:        false,
		},
		{
			name:        "exactly at idle timeout reaps",
			now:         base.Add(time.Minute),
			lastActive:  base,
			idleTimeout: time.Minute,
			sseClients:  0,
			want:        true,
		},
		{
			name:        "past idle timeout reaps",
			now:         base.Add(2 * time.Minute),
			lastActive:  base,
			idleTimeout: time.Minute,
			sseClients:  0,
			want:        true,
		},
		{
			name:        "connected SSE client blocks reap despite stale activity",
			now:         base.Add(time.Hour),
			lastActive:  base,
			idleTimeout: time.Minute,
			sseClients:  1,
			want:        false,
		},
		{
			name:        "multiple SSE clients still blocks reap",
			now:         base.Add(time.Hour),
			lastActive:  base,
			idleTimeout: time.Minute,
			sseClients:  5,
			want:        false,
		},
		{
			name:        "zero idle timeout never reaps",
			now:         base.Add(24 * time.Hour),
			lastActive:  base,
			idleTimeout: 0,
			sseClients:  0,
			want:        false,
		},
		{
			name:        "negative idle timeout never reaps",
			now:         base.Add(24 * time.Hour),
			lastActive:  base,
			idleTimeout: -time.Second,
			sseClients:  0,
			want:        false,
		},
		{
			name:        "activity resets the clock",
			now:         base.Add(90 * time.Second),
			lastActive:  base.Add(89 * time.Second),
			idleTimeout: time.Minute,
			sseClients:  0,
			want:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldReap(tt.now, tt.lastActive, tt.idleTimeout, tt.sseClients)
			if got != tt.want {
				t.Errorf("shouldReap(now=%v, last=%v, timeout=%v, clients=%d) = %v, want %v",
					tt.now, tt.lastActive, tt.idleTimeout, tt.sseClients, got, tt.want)
			}
		})
	}
}

func TestReapCheckInterval(t *testing.T) {
	tests := []struct {
		name        string
		idleTimeout time.Duration
		want        time.Duration
	}{
		{"very short timeout clamps to min", 1 * time.Second, minReapInterval},
		{"e2e-style 3s timeout scales to a tenth", 3 * time.Second, 300 * time.Millisecond},
		{"moderate timeout scales to a tenth", 30 * time.Second, 3 * time.Second},
		{"long default timeout clamps to max", 30 * time.Minute, maxReapInterval},
		{"zero timeout clamps to min", 0, minReapInterval},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reapCheckInterval(tt.idleTimeout); got != tt.want {
				t.Errorf("reapCheckInterval(%v) = %v, want %v", tt.idleTimeout, got, tt.want)
			}
		})
	}
}

// TestReapReturnsImmediatelyWhenDisabled asserts that reap doesn't start a
// ticker at all when idle reaping is disabled (idleTimeout <= 0) — it must
// return right away instead of waking up every reapCheckInterval forever to
// find shouldReap always false.
func TestReapReturnsImmediatelyWhenDisabled(t *testing.T) {
	tests := []struct {
		name        string
		idleTimeout time.Duration
	}{
		{"zero idle timeout", 0},
		{"negative idle timeout", -time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(config.Config{
				Dir:         t.TempDir(),
				Host:        "127.0.0.1",
				Port:        0,
				IdleTimeout: tt.idleTimeout,
			})

			done := make(chan struct{})
			go func() {
				s.reap(t.Context())
				close(done)
			}()

			select {
			case <-done:
				// Returned promptly without ticking — expected.
			case <-time.After(minReapInterval * 2):
				t.Fatalf("reap() did not return promptly with idleTimeout=%v; it should skip the ticker entirely", tt.idleTimeout)
			}
		})
	}
}

// TestReapDoesNotFireBeforeContextCancelWhenEnabled is a control case: with
// reaping enabled but no activity ever going stale within the test window,
// reap must still be running (blocked in its select) until ctx is canceled,
// confirming the disabled-path early return above is specific to
// idleTimeout <= 0 and not a general "reap always returns fast" regression.
func TestReapDoesNotFireBeforeContextCancelWhenEnabled(t *testing.T) {
	s := New(config.Config{
		Dir:         t.TempDir(),
		Host:        "127.0.0.1",
		Port:        0,
		IdleTimeout: time.Hour,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.reap(ctx)
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("reap() returned before ctx was canceled or the daemon went idle")
	case <-time.After(minReapInterval * 2):
		// Still running as expected.
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reap() did not return after ctx was canceled")
	}
}
