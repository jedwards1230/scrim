package mcpserver

import (
	"context"
	"sync"
	"time"
)

// Agent-connection registration at CONNECT time.
//
// The hub records an agent connection (internal/agentconn) inside its own
// request gate, so a row only appears once a tool call actually reaches it. An
// OAuth client that has authorized but not yet called a tool is therefore
// invisible on the hub's devices & access page -- and cannot be pre-emptively
// revoked, which is exactly the window in which an operator would want to cut
// it off.
//
// agentRegistrar closes that window without a new hub endpoint or a second
// registry in this process: the first time the OAuth middleware validates a
// token for a connection this process has not seen (an `initialize` or
// `tools/list` is enough), it fires ONE cheap, read-only hub call in the
// BACKGROUND carrying the usual X-Scrim-Actor-* attribution, so the hub's
// existing admitAgentConn path records it exactly as a real tool call would.
//
// Two properties are load-bearing:
//
//   - It never blocks or fails the auth path. The call runs on its own
//     goroutine with its own context, and its error is discarded: a hub that is
//     down, slow, or erroring must never turn a valid token into a 401/503.
//   - It is inert outside hub-mode OAuth. The registrar is only constructed
//     when the streamable-HTTP transport runs in hub mode WITH OAuth enabled
//     (see newHTTPHandler); stdio, local mode, and the HMAC forwarded-identity
//     plane all leave it nil, and a nil registrar registers nothing.
//
// Nothing here logs: not the subject, the client id, the token, or the hub URL.

// agentRegistrarCap bounds the in-process dedupe set. Each entry is one
// (subject, client, token-iat) triple, so a long-lived server accumulates one
// per token refresh -- an unbounded map would be a slow leak. At the cap the
// OLDEST-INSERTED key is evicted (FIFO); the only cost of evicting a key that
// is still in use is one redundant, idempotent hub call the next time that
// token is seen.
const agentRegistrarCap = 1024

// connKey identifies a connection for registration purposes: the principal, the
// OAuth client it authorized, and the presented token's issued-at.
//
// The iat is part of the key on purpose. A revocation on the hub is lifted only
// by a token issued AFTER it (agentconn.Admit), so a re-authorized client must
// be able to register again and clear a stale revocation -- keying on
// (subject, client) alone would suppress exactly that call. A token with the
// SAME iat is the same credential and needs no second registration.
type connKey struct {
	subject  string
	clientID string
	issuedAt int64
}

// agentRegistrar fires at most one background hub registration per connKey.
type agentRegistrar struct {
	// call performs the registration round-trip against the hub. It is a field
	// (rather than a *hubBackend) so tests can substitute a counting stub.
	call    func(context.Context) error
	timeout time.Duration

	mu    sync.Mutex
	seen  map[connKey]struct{}
	order []connKey // insertion order, for FIFO eviction at cap
	cap   int

	// wg tracks in-flight registration goroutines. Production never waits on it
	// (the whole point is that registration is fire-and-forget); it exists so
	// tests can assert deterministically on what was and was not called.
	wg sync.WaitGroup
}

// newAgentRegistrar builds a registrar that registers a connection by making the
// hub's cheapest authenticated read (GET /api/status) with the actor attached.
// That is deliberately an EXISTING call: the hub's gate then sees exactly the
// shape it sees for a real tool call, so admitAgentConn records the connection
// with no new endpoint on either side.
func newAgentRegistrar(b *hubBackend) *agentRegistrar {
	return &agentRegistrar{
		call:    func(ctx context.Context) error { _, err := b.Status(ctx); return err },
		timeout: hubTimeout,
		seen:    make(map[connKey]struct{}),
		cap:     agentRegistrarCap,
	}
}

// note records k and reports whether it was NEW. Marking happens BEFORE the
// call is launched so concurrent requests presenting the same token collapse to
// a single registration.
//
// A failed registration is deliberately NOT un-marked: the connection is still
// recorded by the hub's gate on the first real tool call (today's behaviour), so
// a persistently unreachable hub degrades to the old timing rather than to one
// retry per request.
func (a *agentRegistrar) note(k connKey) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.seen[k]; ok {
		return false
	}
	if len(a.order) >= a.cap {
		oldest := a.order[0]
		a.order = a.order[1:]
		delete(a.seen, oldest)
	}
	a.seen[k] = struct{}{}
	a.order = append(a.order, k)
	return true
}

// Register fires a background registration for act if this process has not
// already registered that exact connection. It returns immediately and never
// reports an error -- a registration failure must not be visible to the request
// that triggered it. A nil registrar (local mode, stdio, OAuth off) does nothing.
func (a *agentRegistrar) Register(act actor) {
	if a == nil || act.ID == "" {
		return
	}
	k := connKey{subject: act.ID, clientID: act.ClientID}
	if !act.IssuedAt.IsZero() {
		k.issuedAt = act.IssuedAt.Unix()
	}
	if !a.note(k) {
		return
	}
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		// A fresh background context: the triggering request's context is
		// cancelled the moment its response is written, which would abort the
		// registration it just started. The timeout bounds the goroutine.
		ctx, cancel := context.WithTimeout(ctxWithActor(context.Background(), act), a.timeout)
		defer cancel()
		_ = a.call(ctx)
	}()
}

// wait blocks until every in-flight registration has finished. Test-only.
func (a *agentRegistrar) wait() {
	if a == nil {
		return
	}
	a.wg.Wait()
}
