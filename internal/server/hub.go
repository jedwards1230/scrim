package server

import "sync"

// hub tracks connected SSE clients per canvas ID and broadcasts reload
// signals to them. It also backs the idle reaper's "any SSE client
// connected" liveness signal.
type hub struct {
	mu      sync.Mutex
	clients map[string]map[chan struct{}]struct{}
	// total is the running count of connected clients across every canvas
	// (the sum of the inner map lengths), maintained incrementally so the
	// global-cap check in register stays O(1) under a connection flood
	// rather than re-summing the map on every open.
	total int

	// maxGlobal and maxPerCanvas cap the number of concurrent SSE clients
	// (0 = unlimited). They are set only for a hub (see NewHub); newHub
	// leaves them 0, so the local daemon is uncapped and byte-identical to
	// its prior behavior.
	maxGlobal    int
	maxPerCanvas int

	closeOnce sync.Once
	closed    chan struct{}
}

func newHub() *hub {
	return &hub{
		clients: make(map[string]map[chan struct{}]struct{}),
		closed:  make(chan struct{}),
	}
}

// register adds a new client for canvas id and returns its notification
// channel plus an unregister func the caller must call (typically deferred)
// when the connection closes. ok is false (with nil ch/unregister) when a
// configured cap is already at its ceiling -- the caller must reject the
// connection (503) and MUST NOT call unregister in that case. Both caps are
// checked under the same lock as the insert, so the count can't race past
// the ceiling between the check and the add.
func (h *hub) register(id string) (ch chan struct{}, unregister func(), ok bool) {
	h.mu.Lock()
	if h.maxGlobal > 0 && h.total >= h.maxGlobal {
		h.mu.Unlock()
		return nil, nil, false
	}
	if h.maxPerCanvas > 0 && len(h.clients[id]) >= h.maxPerCanvas {
		h.mu.Unlock()
		return nil, nil, false
	}
	ch = make(chan struct{}, 1)
	if h.clients[id] == nil {
		h.clients[id] = make(map[chan struct{}]struct{})
	}
	h.clients[id][ch] = struct{}{}
	h.total++
	h.mu.Unlock()

	return ch, func() {
		h.mu.Lock()
		delete(h.clients[id], ch)
		if len(h.clients[id]) == 0 {
			delete(h.clients, id)
		}
		h.total--
		h.mu.Unlock()
	}, true
}

// broadcast notifies every currently-connected client of canvas id. It never
// blocks: a client whose channel is already full (a reload it hasn't
// consumed yet) just gets its pending reload coalesced.
func (h *hub) broadcast(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients[id] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// clientCount returns the total number of connected SSE clients across all
// canvases. It reads the incrementally-maintained total (identical to summing
// the per-canvas maps, which it replaced), so the reaper's "any SSE client
// connected" liveness check is unchanged: a non-zero count still means at
// least one live connection.
func (h *hub) clientCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.total
}

// canvasClientCount returns the number of connected SSE clients for one
// canvas.
func (h *hub) canvasClientCount(id string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients[id])
}

// closeAll signals every currently- and future-registered SSE handler to
// return promptly, so a graceful http.Server.Shutdown isn't left waiting on
// a client that keeps its connection open indefinitely (e.g. a browser tab
// left open on a canvas). It closes the single shared h.closed channel
// (returned by done), not each client's own per-connection reload channel --
// every SSE handler already selects on h.closed alongside its own reload
// channel and request context, so closing the one shared channel is
// sufficient to unblock all of them at once. Safe to call more than once and
// concurrently with register/unregister; handlers already selecting on done
// pick it up immediately, and any handler that calls register afterward
// finds done already closed.
func (h *hub) closeAll() {
	h.closeOnce.Do(func() { close(h.closed) })
}

// done returns the channel that's closed by closeAll. SSE handlers select
// on it alongside the request context and their own reload channel, so a
// shutdown unblocks them the same way a client disconnect would.
func (h *hub) done() <-chan struct{} {
	return h.closed
}
