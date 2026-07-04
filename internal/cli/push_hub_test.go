package cli

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func TestSplitCSV(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{name: "two CIDRs", in: "127.0.0.0/8,::1/128", want: []string{"127.0.0.0/8", "::1/128"}},
		{name: "trims whitespace and drops empties", in: " a , b ,, c ", want: []string{"a", "b", "c"}},
		{name: "single entry", in: "10.0.0.0/8", want: []string{"10.0.0.0/8"}},
		{name: "empty string", in: "", want: nil},
		{name: "only separators and spaces", in: " , , ", want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := splitCSV(tt.in); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("splitCSV(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestEnvOr(t *testing.T) {
	const key = "SCRIM_TEST_ENVOR"
	t.Run("unset falls back", func(t *testing.T) {
		if got := envOr(key, "fallback"); got != "fallback" {
			t.Errorf("envOr(unset) = %q, want fallback", got)
		}
	})
	t.Run("set non-empty wins", func(t *testing.T) {
		t.Setenv(key, "value")
		if got := envOr(key, "fallback"); got != "value" {
			t.Errorf("envOr(set) = %q, want value", got)
		}
	})
	t.Run("set empty falls back", func(t *testing.T) {
		t.Setenv(key, "")
		if got := envOr(key, "fallback"); got != "fallback" {
			t.Errorf("envOr(empty) = %q, want fallback", got)
		}
	})
}

func TestEnvIntOr(t *testing.T) {
	const key = "SCRIM_TEST_ENVINTOR"
	tests := []struct {
		name string
		set  bool
		val  string
		want int
	}{
		{name: "unset falls back", set: false, want: 7788},
		{name: "valid int wins", set: true, val: "9000", want: 9000},
		{name: "empty falls back", set: true, val: "", want: 7788},
		{name: "malformed falls back", set: true, val: "not-a-number", want: 7788},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.set {
				t.Setenv(key, tt.val)
			}
			if got := envIntOr(key, 7788); got != tt.want {
				t.Errorf("envIntOr(%q) = %d, want %d", tt.val, got, tt.want)
			}
		})
	}
}

// TestCmdPushUsageErrors covers push's argument-validation exits, all of which
// happen before any filesystem or network access.
func TestCmdPushUsageErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want int
	}{
		{name: "no positional id", args: []string{"--to", "http://127.0.0.1:1"}, want: 2},
		{name: "invalid id", args: []string{"--to", "http://127.0.0.1:1", "bad/id"}, want: 2},
		{name: "valid id but no --to", args: []string{"report"}, want: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := append([]string{"--dir", t.TempDir()}, tt.args...)
			var stdout, stderr bytes.Buffer
			if got := cmdPush(args, &stdout, &stderr); got != tt.want {
				t.Errorf("cmdPush(%v) = %d, want %d (stderr: %s)", args, got, tt.want, stderr.String())
			}
		})
	}
}

// TestCmdPushTokenPrecedence proves the push token comes from --token or, in
// its absence, SCRIM_PUSH_TOKEN -- and that with neither, push fails closed at
// the "token required" gate. The two supplied-token cases get PAST that gate
// and fail later at "canvas not found" (the canvas dir is empty), which is how
// the test distinguishes "token accepted" from "token missing" without any
// network I/O.
func TestCmdPushTokenPrecedence(t *testing.T) {
	tests := []struct {
		name     string
		env      string // SCRIM_PUSH_TOKEN, "" means leave unset
		tokenArg []string
		wantCode int
		wantErr  string
	}{
		{name: "no token anywhere fails closed", wantCode: 1, wantErr: "is required"},
		{name: "SCRIM_PUSH_TOKEN accepted", env: "envtok", wantCode: 1, wantErr: "not found"},
		{name: "--token accepted", tokenArg: []string{"--token", "flagtok"}, wantCode: 1, wantErr: "not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.env != "" {
				t.Setenv("SCRIM_PUSH_TOKEN", tt.env)
			}
			args := append([]string{"--dir", t.TempDir(), "--to", "http://127.0.0.1:1", "report"}, tt.tokenArg...)
			var stdout, stderr bytes.Buffer
			if got := cmdPush(args, &stdout, &stderr); got != tt.wantCode {
				t.Fatalf("cmdPush(%v) = %d, want %d (stderr: %s)", args, got, tt.wantCode, stderr.String())
			}
			if !strings.Contains(stderr.String(), tt.wantErr) {
				t.Errorf("cmdPush stderr = %q, want it to contain %q", stderr.String(), tt.wantErr)
			}
		})
	}
}

// TestCmdHubUsageAndTokenGate covers hub's argument/env plumbing on the paths
// that resolve before the (blocking) server run: an unexpected positional arg,
// the push-token gate, and the CIDR-parse path. The push-token cases feed a
// deliberately malformed --allow so server.NewHub fails fast (proving the
// token was accepted and splitCSV's output reached the CIDR parser) instead of
// starting a long-lived hub the test would then have to tear down.
func TestCmdHubUsageAndTokenGate(t *testing.T) {
	tests := []struct {
		name     string
		env      string // SCRIM_PUSH_TOKEN, "" means leave unset
		args     []string
		wantCode int
		wantErr  string
	}{
		{name: "unexpected positional arg", args: []string{"stray"}, wantCode: 2, wantErr: "usage"},
		{name: "no push token fails closed", wantCode: 1, wantErr: "push-token"},
		{
			name:     "--push-token accepted, malformed CIDR fails at NewHub",
			args:     []string{"--push-token", "flagtok", "--allow", "not-a-cidr"},
			wantCode: 1, wantErr: "",
		},
		{
			name:     "SCRIM_PUSH_TOKEN accepted, malformed CIDR fails at NewHub",
			env:      "envtok",
			args:     []string{"--allow", "not-a-cidr"},
			wantCode: 1, wantErr: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.env != "" {
				t.Setenv("SCRIM_PUSH_TOKEN", tt.env)
			}
			args := append([]string{"--data", t.TempDir()}, tt.args...)
			var stdout, stderr bytes.Buffer
			if got := cmdHub(args, &stdout, &stderr); got != tt.wantCode {
				t.Fatalf("cmdHub(%v) = %d, want %d (stderr: %s)", args, got, tt.wantCode, stderr.String())
			}
			if tt.wantErr != "" && !strings.Contains(stderr.String(), tt.wantErr) {
				t.Errorf("cmdHub stderr = %q, want it to contain %q", stderr.String(), tt.wantErr)
			}
		})
	}
}
