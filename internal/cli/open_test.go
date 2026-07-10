package cli

import "testing"

// TestPrimaryURL exercises the seam cmdOpen uses to decide what to hand
// openBrowser. openBrowser itself launches a real browser and can't be
// asserted on in a test, so this pins the pure decision instead: when mDNS
// is inactive there's only ever one line and it's used unchanged; when
// mDNS is active, the first (scrim.local) line is used -- not the raw
// host:port URL a bind-all host like 0.0.0.0 would otherwise produce.
func TestPrimaryURL(t *testing.T) {
	tests := []struct {
		name     string
		host     string
		rawURL   string
		wantOpen string
	}{
		{
			name:     "mDNS inactive (loopback host): single line used unchanged",
			host:     "127.0.0.1",
			rawURL:   "http://127.0.0.1:7777/?t=abc123",
			wantOpen: "http://127.0.0.1:7777/?t=abc123",
		},
		{
			name:     "mDNS active (LAN host): first line (scrim.local) used, not the raw URL",
			host:     "192.0.2.50",
			rawURL:   "http://192.0.2.50:7777/c/report/?t=abc123",
			wantOpen: "http://scrim.local:7777/c/report/?t=abc123",
		},
		{
			name:     "mDNS active, bind-all host: scrim.local used, not the unusable 0.0.0.0 URL",
			host:     "0.0.0.0",
			rawURL:   "http://0.0.0.0:7777/",
			wantOpen: "http://scrim.local:7777/",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lines := urlLines(tt.host, tt.rawURL)
			got := primaryURL(lines, tt.rawURL)
			if got != tt.wantOpen {
				t.Errorf("primaryURL() = %q, want %q", got, tt.wantOpen)
			}
			if got == tt.rawURL && tt.wantOpen != tt.rawURL {
				t.Errorf("primaryURL() fell back to the raw host:port URL %q, want the primary printed line", tt.rawURL)
			}
		})
	}

	t.Run("empty lines falls back to the given URL", func(t *testing.T) {
		fallback := "http://127.0.0.1:7777/"
		if got := primaryURL(nil, fallback); got != fallback {
			t.Errorf("primaryURL(nil, %q) = %q, want %q", fallback, got, fallback)
		}
	})
}
