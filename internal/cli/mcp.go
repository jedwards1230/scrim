package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/jedwards1230/scrim/internal/mcpserver"
	"github.com/jedwards1230/scrim/internal/version"
)

// hubTokenEnv is the environment variable the hub push token is read from when
// --hub-token-file isn't given. It's the same token `scrim push` and the hub
// itself use.
const hubTokenEnv = "SCRIM_PUSH_TOKEN" //nolint:gosec // G101: env var name, not a hardcoded credential

// OAuth resource-mode env vars, each a fallback for the matching --oauth-*
// flag (the flag wins when both are set). Setting the issuer turns OAuth on;
// the audience is then required (see mcpserver.OAuthConfig.Validate).
const (
	oauthIssuerEnv   = "SCRIM_MCP_OAUTH_ISSUER"
	oauthAudienceEnv = "SCRIM_MCP_OAUTH_AUDIENCE"
	oauthResourceEnv = "SCRIM_MCP_OAUTH_RESOURCE"
)

// cmdMcp implements `scrim mcp [--http ADDR] [--allow-lan] [--hub URL]
// [--hub-token-file PATH] [--oauth-issuer URL] [--oauth-audience AUD]
// [--oauth-resource URL]`. It runs an MCP server exposing scrim's verbs as
// tools. The default transport is stdio (stdout is the MCP protocol channel);
// --http switches to streamable HTTP. The default mode is local (drive the
// local daemon + on-disk canvases); --hub drives a remote hub's machine API
// over HTTP instead. Transport (--http) and mode (--hub) are orthogonal — all
// four combinations are valid.
//
// The HTTP transport is unauthenticated by default (it binds loopback and
// refuses a non-loopback bind unless --allow-lan opts in), UNLESS OAuth is
// configured (--oauth-issuer / SCRIM_MCP_OAUTH_ISSUER): OAuth turns /mcp into
// an RFC 9728 protected resource that validates a bearer JWT per request, so a
// non-loopback bind no longer needs --allow-lan (the endpoint is authenticated).
// Hub mode requires a push token (from SCRIM_PUSH_TOKEN or --hub-token-file)
// and fails closed without one. OAuth (client connection) and the CF
// header-trust plane (SCRIM_MCP_IDENTITY_HMAC_SECRET, end-user attribution) are
// orthogonal layers that may be configured together or independently.
func cmdMcp(args []string, _, stderr io.Writer) int {
	fs := newFlagSet("mcp", stderr)
	cf := registerCommonFlags(fs)
	httpAddr := fs.String("http", "", "serve streamable-HTTP MCP on this addr (e.g. 127.0.0.1:7799); default empty = stdio")
	allowLAN := fs.Bool("allow-lan", false, "allow a non-loopback --http bind despite the endpoint being unauthenticated (not needed when OAuth is configured)")
	hubURL := fs.String("hub", "", "drive a remote scrim hub's machine API over HTTP instead of the local daemon (e.g. https://scrim-hub.example); default empty = local mode")
	hubTokenFile := fs.String("hub-token-file", "", "read the hub push token from this file (overrides SCRIM_PUSH_TOKEN); only meaningful with --hub")
	oauthIssuer := fs.String("oauth-issuer", "", "OIDC/OAuth authorization server issuer URL; setting it makes --http an RFC 9728 protected resource (or set "+oauthIssuerEnv+")")
	oauthAudience := fs.String("oauth-audience", "", "expected token audience (the scrim MCP resource id); required with --oauth-issuer (or set "+oauthAudienceEnv+")")
	oauthResource := fs.String("oauth-resource", "", "canonical resource URL advertised in protected-resource metadata; default derived from the request (or set "+oauthResourceEnv+")")
	if err := parseArgs(fs, args); err != nil {
		return exitForParseErr(err)
	}
	if fs.NArg() != 0 {
		return usageError(stderr, "usage: scrim mcp [--http ADDR] [--allow-lan] [--hub URL] [--hub-token-file PATH] [--oauth-issuer URL] [--oauth-audience AUD] [--oauth-resource URL]")
	}

	// Resolve OAuth config (flag wins over env) and fail closed on a
	// misconfiguration (issuer without audience) before anything binds.
	oauthCfg := mcpserver.OAuthConfig{
		Issuer:   orEnv(*oauthIssuer, oauthIssuerEnv),
		Audience: orEnv(*oauthAudience, oauthAudienceEnv),
		Resource: orEnv(*oauthResource, oauthResourceEnv),
	}
	if err := oauthCfg.Validate(); err != nil {
		return usageError(stderr, "scrim mcp: %s", err.Error())
	}

	plan, err := planMcp(*httpAddr, *allowLAN, oauthCfg.Enabled())
	if err != nil {
		return usageError(stderr, "%s", err.Error())
	}
	if plan.warnAllowLAN {
		outf(stderr, "scrim mcp: warning: --allow-lan has no effect without --http (stdio has no network bind to gate)\n")
	}
	// OAuth only guards the HTTP transport; on stdio it's inert (no inbound
	// request to carry a bearer), a no-op worth a one-line warning.
	if oauthCfg.Enabled() && !plan.http {
		outf(stderr, "scrim mcp: warning: --oauth-issuer has no effect without --http (stdio carries no bearer to validate)\n")
	}

	hub, warnTokenFileNoHub, err := resolveHubTarget(*hubURL, *hubTokenFile, os.Getenv(hubTokenEnv))
	if err != nil {
		return usageError(stderr, "%s", err.Error())
	}
	if warnTokenFileNoHub {
		outf(stderr, "scrim mcp: warning: --hub-token-file has no effect without --hub (local mode uses no hub token)\n")
	}
	if hub != nil && hubBearerInsecure(hub.BaseURL) {
		outf(stderr, "scrim mcp: warning: --hub uses plain http to a non-loopback host — the push token is sent unencrypted; prefer https\n")
	}
	// The CF identity plane only reaches this server over the streamable-HTTP
	// transport (stdio carries no inbound headers). When that transport is used
	// in hub mode WITHOUT the shared HMAC secret set, X-Forwarded-User-* identity
	// is not verified and every call is attributed to the hub's admin push token
	// alone -- a deliberately fail-closed default worth a one-line diagnostic.
	if plan.http && hub != nil && os.Getenv(mcpserver.IdentitySecretEnv) == "" {
		outf(stderr, "scrim mcp: note: %s is unset — forwarded-user identity is not verified; all calls are attributed to the hub admin token\n", mcpserver.IdentitySecretEnv)
	}

	cfg := cf.toConfig()

	// A signal-cancellable context so Ctrl-C (SIGINT) or SIGTERM stops the
	// server cleanly, mirroring cli.cmdPush.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if plan.http {
		return serveResult(mcpserver.ServeHTTP(ctx, *httpAddr, cfg, version.Short(), hub, oauthCfg, stderr), stderr)
	}
	return serveResult(mcpserver.Serve(ctx, cfg, version.Short(), hub, stderr), stderr)
}

