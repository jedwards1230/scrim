package mcpserver

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jedwards1230/scrim/internal/config"
)

func TestIsLoopbackAddr(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:9000", true},
		{"[::1]:9000", true},
		{"localhost:9000", true},
		{"LocalHost:9000", true},
		{":9000", false},
		{"0.0.0.0:9000", false},
		{"192.0.2.10:9000", false},
		{"example.com:9000", false},
		{"127.0.0.1", false}, // malformed (no port) → fail closed
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.addr, func(t *testing.T) {
			if got := IsLoopbackAddr(tc.addr); got != tc.want {
				t.Errorf("IsLoopbackAddr(%q) = %v, want %v", tc.addr, got, tc.want)
			}
		})
	}
}

func TestNewHTTPServerTimeouts(t *testing.T) {
	srv := newHTTPServer("127.0.0.1:0", http.NewServeMux())
	if srv.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, want 10s", srv.ReadHeaderTimeout)
	}
	if srv.ReadTimeout != 60*time.Second {
		t.Errorf("ReadTimeout = %v, want 60s", srv.ReadTimeout)
	}
	if srv.IdleTimeout != 120*time.Second {
		t.Errorf("IdleTimeout = %v, want 120s", srv.IdleTimeout)
	}
	// WriteTimeout must stay 0 (unlimited) for MCP streaming responses.
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0 (unlimited)", srv.WriteTimeout)
	}
}

func TestNewHTTPHandlerHealthz(t *testing.T) {
	cfg := config.Config{Dir: t.TempDir(), Host: "127.0.0.1", Port: 7799}
	ts := httptest.NewServer(newHTTPHandler(cfg, "test", nil, nil))
	defer ts.Close()

	resp, err := http.Get(ts.URL + healthPath)
	if err != nil {
		t.Fatalf("GET %s: %v", healthPath, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok\n" {
		t.Errorf("body = %q, want %q", string(body), "ok\n")
	}
}

// TestServeHTTPLifecycle binds an ephemeral loopback port, confirms /healthz is
// live, then cancels the context and asserts ServeHTTP returns cleanly (nil).
func TestServeHTTPLifecycle(t *testing.T) {
	// Reserve a free loopback port, then release it for ServeHTTP to bind.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	cfg := config.Config{Dir: t.TempDir(), Host: "127.0.0.1", Port: 7799}
	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() { errCh <- ServeHTTP(ctx, addr, cfg, "test", nil, OAuthConfig{}, io.Discard) }()

	// Poll /healthz until the server is up (or give up).
	var up bool
	for i := 0; i < 50; i++ {
		resp, getErr := http.Get("http://" + addr + healthPath)
		if getErr == nil {
			_ = resp.Body.Close()
			up = resp.StatusCode == http.StatusOK
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !up {
		cancel()
		<-errCh
		t.Fatal("server did not become healthy")
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("ServeHTTP returned %v, want nil on clean shutdown", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("ServeHTTP did not return after ctx cancel")
	}
}
