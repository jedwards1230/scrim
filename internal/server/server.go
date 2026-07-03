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
	"github.com/jedwards1230/scrim/internal/state"
	"github.com/jedwards1230/scrim/internal/version"
)

// Server is the scrim daemon's HTTP server plus its supporting lifecycle
// state (SSE hub, filesystem watcher, activity tracking, idle reaper).
type Server struct {
	cfg         config.Config
	canvasesDir string
	startedAt   time.Time

	hub      *hub
	activity *activityTracker

	stopCh   chan struct{}
	stopOnce sync.Once

	port int // actual bound port, set once in Run before any handler starts
}

// New returns a Server configured from cfg. Call Run to start it.
func New(cfg config.Config) *Server {
	return &Server{
		cfg:         cfg,
		canvasesDir: cfg.CanvasesDir(),
		startedAt:   time.Now(),
		hub:         newHub(),
		activity:    newActivityTracker(),
		stopCh:      make(chan struct{}),
		port:        cfg.Port,
	}
}

// Run starts the HTTP server, filesystem watcher, and idle reaper, and
// blocks until the daemon is asked to stop: via ctx being canceled (e.g. a
// signal), the /api/stop handler, or the idle reaper. It always cleans up
// the state file before returning.
func (s *Server) Run(ctx context.Context) error {
	if err := os.MkdirAll(s.canvasesDir, 0o755); err != nil { //nolint:gosec // canvases dir is a user-owned working directory
		return fmt.Errorf("creating canvases dir: %w", err)
	}

	listener, err := net.Listen("tcp", net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port)))
	if err != nil {
		return fmt.Errorf("binding %s:%d: %w", s.cfg.Host, s.cfg.Port, err)
	}
	if tcpAddr, ok := listener.Addr().(*net.TCPAddr); ok {
		s.port = tcpAddr.Port
	}

	token, err := state.NewToken()
	if err != nil {
		_ = listener.Close()
		return err
	}
	st := &state.State{
		PID:       os.Getpid(),
		Host:      s.cfg.Host,
		Port:      s.port,
		Token:     token,
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

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go s.reap(runCtx)

	httpServer := &http.Server{
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
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

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = httpServer.Shutdown(shutdownCtx)

	return state.Remove(s.cfg.StateFilePath())
}

// initiateShutdown asks Run to stop, exactly once. Safe to call from
// multiple goroutines (the /api/stop handler and the idle reaper both call
// it).
func (s *Server) initiateShutdown() {
	s.stopOnce.Do(func() { close(s.stopCh) })
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
