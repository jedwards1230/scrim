package cli

import (
	"bytes"
	"testing"

	"github.com/jedwards1230/scrim/internal/apiclient"
)

// TestCmdLinkNeverLaunchesBrowser covers `scrim link` (both the no-id
// dashboard case and the with-id canvas case): link is permanently
// print-only, so it must never call into the launchBrowser seam --
// regardless of anything that would opt cmdOpen in, since link doesn't
// even parse a --browser flag or look at SCRIM_OPEN_BROWSER.
func TestCmdLinkNeverLaunchesBrowser(t *testing.T) {
	tests := []struct {
		name string
		args func(dir string) []string
	}{
		{
			name: "no id: dashboard URL",
			args: func(dir string) []string { return []string{"--dir", dir} },
		},
		{
			name: "with id: canvas URL",
			args: func(dir string) []string { return []string{"--dir", dir, "note"} },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newFakeDaemon(t, []apiclient.CanvasResponse{
				{ID: "note", URL: "http://127.0.0.1:7777/c/note/?t=abc"},
			})
			t.Setenv(openBrowserEnvVar, "1") // even a truthy env must not affect link
			calls := stubLaunchBrowser(t)

			var stdout, stderr bytes.Buffer
			args := tt.args(cfg.Dir)
			if code := cmdLink(args, &stdout, &stderr); code != 0 {
				t.Fatalf("cmdLink(%v) = %d, want 0 (stderr: %s)", args, code, stderr.String())
			}
			if stdout.Len() == 0 {
				t.Error("cmdLink() printed nothing to stdout, want a URL")
			}
			if len(*calls) > 0 {
				t.Errorf("cmdLink(%v) launched a browser (calls %v), want it to never launch one", args, *calls)
			}
		})
	}
}

// TestCmdLinkCanvasNotFound pins link's error path -- it reuses
// resolveAndPrintURLs, so this also exercises that helper's not-found
// branch from a second call site (cmdOpen's own tests exercise the
// found-canvas and dashboard branches).
func TestCmdLinkCanvasNotFound(t *testing.T) {
	cfg := newFakeDaemon(t, nil)
	var stdout, stderr bytes.Buffer
	args := []string{"--dir", cfg.Dir, "missing"}
	if code := cmdLink(args, &stdout, &stderr); code != 1 {
		t.Errorf("cmdLink(%v) = %d, want 1", args, code)
	}
	if stdout.Len() != 0 {
		t.Errorf("cmdLink() stdout = %q, want empty on not-found", stdout.String())
	}
}
