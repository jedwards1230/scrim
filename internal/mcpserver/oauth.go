package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	coreoidc "github.com/coreos/go-oidc/v3/oidc"
)

// OAuth 2.0 protected-resource support for the streamable-HTTP transport
// (scrim#33). This is a SECOND, orthogonal identity layer to identity.go's CF
// header-trust plane:
//
//   - OAuth (here) authenticates the CLIENT CONNECTION: a remote MCP host
//     (claude.ai, another agent runtime) presents an OAuth bearer JWT minted by
//     an OIDC/OAuth authorization server (Authentik is the reference AS); scrim
//     validates its signature/issuer/audience/expiry against the AS's JWKS and
//     enforces per-tool read/write scopes.
//   - CF header-trust (identity.go) attributes the END USER behind a trusted
//     gateway via HMAC-signed X-Forwarded-User-* headers.
//
// The two compose: a deployment may run behind ContextForge (CF plane) AND
// require an OAuth bearer (this plane), or either alone. When OAuth is
// unconfigured the transport behaves exactly as before -- no metadata endpoint,
// no bearer requirement. The stdio transport carries no HTTP request and is
// never touched by this layer (local trust).

const (
	// scopeRead / scopeWrite are the two OAuth scopes the resource advertises
	// and enforces: read tools (list/read/status/...) need scopeRead, mutating
	// tools (add/write/edit/share/push/...) need scopeWrite. A write-scoped
	// token also satisfies a read requirement (write strictly dominates read);
	// the reverse does not hold.
	scopeRead  = "scrim:read"
	scopeWrite = "scrim:write"

	// protectedResourceMetadataPath is the RFC 9728 well-known location the
	// resource serves its protected-resource metadata at, UNAUTHENTICATED, so a
	// client can discover the authorization server before it holds any token.
	protectedResourceMetadataPath = "/.well-known/oauth-protected-resource"

	// maxRPCPeekBytes bounds how much of a POST /mcp body the scope gate buffers
	// to read the JSON-RPC method + tool name. The largest legitimate tools/call
	// is a write_file whose content is capped at 2 MiB (maxFileBytes); even
	// heavily JSON-escaped or gzip+base64 that stays well under this ceiling, so
	// a body larger than this is never a valid call and is refused 413 rather
	// than parsed -- a fail-closed request-size guard that also bounds the buffer.
	maxRPCPeekBytes = 8 * 1024 * 1024

	// oauthDiscoveryTimeout bounds the one-shot OIDC discovery at startup so a
	// hung AS cannot pin server bring-up open indefinitely (mirrors oidc.New).
	oauthDiscoveryTimeout = 15 * time.Second
)

// OAuthConfig is the resolved OAuth protected-resource configuration, populated
// from scrim mcp's flags/env (see internal/cli). A zero value (empty Issuer)
// disables OAuth entirely -- the transport then requires no bearer.
type OAuthConfig struct {
	// Issuer is the OIDC/OAuth authorization server's issuer URL. Its
	// /.well-known/openid-configuration is fetched once at startup to discover
	// the JWKS endpoint. A non-empty Issuer turns OAuth resource mode ON.
	Issuer string
	// Audience is the expected `aud` claim -- the scrim MCP resource identifier
	// the AS mints tokens for. Required whenever Issuer is set (a resource that
	// doesn't pin its audience would accept any token the AS ever issued, for
	// any resource). Validated as the coreoidc verifier's ClientID.
	Audience string
	// Resource is the canonical resource URL advertised in the protected-resource
	// metadata and pointed at by the WWW-Authenticate resource_metadata parameter.
	// Optional: when empty it is derived per-request from the inbound
	// scheme+host (honoring X-Forwarded-Proto behind a TLS-terminating proxy),
	// which is correct for a direct bind; set it explicitly when the external URL
	// cannot be derived from the request (the oidc.RedirectURL precedent).
	Resource string
}

// Enabled reports whether OAuth resource mode is on (an Issuer was configured).
func (c OAuthConfig) Enabled() bool { return c.Issuer != "" }

