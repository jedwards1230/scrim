// Package server implements the scrim daemon's HTTP server: static canvas
// serving with SSE live-reload injection, the per-canvas SSE endpoint, the
// index page, the /api/* control surface, and the idle reaper.
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/jedwards1230/scrim/internal/config"
	"github.com/jedwards1230/scrim/internal/logging"
	"github.com/jedwards1230/scrim/internal/mdns"
	"github.com/jedwards1230/scrim/internal/oidc"
	"github.com/jedwards1230/scrim/internal/principal"
	"github.com/jedwards1230/scrim/internal/state"
	"github.com/jedwards1230/scrim/internal/usertoken"
	"github.com/jedwards1230/scrim/internal/version"
)

// Server is the scrim daemon's HTTP server plus its supporting lifecycle
// state (SSE hub, filesystem watcher, activity tracking, idle reaper).
type Server struct {
	cfg         config.Config
	canvasesDir string
	metaDir     string
	startedAt   time.Time

	hub      *hub
	activity *activityTracker

	stopCh   chan struct{}
	stopOnce sync.Once

	port  int    // actual bound port, set once in Run before any handler starts
	token string // capability token; empty when cfg.NoAuth is set, set once in Run

	// hubCfg is non-nil only for a server constructed via NewHub: it carries
	// the push/read tokens and the parsed read-CIDR allowlist that
	// distinguish hub mode (see routes.go and hubgate.go). It is nil for the
	// default daemon (server.New), which is what routes() and withHubGate's
	// callers use as the hub-mode discriminator instead of a separate bool.
	hubCfg *hubConfig

	// oidcAuth is non-nil only for a hub started with OIDC configured (see
	// NewHub). When set, it becomes the hub read gate -- reads require a valid
	// OIDC session cookie instead of the CIDR/read-token check -- and the
	// /auth/* login routes are registered. Always nil for the default daemon
	// and for a hub with no OIDC config, so the fail-closed/opt-in contract is
	// a simple nil check.
	oidcAuth *oidc.Authenticator

	// principals is the hub's lazily-populated principal registry, fed on
	// login (and, later, from CF headers and grant targets). Display/
	// autocomplete only -- enforcement NEVER reads it. Non-nil only for a hub
	// (set in NewHub); the default daemon leaves it nil.
	principals *principal.Registry

	// tokens is the hub's user-minted bearer-token store: a bearer that isn't
	// the admin push token is looked up here and, when valid, acts AS its owner
	// (owner attribution + owner-only writes). Non-nil only for a hub (set in
	// NewHub); the default daemon leaves it nil. #52/#51 reach it via s.tokens.
	tokens *usertoken.Store
}

// New returns a Server configured from cfg. Call Run to start it.
func New(cfg config.Config) *Server {
	return &Server{
		cfg:         cfg,
		canvasesDir: cfg.CanvasesDir(),
		metaDir:     cfg.MetaDir(),
		startedAt:   time.Now(),
		hub:         newHub(),
		activity:    newActivityTracker(),
		stopCh:      make(chan struct{}),
		port:        cfg.Port,
	}
}

// isHub reports whether this Server was constructed via NewHub.
func (s *Server) isHub() bool { return s.hubCfg != nil }

// Handler returns the server's fully-wrapped HTTP handler (routes plus the
// auth/hub gate and security headers) -- the identical handler Run builds and
// serves. It is exposed for in-process use (tests, embedding a hub behind
// another server) and does NOT start the filesystem watcher, idle reaper, or
// permission hardening: callers that want the full daemon lifecycle use Run
// instead. It does not widen the hub-mode surface -- a default-mode server's
// Handler registers no hub routes, exactly as routes() always has.
func (s *Server) Handler() http.Handler { return s.routes() }

