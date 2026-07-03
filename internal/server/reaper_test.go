package server

import (
	"testing"
	"time"
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
