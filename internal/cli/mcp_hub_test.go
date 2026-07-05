package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveHubTarget covers the --hub / --hub-token-file / SCRIM_PUSH_TOKEN
// resolution matrix without starting a server: local mode, env token, file
// token (overriding env), the fail-closed no-token case, and the
// token-file-without-hub warning.
func TestResolveHubTarget(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("  file-token\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	t.Run("local mode: no hub", func(t *testing.T) {
		target, warn, err := resolveHubTarget("", "", "")
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if target != nil {
			t.Errorf("target = %+v, want nil (local mode)", target)
		}
		if warn {
			t.Error("warn = true, want false")
		}
	})

	t.Run("token-file without hub warns", func(t *testing.T) {
		target, warn, err := resolveHubTarget("", tokenFile, "")
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if target != nil {
			t.Errorf("target = %+v, want nil", target)
		}
		if !warn {
			t.Error("warn = false, want true (token file is a no-op without --hub)")
		}
	})

	t.Run("hub with env token", func(t *testing.T) {
		target, warn, err := resolveHubTarget("https://hub.example", "", "env-token")
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if warn {
			t.Error("warn = true, want false")
		}
		if target == nil || target.BaseURL != "https://hub.example" || target.Token != "env-token" {
			t.Errorf("target = %+v, want base https://hub.example token env-token", target)
		}
	})

	t.Run("hub with file token overrides env", func(t *testing.T) {
		target, _, err := resolveHubTarget("https://hub.example", tokenFile, "env-token")
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if target == nil || target.Token != "file-token" {
			t.Errorf("target token = %v, want file-token (file overrides env, trimmed)", target)
		}
	})

	t.Run("hub without any token fails closed", func(t *testing.T) {
		target, _, err := resolveHubTarget("https://hub.example", "", "")
		if err == nil {
			t.Fatal("err = nil, want a fail-closed error when --hub has no token")
		}
		if target != nil {
			t.Errorf("target = %+v, want nil on error", target)
		}
	})

	t.Run("hub with unreadable token file errors", func(t *testing.T) {
		_, _, err := resolveHubTarget("https://hub.example", filepath.Join(t.TempDir(), "missing"), "env-token")
		if err == nil {
			t.Fatal("err = nil, want an error for an unreadable token file")
		}
	})
}

// TestMcpHubWithoutTokenIsUsageError verifies the fail-closed path end-to-end
// through Run: `mcp --hub URL` with no token in the environment is a usage
// error (exit 2) and never starts a server. It clears SCRIM_PUSH_TOKEN for the
// test so an ambient value can't mask the check.
func TestMcpHubWithoutTokenIsUsageError(t *testing.T) {
	t.Setenv("SCRIM_PUSH_TOKEN", "")
	var out, errb bytes.Buffer
	code := Run([]string{"mcp", "--dir", t.TempDir(), "--hub", "https://hub.example"}, &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (usage); stderr: %s", code, errb.String())
	}
	if !strings.Contains(errb.String(), "push token") {
		t.Errorf("stderr = %q, want it to mention the missing push token", errb.String())
	}
}
