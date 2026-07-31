package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveHubTarget covers the --hub / --hub-public-url / --hub-token-file /
// SCRIM_PUSH_TOKEN resolution matrix without starting a server: local mode, env
// token, file token (overriding env), the fail-closed no-token case, the
// token-file-without-hub warning, and the link-only public base.
func TestResolveHubTarget(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("  file-token\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	t.Run("local mode: no hub", func(t *testing.T) {
		target, warn, err := resolveHubTarget("", "", "", "")
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
		target, warn, err := resolveHubTarget("", "", tokenFile, "")
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
		target, warn, err := resolveHubTarget("https://hub.example", "", "", "env-token")
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
		target, _, err := resolveHubTarget("https://hub.example", "", tokenFile, "env-token")
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if target == nil || target.Token != "file-token" {
			t.Errorf("target token = %v, want file-token (file overrides env, trimmed)", target)
		}
	})

	t.Run("hub without any token fails closed", func(t *testing.T) {
		target, _, err := resolveHubTarget("https://hub.example", "", "", "")
		if err == nil {
			t.Fatal("err = nil, want a fail-closed error when --hub has no token")
		}
		if target != nil {
			t.Errorf("target = %+v, want nil on error", target)
		}
	})

	t.Run("hub with unreadable token file errors", func(t *testing.T) {
		_, _, err := resolveHubTarget("https://hub.example", "", filepath.Join(t.TempDir(), "missing"), "env-token")
		if err == nil {
			t.Fatal("err = nil, want an error for an unreadable token file")
		}
	})

	t.Run("no public url leaves the link base empty", func(t *testing.T) {
		target, _, err := resolveHubTarget("https://hub.example", "", "", "env-token")
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if target == nil || target.PublicBaseURL != "" {
			t.Errorf("target = %+v, want an empty PublicBaseURL (links built from --hub)", target)
		}
	})

	t.Run("public url is carried through without touching the API base", func(t *testing.T) {
		target, _, err := resolveHubTarget("http://scrim-hub.internal:7788", "https://scrim.example", "", "env-token")
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if target == nil {
			t.Fatal("target = nil, want hub mode")
		}
		if target.BaseURL != "http://scrim-hub.internal:7788" {
			t.Errorf("BaseURL = %q, want the --hub value unchanged", target.BaseURL)
		}
		if target.PublicBaseURL != "https://scrim.example" {
			t.Errorf("PublicBaseURL = %q, want https://scrim.example", target.PublicBaseURL)
		}
	})

	t.Run("public url without hub is ignored (local mode)", func(t *testing.T) {
		target, warn, err := resolveHubTarget("", "https://scrim.example", "", "")
		if err != nil {
			t.Fatalf("err = %v, want nil (a no-op flag warns, it is not a usage error)", err)
		}
		if target != nil {
			t.Errorf("target = %+v, want nil (local mode)", target)
		}
		if warn {
			t.Error("warn = true, want false (that bool is the token-file warning)")
		}
	})
}

// TestOrEnvFlagWinsOverEnv pins the resolution --hub-public-url shares with the
// OAuth flags: a non-empty flag wins, an empty flag falls back to the env var,
// and neither set resolves to empty.
func TestOrEnvFlagWinsOverEnv(t *testing.T) {
	t.Setenv(hubPublicURLEnv, "https://from-env.example")
	if got := orEnv("https://from-flag.example", hubPublicURLEnv); got != "https://from-flag.example" {
		t.Errorf("orEnv(flag, env) = %q, want the flag value", got)
	}
	if got := orEnv("", hubPublicURLEnv); got != "https://from-env.example" {
		t.Errorf("orEnv(\"\", env) = %q, want the env value", got)
	}
	t.Setenv(hubPublicURLEnv, "")
	if got := orEnv("", hubPublicURLEnv); got != "" {
		t.Errorf("orEnv(\"\", unset env) = %q, want empty", got)
	}
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
