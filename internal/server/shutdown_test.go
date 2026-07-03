package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jedwards1230/scrim/internal/canvas"
	"github.com/jedwards1230/scrim/internal/config"
	"github.com/jedwards1230/scrim/internal/state"
)

// This is the regression test for issue #11: `scrim stop` timed out waiting
// for the daemon to exit whenever a browser tab held an SSE connection
// open, because http.Server.Shutdown waits for in-flight handlers to
// return, and the SSE handler had no reason to return on its own -- it just
// sat blocked selecting on the (never-closed) request context and its
// reload channel until the shutdown deadline or an actual client
// disconnect. Without hub.closeAll wired into initiateShutdown, this test
// would time out waiting for runErrCh (bounded well under the 5s
// http.Server.Shutdown deadline used in Run), demonstrating the bug; with
// the fix, Run returns almost immediately after shutdown is initiated.
func TestShutdownClosesOpenSSEConnectionsPromptly(t *testing.T) {
	cfg := config.Config{
		Dir:         t.TempDir(),
		Host:        "127.0.0.1",
		Port:        0,
		IdleTimeout: time.Hour,
		NoAuth:      true,
	}
	srv := New(cfg)

	if _, err := canvas.Create(cfg.CanvasesDir(), "demo", "Demo"); err != nil {
		t.Fatalf("canvas.Create() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runErrCh := make(chan error, 1)
	go func() { runErrCh <- srv.Run(ctx) }()

	st := waitForState(t, cfg.StateFilePath())

	// Open a real SSE connection and leave it open, the same way a browser
	// tab watching the canvas would -- this is the connection that must not
	// block shutdown.
	req, err := http.NewRequest(http.MethodGet, st.BaseURL()+"/c/demo/__events", nil)
	if err != nil {
		t.Fatalf("building SSE request: %v", err)
	}
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("opening SSE connection: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SSE connection status = %d, want 200", resp.StatusCode)
	}

	if got := srv.hub.clientCount(); got != 1 {
		t.Fatalf("hub.clientCount() = %d after connecting, want 1", got)
	}

	srv.initiateShutdown()

	const wantWithin = 2 * time.Second
	select {
	case err := <-runErrCh:
		if err != nil {
			t.Errorf("Run() error = %v, want nil", err)
		}
	case <-time.After(wantWithin):
		t.Fatalf("Run() did not return within %s of initiateShutdown with an open SSE connection -- issue #11 regression", wantWithin)
	}

	if got := srv.hub.clientCount(); got != 0 {
		t.Errorf("hub.clientCount() = %d after shutdown, want 0 (the SSE handler should have unregistered on its way out, same as a normal disconnect)", got)
	}
}

// waitForState polls for path to appear and parse, the same way the daemon
// package's healthyState does, since Run binds an OS-assigned port
// (cfg.Port == 0) asynchronously in its own goroutine.
func waitForState(t *testing.T, path string) *state.State {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st, err := state.Load(path); err == nil {
			return st
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("state file %s was not written within the deadline", path)
	return nil
}