// orEnv returns the flag value when non-empty, else the named env var. It is
// the flag-wins-over-env resolution the OAuth flags share.
func orEnv(flagVal, envKey string) string {
	if flagVal != "" {
		return flagVal
	}
	return os.Getenv(envKey)
}

// resolveHubTarget derives the hub-mode selection from the --hub URL, the
// --hub-token-file path, and the ambient SCRIM_PUSH_TOKEN value. It returns:
//   - a nil target for local mode (no --hub), plus warnTokenFileNoHub=true when
//     --hub-token-file was pointlessly given without --hub (a no-op worth a
//     warning, not a hard error — mirroring --allow-lan-without-http);
//   - a populated target for hub mode, with the token resolved from the file
//     (which overrides the env) or else the env;
//   - a fail-closed error when --hub is set but no token resolves.
//
// The only I/O is reading --hub-token-file when set, so tests exercise every
// branch with a temp file (or none).
func resolveHubTarget(hubURL, tokenFile, envToken string) (*mcpserver.HubTarget, bool, error) {
	if hubURL == "" {
		return nil, tokenFile != "", nil
	}

	token := strings.TrimSpace(envToken)
	if tokenFile != "" {
		data, err := os.ReadFile(tokenFile) //nolint:gosec // G304: tokenFile is an operator-supplied --hub-token-file path, not untrusted input
		if err != nil {
			return nil, false, fmt.Errorf("reading --hub-token-file: %w", err)
		}
		token = strings.TrimSpace(string(data))
	}
	if token == "" {
		return nil, false, fmt.Errorf(
			"scrim mcp --hub requires a push token, but none was found "+
				"(set %s or pass --hub-token-file PATH); refusing to start hub mode without one", hubTokenEnv)
	}
	return &mcpserver.HubTarget{BaseURL: hubURL, Token: token}, false, nil
}

