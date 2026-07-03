package server

import "time"

const (
	minReapInterval = 250 * time.Millisecond
	maxReapInterval = 5 * time.Second
)

// shouldReap reports whether the daemon should exit given the current time,
// the last recorded activity time, the configured idle timeout, and the
// number of currently-connected SSE clients.
//
// A connected SSE client counts as "in use" for the client's entire
// lifetime — independent of how stale lastActivity is — so the daemon never
// reaps out from under an open live-reload connection.
func shouldReap(now, lastActivity time.Time, idleTimeout time.Duration, sseClients int) bool {
	if sseClients > 0 {
		return false
	}
	if idleTimeout <= 0 {
		return false
	}
	return !now.Before(lastActivity.Add(idleTimeout))
}

// reapCheckInterval returns how often the reaper should poll, scaled to the
// configured idle timeout (idleTimeout/10, clamped) so a short idle timeout
// (e.g. the few-second values used in e2e tests) is still enforced
// promptly, without busy-looping for the long defaults used in practice.
func reapCheckInterval(idleTimeout time.Duration) time.Duration {
	interval := idleTimeout / 10
	if interval < minReapInterval {
		return minReapInterval
	}
	if interval > maxReapInterval {
		return maxReapInterval
	}
	return interval
}