// Validate fails closed on a misconfiguration: an Issuer with no Audience would
// leave the resource accepting any of the AS's tokens. A disabled config (no
// Issuer) is always valid; a stray Resource without an Issuer is a harmless
// no-op left for the CLI to warn about, not a hard error here.
func (c OAuthConfig) Validate() error {
	if c.Issuer == "" {
		return nil
	}
	if c.Audience == "" {
		return fmt.Errorf("oauth issuer is set but audience is missing (set the expected token aud); refusing to start an OAuth resource that pins no audience")
	}
	if c.Resource != "" {
		if u, err := url.Parse(c.Resource); err != nil {
			return fmt.Errorf("oauth resource URL %q is invalid: %w", c.Resource, err)
		} else if !u.IsAbs() || u.Host == "" {
			return fmt.Errorf("oauth resource URL %q must be absolute with a host (e.g. https://scrim-mcp.example)", c.Resource)
		}
	}
	return nil
}

// oauthValidator is the runtime OAuth gate: the resolved config plus the
// coreoidc verifier (signature via the AS's JWKS, issuer, audience, and expiry).
// It is safe for concurrent use -- the verifier's RemoteKeySet caches and
// refreshes JWKS internally.
type oauthValidator struct {
	cfg      OAuthConfig
	verifier *coreoidc.IDTokenVerifier
}

// newOAuthValidator validates cfg and performs OIDC discovery against the issuer
// so a bad issuer fails FAST at startup (before the listener binds), exactly
// like oidc.New. Discovery is bounded by a short timeout derived from ctx: a
// hung AS cannot hang bring-up. Once running, the verifier fetches JWKS lazily
// (with its own caching), so a transient network blip degrades a single
// request's validation rather than crashing the server.
func newOAuthValidator(ctx context.Context, cfg OAuthConfig) (*oauthValidator, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	discCtx, cancel := context.WithTimeout(ctx, oauthDiscoveryTimeout)
	defer cancel()
	provider, err := coreoidc.NewProvider(discCtx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oauth discovery against issuer failed: %w", err)
	}
	return &oauthValidator{
		cfg: cfg,
		// ClientID is the coreoidc name for the required `aud` value; setting it
		// makes Verify reject any token not audienced to this resource.
		verifier: provider.Verifier(&coreoidc.Config{ClientID: cfg.Audience}),
	}, nil
}

// protectedResourceMetadata is the RFC 9728 document shape scrim serves at
// protectedResourceMetadataPath. bearer_methods_supported is ["header"]: scrim
// reads the token only from the Authorization header, never a query string
// (which would leak into logs and referrers).
type protectedResourceMetadata struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers"`
	ScopesSupported        []string `json:"scopes_supported"`
	BearerMethodsSupported []string `json:"bearer_methods_supported"`
}

// handleMetadata serves the protected-resource metadata UNAUTHENTICATED (200):
// it is the discovery document a client fetches to learn which authorization
// server to obtain a token from, so it must be reachable with no token.
func (o *oauthValidator) handleMetadata(w http.ResponseWriter, r *http.Request) {
	meta := protectedResourceMetadata{
		Resource:               o.resourceBase(r),
		AuthorizationServers:   []string{o.cfg.Issuer},
		ScopesSupported:        []string{scopeRead, scopeWrite},
		BearerMethodsSupported: []string{"header"},
	}
	w.Header().Set("Content-Type", "application/json")
	// A marshal failure on this tiny fixed struct is not reachable in practice;
	// encode directly to the response and ignore the write error like the
	// health handler does (the client simply sees a truncated body).
	_ = json.NewEncoder(w).Encode(meta)
}