// hubBearerInsecure reports whether the hub base URL would send the bearer
// token in cleartext to a non-loopback host (plain http off-machine). Loopback
// http is fine — it never leaves the host.
func hubBearerInsecure(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme != "http" {
		return false // https (or unparseable, which fails later anyway)
	}
	host := u.Hostname()
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return false
	}
	return true
}

// mcpPlan is the decision planMcp derives from the mcp verb's transport flags,
// separated out so the flag/gate logic is unit-testable without starting a
// blocking server.
type mcpPlan struct {
	// http is true when the streamable-HTTP transport was requested (--http).
	http bool
	// warnAllowLAN is true when --allow-lan was passed without --http (a no-op
	// worth a stderr warning, not a hard error).
	warnAllowLAN bool
}

// planMcp validates the transport flags and returns the resulting plan, or a
// usage error when a non-loopback --http bind is requested without --allow-lan
// AND without OAuth. The LAN gate exists only because an unauthenticated
// endpoint should not silently become LAN-reachable; when oauthEnabled is true
// the endpoint validates a bearer per request, so the gate no longer applies
// and a non-loopback bind is accepted. It performs no I/O, so tests can
// exercise every branch without binding a listener.
func planMcp(httpAddr string, allowLAN, oauthEnabled bool) (mcpPlan, error) {
	if httpAddr == "" {
		return mcpPlan{http: false, warnAllowLAN: allowLAN}, nil
	}
	if !mcpserver.IsLoopbackAddr(httpAddr) && !allowLAN && !oauthEnabled {
		return mcpPlan{}, fmt.Errorf(
			"scrim mcp --http %s binds a non-loopback address (a bare :PORT binds every interface), "+
				"but the HTTP MCP endpoint is UNAUTHENTICATED. "+
				"Pass --allow-lan to accept an unauthenticated LAN-reachable server, "+
				"configure OAuth (--oauth-issuer) to authenticate it, "+
				"or bind loopback instead (e.g. 127.0.0.1%s)",
			httpAddr, portSuffix(httpAddr))
	}
	return mcpPlan{http: true}, nil
}

// serveResult maps a server's exit to a process exit code: 0 for a clean stop
// (nil, or a ctx-cancelled shutdown that surfaces as nil or context.Canceled),
// 1 for a genuine serve error — which it prints to stderr first, like every
// other verb, so a failed bind or serve isn't silently swallowed behind a bare
// exit code.
func serveResult(err error, stderr io.Writer) int {
	if err == nil || errors.Is(err, context.Canceled) {
		return 0
	}
	errOut(stderr, err)
	return 1
}

// portSuffix returns the ":PORT" tail of addr for the loopback suggestion in
// the refusal message, or ":"+addr when addr has no ':'. Cosmetic — a
// best-effort hint, never load-bearing.
func portSuffix(addr string) string {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[i:]
		}
	}
	return ":" + addr
}
