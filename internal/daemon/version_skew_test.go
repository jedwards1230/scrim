package daemon

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jedwards1230/scrim/internal/config"
	"github.com/jedwards1230/scrim/internal/state"
)

func TestIsDevVersion(t *testing.T) {
	tests := []struct {
		name string
		v    string
		want bool
	}{
		{"empty string", "", true},
		{"literal dev sentinel", "dev", true},
		{"semver", "1.2.3", false},
		{"git-derived short commit", "fa450e7", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isDevVersion(tt.v); got != tt.want {
				t.Errorf("isDevVersion(%q) = %v, want %v", tt.v, got, tt.want)
			}
		})
	}
}

func TestVersionSkewed(t *testing.T) {
	tests := []struct {
		name          string
		cliVersion    string
		daemonVersion string
		want          bool
	}{
		{"matching versions", "1.0.0", "1.0.0", false},
		{"different versions", "1.0.0", "0.9.0", true},
		{"cli dev sentinel never mismatches", "dev", "1.0.0", false},
		{"cli empty version never mismatches", "", "1.0.0", false},
		{"daemon on dev sentinel against a real cli version is a mismatch", "1.0.0", "dev", true},
		{"both on the dev sentinel", "dev", "dev", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := versionSkewed(tt.cliVersion, tt.daemonVersion); got != tt.want {
				t.Errorf("versionSkewed(%q, %q) = %v, want %v", tt.cliVersion, tt.daemonVersion, got, tt.want)
			}
		})
	}
}

// TestStopIfVersionSkewed exercises the restart decision against a fake
// daemon, using the same pattern as TestStopUsesStateHostPortNotConfig: a
// real (short-lived) subprocess stands in for a live daemon pid, and an
// httptest server stands in for its HTTP control API.
func TestStopIfVersionSkewed(t *testing.T) {
	tests := []struct {
		name          string
		daemonVersion string
		cliVersion    string
		wantStopped   bool
		wantStopHits  int32
	}{
		{
			name:          "matching version leaves the daemon running",
			daemonVersion: "1.0.0",
			cliVersion:    "1.0.0",
			wantStopped:   false,
			wantStopHits:  0,
		},
		{
			name:          "mismatched version stops the daemon",
			daemonVersion: "1.0.0",
			cliVersion:    "2.0.0",
			wantStopped:   true,
			wantStopHits:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
			cfg := config.Config{Dir: dir, Host: "127.0.0.1", Port: srvPort, IdleTimeout: time.Hour}

			cmd := shortLivedCommand()
			if err := cmd.Start(); err != nil {
				t.Fatalf("starting short-lived process: %v", err)
			}
			pid := cmd.Process.Pid
			waitDone := make(chan struct{})
			go func() {
				_ = cmd.Wait()
				close(waitDone)
			}()
			defer func() { <-waitDone }()

			st := &state.State{PID: pid, Host: "127.0.0.1", Port: srvPort, Version: tt.daemonVersion}
			if err := state.Save(cfg.StateFilePath(), st); err != nil {
				t.Fatalf("state.Save() error = %v", err)
			}

			stopped, err := stopIfVersionSkewed(cfg, st, tt.cliVersion)
			if err != nil {
				t.Fatalf("stopIfVersionSkewed() error = %v, want nil", err)
			}
			if stopped != tt.wantStopped {
				t.Errorf("stopIfVersionSkewed() stopped = %v, want %v", stopped, tt.wantStopped)
			}
			if got := stopHits.Load(); got != tt.wantStopHits {
				t.Errorf("/api/stop hit count = %d, want %d", got, tt.wantStopHits)
			}
		})
	}
}
