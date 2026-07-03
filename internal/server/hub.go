package server

import "sync"

// hub tracks connected SSE clients per canvas ID and broadcasts reload
// signals to them. It also backs the idle reaper's "any SSE client
// connected" liveness signal.
type hub struct {
	mu      sync.Mutex
	clients map[string]map[chan struct{}]struct{}
}

func newHub() *hub {
	return &hub{clients: make(map[string]map[chan struct{}]struct{})}
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
