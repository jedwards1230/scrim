package daemon

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jedwards1230/scrim/internal/config"
	"github.com/jedwards1230/scrim/internal/state"
)

// TestFinalizeStopTreatsDeadPidAsImmediateSuccess reproduces the
// SIGKILL/OOM-kill scenario: the pid is already dead but nothing cleaned up
// the state file. finalizeStop must return success right away (not burn the
// full stopTimeout polling for the file to also disappear) and remove the
// stale file itself.
func TestFinalizeStopTreatsDeadPidAsImmediateSuccess(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{Dir: dir, Host: "127.0.0.1", Port: 7777, IdleTimeout: time.Hour}

	pid := deadPid(t)
	if err := state.Save(cfg.StateFilePath(), &state.State{
		PID:  pid,
		Host: cfg.Host,
		Port: cfg.Port,
	}); err != nil {
		t.Fatalf("state.Save() error = %v", err)
	}

	start := time.Now()
	if err := finalizeStop(cfg, pid); err != nil {
		t.Fatalf("finalizeStop() error = %v, want nil (pid already dead)", err)
	}
	if elapsed := time.Since(start); elapsed > stopTimeout/2 {
		t.Errorf("finalizeStop() took %v for an already-dead pid, want a fast return well under stopTimeout=%v", elapsed, stopTimeout)
	}

	if _, err := os.Stat(cfg.StateFilePath()); !os.IsNotExist(err) {
		t.Errorf("state file still exists after finalizeStop(), want it cleaned up (stat err = %v)", err)
	}
}

// unusedLoopbackPort returns a port on 127.0.0.1 that nothing is listening
// on: it binds a listener just long enough to learn a free port, then closes
// it, so a client that connects there gets a fast connection-refused instead
// of hanging.
func unusedLoopbackPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("closing probe listener: %v", err)
	}
	return port
}

// TestStopUsesStateHostPortNotConfig is a regression test: Stop must build
// its API client from the state file's Host/Port (where the daemon actually
// bound), not from cfg's -- a running daemon can legitimately differ from
// the caller's config (started with different --host/--port, or an
// auto-assigned port). cfg here points at a port nothing is listening on, so
// if Stop mistakenly used cfg.BaseURL() the request would fail outright
// instead of reaching the fake daemon.
func TestStopUsesStateHostPortNotConfig(t *testing.T) {
	var stopHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/api/stop", func(w http.ResponseWriter, r *http.Request) {
		stopHits.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	srvURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("url.Parse(%q) error = %v", srv.URL, err)
	}
	srvPort, err := strconv.Atoi(srvURL.Port())
	if err != nil {
		t.Fatalf("parsing server port: %v", err)
	}

	dir := t.TempDir()
	// cfg deliberately targets a different, unused port than the daemon's
	// real state -- this is the config/state mismatch Stop must not fall
	// prey to.
	cfg := config.Config{Dir: dir, Host: "127.0.0.1", Port: unusedLoopbackPort(t), IdleTimeout: time.Hour}

	// healthyState requires a live pid, so this needs a real (if trivial)
	// subprocess -- one that outlives the health check but exits well
	// within stopTimeout, so finalizeStop's poll for its exit succeeds
	// quickly rather than timing out.
	cmd := shortLivedCommand()
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting short-lived process: %v", err)
	}
	pid := cmd.Process.Pid
	// Reap it as soon as it exits -- otherwise it lingers as a zombie
	// (still "alive" to pidAlive's signal-0 check) until Wait is called,
	// which would make finalizeStop's poll for its exit time out.
	waitDone := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(waitDone)
	}()
	defer func() { <-waitDone }()

	if err := state.Save(cfg.StateFilePath(), &state.State{
		PID:  pid,
		Host: "127.0.0.1",
		Port: srvPort,
	}); err != nil {
		t.Fatalf("state.Save() error = %v", err)
	}

	found, err := Stop(cfg)
	if err != nil {
		t.Fatalf("Stop() error = %v, want nil", err)
	}
	if !found {
		t.Fatal("Stop() found = false, want true")
	}
	if got := stopHits.Load(); got != 1 {
		t.Errorf("fake daemon's /api/stop hit count = %d, want 1 (Stop() must target the state's host:port, not cfg's)", got)
	}
}