// Run starts the HTTP server, filesystem watcher, and idle reaper, and
// blocks until the daemon is asked to stop: via ctx being canceled (e.g. a
// signal), the /api/stop handler, or the idle reaper. It always cleans up
// the state file before returning.
func (s *Server) Run(ctx context.Context) error {
	// HardenPermissions tightens s.cfg.Dir (and any preexisting state/log
	// file under it) to owner-only before anything else touches the
	// filesystem, so a directory created by an older scrim version -- or by
	// hand -- doesn't stay world-readable just because this daemon didn't
	// create it itself.
	if err := s.cfg.HardenPermissions(); err != nil {
		return fmt.Errorf("hardening scrim dir permissions: %w", err)
	}
	if err := os.MkdirAll(s.canvasesDir, 0o755); err != nil { //nolint:gosec // canvases dir is a user-owned working directory; its parent (s.cfg.Dir) is already owner-only
		return fmt.Errorf("creating canvases dir: %w", err)
	}

	listener, err := net.Listen("tcp", net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port)))
	if err != nil {
		return fmt.Errorf("binding %s:%d: %w", s.cfg.Host, s.cfg.Port, err)
	}
	if tcpAddr, ok := listener.Addr().(*net.TCPAddr); ok {
		s.port = tcpAddr.Port
	}

	var token string
	if !s.cfg.NoAuth {
		var err error
		token, err = state.NewToken()
		if err != nil {
			_ = listener.Close()
			return err
		}
	}
	s.token = token

	st := &state.State{
		PID:       os.Getpid(),
		Host:      s.cfg.Host,
		Port:      s.port,
		Token:     token,
		NoAuth:    s.cfg.NoAuth,
		Version:   version.Short(),
		StartedAt: s.startedAt,
	}
	if err := state.Save(s.cfg.StateFilePath(), st); err != nil {
		_ = listener.Close()
		return err
	}

	watcher, err := newCanvasWatcher(s.canvasesDir, defaultDebounce, s.hub.broadcast)
	if err != nil {
		_ = listener.Close()
		_ = state.Remove(s.cfg.StateFilePath())
		return err
	}
	defer watcher.Close() //nolint:errcheck // best-effort cleanup on shutdown

	// mDNS advertisement is a discovery aid, not a functional requirement,
	// and it's only meaningful when something on the LAN could actually
	// reach this daemon (see mdns.IsLoopbackHost) and the daemon hasn't
	// opted out of it entirely (--no-mdns / s.cfg.NoMDNS). A bind failure
	// (e.g. no multicast support in this environment) is logged and
	// otherwise ignored rather than treated as fatal to the daemon.
	// mdnsAdv.Stop is nil-safe, so it's deferred unconditionally and
	// withdraws the advertisement on every exit path below -- graceful
	// stop, idle reap, and the http-server-error path alike.
	mdnsAdv, mdnsErr := mdns.MaybeStart(s.cfg.Host, s.port, s.cfg.NoMDNS)
	if mdnsErr != nil {
		logging.Error(logging.CategoryMDNS, fmt.Errorf("not advertising %s: %w", mdns.ServiceHost, mdnsErr))
	}
	defer func() { _ = mdnsAdv.Stop() }()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go s.reap(runCtx)

	httpServer := &http.Server{
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		// Left nil, net/http falls back to its own package-level logger
		// (straight to os.Stderr) for things like malformed requests or a
		// panic recovered inside a handler -- bypassing scrim's logging
		// policy (never a raw request path/query/token) entirely. Routing
		// it through logging.StdLogger applies the same scrubbing as every
		// other log call site in this package.
		ErrorLog: logging.StdLogger(logging.CategoryHTTP),
	}
	serveErrCh := make(chan error, 1)
	go func() { serveErrCh <- httpServer.Serve(listener) }()

	select {
	case <-ctx.Done():
	case <-s.stopCh:
	case serveErr := <-serveErrCh:
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			_ = state.Remove(s.cfg.StateFilePath())
			return fmt.Errorf("http server: %w", serveErr)
		}
	}

	// Whichever case unblocked the select above, every open SSE connection
	// must be told to return before http.Server.Shutdown below waits on
	// in-flight handlers -- otherwise a browser tab left open on a canvas
	// blocks shutdown for up to the timeout, exactly like issue #11. The
	// stopCh path already calls this via initiateShutdown, but ctx.Done()
	// (an OS signal via signal.NotifyContext in cli/serve.go) and the
	// serveErrCh path do not, so it's called unconditionally here;
	// hub.closeAll is sync.Once-guarded, so a repeat call is a no-op.
	s.hub.closeAll()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = httpServer.Shutdown(shutdownCtx)

	return state.Remove(s.cfg.StateFilePath())
}

// initiateShutdown asks Run to stop, exactly once. Safe to call from
// multiple goroutines (the /api/stop handler and the idle reaper both call
// it). It also tells every open SSE connection to return promptly, so
// http.Server.Shutdown in Run doesn't sit blocked waiting on a long-lived
// handler that has no reason to return on its own (e.g. a browser tab left
// open on a canvas) -- see hub.closeAll.
func (s *Server) initiateShutdown() {
	s.stopOnce.Do(func() {
		s.hub.closeAll()
		close(s.stopCh)
	})
}

func (s *Server) reap(ctx context.Context) {
	if s.cfg.IdleTimeout <= 0 {
		// Reaping is disabled entirely; never start a ticker that can only
		// ever wake up to find shouldReap false.
		return
	}
	ticker := time.NewTicker(reapCheckInterval(s.cfg.IdleTimeout))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if shouldReap(time.Now(), s.activity.last(), s.cfg.IdleTimeout, s.hub.clientCount()) {
				s.initiateShutdown()
				return
			}
		}
	}
}

// activityTracker records the last time any HTTP request (including an SSE
// connection's open and close) touched the server.
type activityTracker struct {
	mu     sync.Mutex
	lastAt time.Time
}

func newActivityTracker() *activityTracker {
	return &activityTracker{lastAt: time.Now()}
}

func (a *activityTracker) touch() {
	a.mu.Lock()
	a.lastAt = time.Now()
	a.mu.Unlock()
}

func (a *activityTracker) last() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastAt
}
