package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jedwards1230/scrim/internal/state"
)

func TestBaseURLFor(t *testing.T) {
	tests := []struct {
		name string
		st   *state.State
		path string
		want string
	}{
		{
			name: "auth enabled appends token",
			st:   &state.State{Host: "127.0.0.1", Port: 7777, Token: "abc123", NoAuth: false},
			path: "/",
			want: "http://127.0.0.1:7777/?t=abc123",
		},
		{
			name: "no-auth omits token param entirely",
			st:   &state.State{Host: "127.0.0.1", Port: 7777, Token: "", NoAuth: true},
			path: "/",
			want: "http://127.0.0.1:7777/",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := baseURLFor(tt.st, tt.path); got != tt.want {
				t.Errorf("baseURLFor() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestURLLines(t *testing.T) {
	tests := []struct {
		name   string
		host   string
		rawURL string
		want   []string
	}{
		{
			name:   "loopback host: single unchanged line, no mDNS line",
			host:   "127.0.0.1",
			rawURL: "http://127.0.0.1:7777/c/report/?t=abc123",
			want:   []string{"http://127.0.0.1:7777/c/report/?t=abc123"},
		},
		{
			name:   "loopback host, no-auth: single clean line",
			host:   "127.0.0.1",
			rawURL: "http://127.0.0.1:7777/c/report/",
			want:   []string{"http://127.0.0.1:7777/c/report/"},
		},
		{
			name:   "non-loopback host: mDNS line first, plain fallback second",
			host:   "192.168.8.50",
			rawURL: "http://192.168.8.50:7777/c/report/?t=abc123",
			want: []string{
				"http://scrim.local:7777/c/report/?t=abc123",
				"http://192.168.8.50:7777/c/report/?t=abc123",
			},
		},
		{
			name:   "bind-all host advertises, but the plain URL is whatever the daemon reported",
			host:   "0.0.0.0",
			rawURL: "http://0.0.0.0:7777/",
			want: []string{
				"http://scrim.local:7777/",
				"http://0.0.0.0:7777/",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := urlLines(tt.host, tt.rawURL)
			if len(got) != len(tt.want) {
				t.Fatalf("urlLines() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("urlLines()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestPrintURLLines(t *testing.T) {
	tests := []struct {
		name          string
		host          string
		rawURL        string
		wantContains  []string
		wantLineCount int
	}{
		{
			name:          "loopback: exactly one clean line",
			host:          "127.0.0.1",
			rawURL:        "http://127.0.0.1:7777/",
			wantContains:  []string{"http://127.0.0.1:7777/"},
			wantLineCount: 1,
		},
		{
			name:   "non-loopback: both mDNS and plain lines printed",
			host:   "192.168.8.50",
			rawURL: "http://192.168.8.50:7777/?t=xyz",
			wantContains: []string{
				"http://scrim.local:7777/?t=xyz",
				"http://192.168.8.50:7777/?t=xyz",
			},
			wantLineCount: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			printURLLines(&buf, urlLines(tt.host, tt.rawURL))
			out := buf.String()
			for _, want := range tt.wantContains {
				if !strings.Contains(out, want) {
					t.Errorf("output %q missing %q", out, want)
				}
			}
			lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
			if len(lines) != tt.wantLineCount {
				t.Errorf("printed %d lines, want %d; output:\n%s", len(lines), tt.wantLineCount, out)
			}
		})
	}
}

func TestRewriteURLHost(t *testing.T) {
	tests := []struct {
		name    string
		rawURL  string
		newHost string
		want    string
	}{
		{
			name:    "replaces host, keeps port/path/query",
			rawURL:  "http://192.168.8.50:7777/c/report/?t=abc",
			newHost: "scrim.local",
			want:    "http://scrim.local:7777/c/report/?t=abc",
		},
		{
			name:    "no port in host",
			rawURL:  "http://192.168.8.50/",
			newHost: "scrim.local",
			want:    "http://scrim.local/",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := rewriteURLHost(tt.rawURL, tt.newHost)
			if err != nil {
				t.Fatalf("rewriteURLHost() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("rewriteURLHost() = %q, want %q", got, tt.want)
			}
		})
	}
}
