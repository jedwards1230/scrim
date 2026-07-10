package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestMcpHTTPRefusesNonLoopbackWithoutAllowLAN verifies the secure-by-default
// gate end-to-end through Run: `mcp --http <non-loopback>` without --allow-lan
// is a usage error (exit 2) and never binds a listener. It uses an isolated
// --dir so it can never touch the real ~/.scrim.
func TestMcpHTTPRefusesNonLoopbackWithoutAllowLAN(t *testing.T) {
	for _, addr := range []string{":9999", "0.0.0.0:9999", "192.0.2.10:9999"} {
		t.Run(addr, func(t *testing.T) {
			var out, errb bytes.Buffer
			code := Run([]string{"mcp", "--dir", t.TempDir(), "--http", addr}, &out, &errb)
			if code != 2 {
				t.Fatalf("exit = %d, want 2 (usage); stderr: %s", code, errb.String())
			}
			for _, want := range []string{"--allow-lan", "UNAUTHENTICATED", "OAuth"} {
				if !strings.Contains(errb.String(), want) {
					t.Errorf("stderr = %q, want it to mention %q", errb.String(), want)
				}
			}
		})
	}
}

// TestPlanMcp exercises the pure flag/gate decision without ever starting a
// blocking server.
func TestPlanMcp(t *testing.T) {
	cases := []struct {
		name         string
		httpAddr     string
		allowLAN     bool
		oauthEnabled bool
		wantErr      bool
		wantHTTP     bool
		wantWarnLAN  bool
	}{
		{name: "stdio", httpAddr: "", wantHTTP: false},
		{name: "stdio with allow-lan warns", httpAddr: "", allowLAN: true, wantWarnLAN: true},
		{name: "loopback http", httpAddr: "127.0.0.1:9999", wantHTTP: true},
		{name: "localhost http", httpAddr: "localhost:9999", wantHTTP: true},
		{name: "ipv6 loopback http", httpAddr: "[::1]:9999", wantHTTP: true},
		{name: "non-loopback refused", httpAddr: ":9999", wantErr: true},
		{name: "non-loopback allowed with flag", httpAddr: "0.0.0.0:9999", allowLAN: true, wantHTTP: true},
		{name: "non-loopback allowed by oauth", httpAddr: "0.0.0.0:9999", oauthEnabled: true, wantHTTP: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := planMcp(tc.httpAddr, tc.allowLAN, tc.oauthEnabled)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("planMcp(%q, %v) = nil error, want an error", tc.httpAddr, tc.allowLAN)
				}
				return
			}
			if err != nil {
				t.Fatalf("planMcp(%q, %v) = %v, want nil", tc.httpAddr, tc.allowLAN, err)
			}
			if plan.http != tc.wantHTTP {
				t.Errorf("plan.http = %v, want %v", plan.http, tc.wantHTTP)
			}
			if plan.warnAllowLAN != tc.wantWarnLAN {
				t.Errorf("plan.warnAllowLAN = %v, want %v", plan.warnAllowLAN, tc.wantWarnLAN)
			}
		})
	}
}

// TestMcpUnknownArg verifies a stray positional argument is a usage error.
func TestMcpUnknownArg(t *testing.T) {
	var out, errb bytes.Buffer
	code := Run([]string{"mcp", "--dir", t.TempDir(), "extra"}, &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2; stderr: %s", code, errb.String())
	}
}

// TestMcpBadFlag verifies an unknown flag is a usage error (exit 2), surfaced
// by the flag package before any serve is attempted.
func TestMcpBadFlag(t *testing.T) {
	var out, errb bytes.Buffer
	code := Run([]string{"mcp", "--nope"}, &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2; stderr: %s", code, errb.String())
	}
}
