package server

import "sync"

// hub tracks connected SSE clients per canvas ID and broadcasts reload
// signals to them. It also backs the idle reaper's "any SSE client
// connected" liveness signal.
type hub struct {
	mu      sync.Mutex
	clients map[string]map[chan struct{}]struct{}

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
// when the connection closes.
func (h *hub) register(id string) (ch chan struct{}, unregister func()) {
	ch = make(chan struct{}, 1)
	h.mu.Lock()
	if h.clients[id] == nil {
		h.clients[id] = make(map[chan struct{}]struct{})
	}
	h.clients[id][ch] = struct{}{}
	h.mu.Unlock()

	return ch, func() {
		h.mu.Lock()
		delete(h.clients[id], ch)
		if len(h.clients[id]) == 0 {
			delete(h.clients, id)
		}
		h.mu.Unlock()
	}
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
// canvases.
func (h *hub) clientCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, m := range h.clients {
		n += len(m)
	}
	return n
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
// left open on a canvas). Safe to call more than once and concurrently with
// register/unregister; handlers already selecting on done pick it up
// immediately, and any handler that calls register afterward gets a channel
// that's already closed.
func (h *hub) closeAll() {
	h.closeOnce.Do(func() { close(h.closed) })
}

// done returns the channel that's closed by closeAll. SSE handlers select
// on it alongside the request context and their own reload channel, so a
// shutdown unblocks them the same way a client disconnect would.
func (h *hub) done() <-chan struct{} {
	return h.closed
}