// resourceBase returns the canonical resource URL: the configured Resource when
// set, else derived from the request's scheme+host. Behind a TLS-terminating
// proxy the scheme is taken from X-Forwarded-Proto (the proxy is trusted for
// this hint; the actual security boundary is JWT validation, not this URL).
func (o *oauthValidator) resourceBase(r *http.Request) string {
	if o.cfg.Resource != "" {
		return strings.TrimRight(o.cfg.Resource, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if xf := r.Header.Get("X-Forwarded-Proto"); xf != "" {
		scheme = xf
	}
	return scheme + "://" + r.Host
}

// metadataURL is the absolute URL of the protected-resource metadata document,
// used both as the WWW-Authenticate resource_metadata pointer and as what a
// client GETs for discovery.
func (o *oauthValidator) metadataURL(r *http.Request) string {
	return o.resourceBase(r) + protectedResourceMetadataPath
}

// middleware wraps the /mcp handler with OAuth validation. Every request must
// carry a valid bearer (signature/issuer/audience/expiry); a tools/call must
// additionally carry the tool's required scope. Authentication failures are 401
// and scope failures are 403, each with an RFC 6750 / RFC 9728 WWW-Authenticate
// challenge pointing at the metadata document.
//
// Scope is enforced at THIS HTTP layer rather than inside each tool handler for
// one reason: only here can a scope failure become a real HTTP 403 carrying a
// WWW-Authenticate: Bearer error="insufficient_scope" challenge (a tool-handler
// error surfaces as a JSON-RPC/tool-result error with a 200 status, which is
// not an OAuth challenge). The tool name is read from the JSON-RPC body and
// mapped to a required scope (requiredScope) -- per-tool granularity, enforced
// at the transport edge. Non-tools/call methods (initialize, tools/list, ping)
// require only a valid token so a read-only client can still handshake.
//
// The body may be a SINGLE JSON-RPC message OR a batch array (the go-sdk accepts
// batches whenever the negotiated protocol version predates 2025-06-18, which is
// the default when the client sends no MCP-Protocol-Version header). A batch is
// gated by the most-privileged scope any of its tools/call elements requires --
// so a scrim:read token cannot smuggle a write tool inside a one-element array.
// A present-but-unparseable body fails CLOSED (requires scopeWrite) rather than
// slipping through the gate; see requiredScopeForBody.
func (o *oauthValidator) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		metaURL := o.metadataURL(r)

		token, ok := bearerToken(r)
		if !ok {
			// No credentials presented: a bare challenge (no error code) inviting
			// the client to discover the AS and retry with a token.
			o.challenge(w, http.StatusUnauthorized, metaURL, "", "", "missing bearer token")
			return
		}
		idt, err := o.verifier.Verify(r.Context(), token)
		if err != nil {
			// Signature/issuer/audience/expiry all funnel here as an invalid token.
			// The error is deliberately not surfaced to the client or logged --
			// it can echo token contents.
			o.challenge(w, http.StatusUnauthorized, metaURL, "invalid_token", "", "invalid or expired bearer token")
			return
		}

		// Derive per-user attribution from the now-validated token and stash it in
		// the request context. Because the token has been independently verified
		// (signature/issuer/audience/expiry), this actor is AUTHORITATIVE over the
		// separate HMAC X-Forwarded-User-* header plane -- actorContext promotes it
		// to the final actor the hub is attributed to.
		//
		// Guard on a non-empty subject: coreoidc verifies sig/iss/aud/exp but does
		// NOT enforce `sub` presence, so a valid-but-sub-less token yields an actor
		// with an empty ID. Since OAuth precedence is absolute, stashing that empty
		// actor would SHADOW a valid HMAC actor and emit blank X-Scrim-Actor-*
		// headers -- an attribution downgrade, not a fallback. Only stashing when a
		// subject is present keeps "OAuth wins" strictly an UPGRADE: a sub-less
		// token falls through to the HMAC plane (or anonymous), fail-closed.
		if a := actorFromToken(idt); a.ID != "" {
			r = r.WithContext(ctxWithOAuthActor(r.Context(), a))
		}

		// Scope gate: derive the single scope this request requires -- the
		// most-privileged across every tools/call in the (possibly batched)
		// JSON-RPC body -- and check the already-verified token holds it.
		// requiredScopeForBody restores the body so the SDK reads it intact.
		need, tooLarge := requiredScopeForBody(r)
		if tooLarge {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		if need != "" && !satisfiesScope(tokenScopeClaim(idt), need) {
			o.challenge(w, http.StatusForbidden, metaURL, "insufficient_scope", need, "insufficient scope")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// challenge writes an RFC 6750 WWW-Authenticate: Bearer challenge and the given
// status. Parameters are emitted in a fixed order (error, scope,
// resource_metadata) so the header is deterministic for tests; the human body
// is a short, token-free message.
func (o *oauthValidator) challenge(w http.ResponseWriter, status int, metaURL, errCode, scope, msg string) {
	parts := make([]string, 0, 3)
	if errCode != "" {
		parts = append(parts, fmt.Sprintf("error=%q", errCode))
	}
	if scope != "" {
		parts = append(parts, fmt.Sprintf("scope=%q", scope))
	}
	parts = append(parts, fmt.Sprintf("resource_metadata=%q", metaURL))
	w.Header().Set("WWW-Authenticate", "Bearer "+strings.Join(parts, ", "))
	http.Error(w, msg, status)
}

// bearerToken extracts the token from an "Authorization: Bearer <token>" header.
// It returns ok=false for a missing, malformed, or non-Bearer scheme so the
// caller issues the no-credentials challenge.
func bearerToken(r *http.Request) (string, bool) {
	fields := strings.Fields(r.Header.Get("Authorization"))
	if len(fields) != 2 || !strings.EqualFold(fields[0], "bearer") {
		return "", false
	}
	return fields[1], true
}

// tokenScopeClaim reads a verified token's `scope` claim as a space-delimited
// list (RFC 8693 / OAuth). strings.Fields drops empty entries and any whitespace
// run. A claims-decode error yields no scopes (deny by default).
func tokenScopeClaim(idt *coreoidc.IDToken) []string {
	var c struct {
		Scope string `json:"scope"`
	}
	if err := idt.Claims(&c); err != nil {
		return nil
	}
	return strings.Fields(c.Scope)
}

// actorFromToken derives the attribution actor from an ALREADY-VALIDATED OAuth
// token. The subject (`sub`) claim is the stable principal identifier and is
// read directly from the verified IDToken's exported Subject field (never
// re-decoded). Email and groups are a best-effort enrichment decoded from the
// remaining claims and each yielded INDEPENDENTLY: a decode failure on one never
// costs the other, and never fails the request -- the token has already
// validated -- so at worst the actor is keyed by subject with an empty email
// and nil groups.
func actorFromToken(idt *coreoidc.IDToken) actor {
	a := actor{ID: idt.Subject}
	// Decode email on its own single-field struct so an exotic `groups` shape
	// (e.g. a number array or an object) can never fail the unmarshal and drop a
	// perfectly valid email alongside it.
	var ec struct {
		Email string `json:"email"`
	}
	if idt.Claims(&ec) == nil {
		a.Email = ec.Email
	}
	a.Groups = tokenGroupsClaim(idt)
	return a
}

// tokenGroupsClaim extracts the `groups` claim from a validated token. The
// standard OIDC shape is a JSON array; a deployment that instead emits it as a
// delimited string is handled defensively (the array decode fails, so a second
// pass splits the string via parseGroups). Any other JSON shape (a number array,
// an object, ...) fails both decodes and yields nil -- attribution degrades to
// no groups rather than dropping the actor. An empty or absent claim leaves
// Groups nil.
func tokenGroupsClaim(idt *coreoidc.IDToken) []string {
	var arr struct {
		Groups []string `json:"groups"`
	}
	if idt.Claims(&arr) == nil && len(arr.Groups) > 0 {
		return arr.Groups
	}
	var str struct {
		Groups string `json:"groups"`
	}
	if idt.Claims(&str) == nil {
		return parseGroups(str.Groups)
	}
	return nil
}

// toolScopes maps each MCP tool to the scope its invocation requires. Read tools
// (pure lookups, no state change) need scopeRead; mutating tools need scopeWrite.
// A tool absent from this map defaults to scopeWrite via requiredScope (fail
// closed: a newly-added tool requires the stronger scope until classified here).
var toolScopes = map[string]string{
	// read
	"list":        scopeRead,
	"link":        scopeRead,
	"status":      scopeRead,
	"snaps":       scopeRead,
	"list_files":  scopeRead,
	"read_file":   scopeRead,
	"list_grants": scopeRead,
	"path":        scopeRead,
	// write
	"add":          scopeWrite,
	"rm":           scopeWrite,
	"snap":         scopeWrite,
	"revert":       scopeWrite,
	"copy_canvas":  scopeWrite,
	"write_file":   scopeWrite,
	"edit_file":    scopeWrite,
	"share_canvas": scopeWrite,
	"push":         scopeWrite,
}

// requiredScope returns the scope a tool call requires, defaulting an unmapped
// tool to scopeWrite (fail closed).
func requiredScope(tool string) string {
	if s, ok := toolScopes[tool]; ok {
		return s
	}
	return scopeWrite
}

// satisfiesScope reports whether a token's scopes satisfy a requirement. A
// scopeWrite grant satisfies a scopeRead requirement (write strictly dominates
// read); every other match is exact.
func satisfiesScope(scopes []string, need string) bool {
	for _, s := range scopes {
		if s == need {
			return true
		}
		if need == scopeRead && s == scopeWrite {
			return true
		}
	}
	return false
}

// requiredScopeForBody determines the scope a POST /mcp body requires WITHOUT
// consuming it: the body is buffered (bounded by maxRPCPeekBytes) and restored
// so the downstream MCP handler reads it intact. The return is "" when the body
// carries no tools/call (initialize/tools-list/ping, or a bodyless/non-POST
// request), scopeRead or scopeWrite for a single call, or the most-privileged
// scope across a JSON-RPC BATCH (array body). It returns tooLarge=true for a
// body over the cap (refused 413 by the caller -- never a valid call).
//
// Fail-closed rules: a present-but-unparseable body -- one that is neither a
// JSON object nor a JSON array, or an array/object that fails to unmarshal --
// requires scopeWrite, so garbage can never slip past the gate as "no scope
// needed". An empty (or whitespace-only) body needs no scope; the SDK rejects it.
func requiredScopeForBody(r *http.Request) (need string, tooLarge bool) {
	if r.Method != http.MethodPost || r.Body == nil {
		return "", false
	}
	buf, err := io.ReadAll(io.LimitReader(r.Body, maxRPCPeekBytes+1))
	if err != nil {
		// A read error: restore whatever was read and let the MCP handler surface
		// the failure; don't gate scope on an unreadable body.
		r.Body = io.NopCloser(bytes.NewReader(buf))
		return "", false
	}
	if len(buf) > maxRPCPeekBytes {
		return "", true
	}
	r.Body = io.NopCloser(bytes.NewReader(buf))

	trimmed := bytes.TrimSpace(buf)
	if len(trimmed) == 0 {
		return "", false
	}
	switch trimmed[0] {
	case '[':
		// JSON-RPC batch: gate by the most-privileged scope any element requires.
		var msgs []json.RawMessage
		if json.Unmarshal(trimmed, &msgs) != nil {
			return scopeWrite, false // unparseable array -> fail closed
		}
		need = ""
		for _, m := range msgs {
			need = maxScope(need, scopeForMessage(m))
		}
		return need, false
	case '{':
		return scopeForMessage(trimmed), false
	default:
		// Present but neither object nor array (a bare string/number/garbage):
		// fail closed.
		return scopeWrite, false
	}
}

// scopeForMessage returns the scope a single JSON-RPC message requires: "" when
// it is not a tools/call, else the mapped scope for the named tool. A message
// that fails to unmarshal fails CLOSED (scopeWrite) -- a malformed element must
// never lower the batch's requirement.
func scopeForMessage(raw []byte) string {
	var msg struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if json.Unmarshal(raw, &msg) != nil {
		return scopeWrite
	}
	if msg.Method != "tools/call" {
		return ""
	}
	return requiredScope(msg.Params.Name)
}

// maxScope returns the more-privileged of two required scopes, ordered
// scopeWrite > scopeRead > "" (no requirement). It folds a batch's per-element
// requirements into the single scope the whole request must satisfy.
func maxScope(a, b string) string {
	if a == scopeWrite || b == scopeWrite {
		return scopeWrite
	}
	if a == scopeRead || b == scopeRead {
		return scopeRead
	}
	return ""
}
