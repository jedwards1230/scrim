package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jedwards1230/scrim/internal/apiclient"
	"github.com/jedwards1230/scrim/internal/config"
	"github.com/jedwards1230/scrim/internal/state"
)

// TestShouldOpenBrowser pins the flag||env opt-in decision cmdOpen uses to
// gate the (formerly unconditional) browser launch: printing the URL is
// always the default, and a browser is only launched when the caller
// explicitly asked for it, via --browser or a truthy SCRIM_OPEN_BROWSER.
func TestShouldOpenBrowser(t *testing.T) {
	tests := []struct {
		name    string
		flagSet bool
		envVal  string
		want    bool
	}{
		{name: "neither flag nor env set: prints only, does not launch", flagSet: false, envVal: "", want: false},
		{name: "--browser flag alone launches", flagSet: true, envVal: "", want: true},
		{name: "env=1 launches", flagSet: false, envVal: "1", want: true},
		{name: "env=true launches", flagSet: false, envVal: "true", want: true},
		{name: "env=TRUE launches (strconv.ParseBool is case-insensitive)", flagSet: false, envVal: "TRUE", want: true},
		{name: "env=0 does not launch", flagSet: false, envVal: "0", want: false},
		{name: "env=false does not launch", flagSet: false, envVal: "false", want: false},
		{name: "malformed env value is treated as unset, does not launch", flagSet: false, envVal: "yes-please", want: false},
		{name: "flag set wins over a falsy env value", flagSet: true, envVal: "0", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldOpenBrowser(tt.flagSet, tt.envVal); got != tt.want {
				t.Errorf("shouldOpenBrowser(%v, %q) = %v, want %v", tt.flagSet, tt.envVal, got, tt.want)
			}
		})
	}
}

// newFakeDaemon starts an in-process httptest server that answers the two
// control-API calls cmdOpen needs (health check + canvas listing) and
// writes a state file pointing at it, so daemon.Ensure finds an
// already-healthy daemon instead of spawning a real one. The returned
// config's Dir is what a test should pass as --dir.
func newFakeDaemon(t *testing.T, canvases []apiclient.CanvasResponse) config.Config {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(apiclient.StatusResponse{Version: "dev"})
	})
	mux.HandleFunc("/api/canvases", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(canvases)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

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
	if err := state.Save(cfg.StateFilePath(), &state.State{
		PID:     os.Getpid(), // the test process itself is always alive
		Host:    "127.0.0.1",
		Port:    srvPort,
		NoAuth:  true,
		Version: "dev",
	}); err != nil {
		t.Fatalf("state.Save() error = %v", err)
	}
	return cfg
}

// stubLaunchBrowser replaces the package-level launchBrowser seam for the
// duration of a test, recording every URL it's called with instead of
// exec'ing a real OS command.
func stubLaunchBrowser(t *testing.T) *[]string {
	t.Helper()
	var calls []string
	orig := launchBrowser
	launchBrowser = func(url string) error {
		calls = append(calls, url)
		return nil
	}
	t.Cleanup(func() { launchBrowser = orig })
	return &calls
}

// TestCmdOpenNoIDBrowserOptIn covers `scrim open` (no id, the dashboard
// URL): the browser is launched only when explicitly opted in.
func TestCmdOpenNoIDBrowserOptIn(t *testing.T) {
	tests := []struct {
		name       string
		extraArgs  []string
		env        string
		wantLaunch bool
	}{
		{name: "default: no --browser, no env -- prints only, never launches", wantLaunch: false},
		{name: "--browser flag launches", extraArgs: []string{"--browser"}, wantLaunch: true},
		{name: "SCRIM_OPEN_BROWSER=1 launches without the flag", env: "1", wantLaunch: true},
		{name: "SCRIM_OPEN_BROWSER=false does not launch", env: "false", wantLaunch: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newFakeDaemon(t, nil)
			if tt.env != "" {
				t.Setenv(openBrowserEnvVar, tt.env)
			}
			calls := stubLaunchBrowser(t)

			var stdout, stderr bytes.Buffer
			args := append([]string{"--dir", cfg.Dir}, tt.extraArgs...)
			if code := cmdOpen(args, &stdout, &stderr); code != 0 {
				t.Fatalf("cmdOpen(%v) = %d, want 0 (stderr: %s)", args, code, stderr.String())
			}
			if stdout.Len() == 0 {
				t.Error("cmdOpen() printed nothing to stdout, want the dashboard URL")
			}
			if got := len(*calls) > 0; got != tt.wantLaunch {
				t.Errorf("cmdOpen(%v) launched browser = %v (calls %v), want %v", args, got, *calls, tt.wantLaunch)
			}
		})
	}
}

// TestCmdOpenWithIDBrowserOptIn covers `scrim open <id>` (the found-canvas
// branch) -- the same opt-in gating applies to this second call site.
func TestCmdOpenWithIDBrowserOptIn(t *testing.T) {
	tests := []struct {
		name       string
		extraArgs  []string
		env        string
		wantLaunch bool
	}{
		{name: "default: no --browser, no env -- prints only, never launches", wantLaunch: false},
		{name: "--browser flag launches", extraArgs: []string{"--browser"}, wantLaunch: true},
		{name: "SCRIM_OPEN_BROWSER=1 launches without the flag", env: "1", wantLaunch: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newFakeDaemon(t, []apiclient.CanvasResponse{
				{ID: "note", URL: "http://127.0.0.1:7777/c/note/?t=abc"},
			})
			if tt.env != "" {
				t.Setenv(openBrowserEnvVar, tt.env)
			}
			calls := stubLaunchBrowser(t)

			var stdout, stderr bytes.Buffer
			args := append([]string{"--dir", cfg.Dir, "note"}, tt.extraArgs...)
			if code := cmdOpen(args, &stdout, &stderr); code != 0 {
				t.Fatalf("cmdOpen(%v) = %d, want 0 (stderr: %s)", args, code, stderr.String())
			}
			if stdout.Len() == 0 {
				t.Error("cmdOpen() printed nothing to stdout, want the canvas URL")
			}
			if got := len(*calls) > 0; got != tt.wantLaunch {
				t.Errorf("cmdOpen(%v) launched browser = %v (calls %v), want %v", args, got, *calls, tt.wantLaunch)
			}
		})
	}
}
