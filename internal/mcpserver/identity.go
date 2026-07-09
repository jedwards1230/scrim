package mcpserver

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"os"
	"strings"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// IdentitySecretEnv is the environment variable carrying the shared HMAC secret
// scrim mcp verifies X-Forwarded-User-* identity headers with. An empty/unset
// secret disables identity trust entirely: every request is anonymous, so the
// hub attributes it to the admin push token alone (no per-request actor). This
// is the fail-closed default -- a misconfigured deployment forwards NO identity
// rather than a spoofable one.
const IdentitySecretEnv = "SCRIM_MCP_IDENTITY_HMAC_SECRET"

// Forwarded-identity request headers (ContextForge → scrim-mcp). The CF gateway
// authenticates the claude.ai/agent user and forwards the resulting principal
// as these headers, signed with the shared secret so scrim-mcp can verify the
// gateway -- and only the gateway -- set them.
const (
	hdrFwdUserID     = "X-Forwarded-User-Id"
	hdrFwdUserEmail  = "X-Forwarded-User-Email"
	hdrFwdUserGroups = "X-Forwarded-User-Groups"
	hdrFwdUserSig    = "X-Forwarded-User-Signature"
)

// Actor attribution headers (scrim-mcp → hub). Once scrim-mcp has VERIFIED the
// forwarded identity above, it re-emits the principal to the hub as these
// headers, on top of the admin push-token bearer. The hub trusts them ONLY
// because they ride a valid admin bearer (see internal/server/hubgate.go's
// resolveClaims) -- this hop is scrim-mcp asserting "I verified this actor",
// distinct from the CF→mcp hop above which scrim-mcp must itself verify.
const (
	hdrActorID     = "X-Scrim-Actor-Id"
	hdrActorEmail  = "X-Scrim-Actor-Email"
	hdrActorGroups = "X-Scrim-Actor-Groups"
)

// actor is the verified CF-forwarded principal a single tool call acts as. It
// is mcpserver's OWN small identity type -- deliberately not internal/server's
// identity.Claims -- so this package keeps no dependency on internal/server.
// The zero value is the anonymous principal (no verified identity).
type actor struct {
	ID     string
	Email  string
	Groups []string
}

// actorCtxKey is the private context key under which a handler stashes the
// verified actor for the backend to read (hubBackend re-emits it as
// X-Scrim-Actor-* headers; localBackend ignores it).
type actorCtxKey struct{}

// ctxWithActor returns ctx carrying a. Handlers call it after verifying the
// inbound identity headers so the backend can attribute the call.
func ctxWithActor(ctx context.Context, a actor) context.Context {
	return context.WithValue(ctx, actorCtxKey{}, a)
}

// actorFromContext returns the verified actor a handler stashed, and whether one
// was present. hubBackend reads it to set the X-Scrim-Actor-* attribution
// headers on its outgoing hub requests.
func actorFromContext(ctx context.Context) (actor, bool) {
	a, ok := ctx.Value(actorCtxKey{}).(actor)
	return a, ok
}

// identitySecretFromEnv reads the shared HMAC secret from the environment once,
// at server construction. Kept isolated so the single read point is obvious.
func identitySecretFromEnv() string {
	return os.Getenv(IdentitySecretEnv)
}

// canonicalIdentityString builds the EXACT byte string the HMAC signature is
// computed over, from the three forwarded identity values. This is the single
// canonicalization point for the CF→scrim-mcp identity hop: a leading versioned
// domain tag binds the signature to this scheme (so a signature can never be
// replayed under a different one), then the id, email, and the raw groups
// header value each on their own line.
//
// ASSUMPTION / #48 homelab task: the exact header NAMES (X-Forwarded-User-*)
// and this canonicalization MUST be reconciled against ContextForge v1.0.4's
// IDENTITY_SIGN_CLAIMS output before homelab enablement. Here we ship a
// defined, self-consistent scheme with the canonicalization isolated in this
// one function; aligning the wire format with CF is a configuration/adapter
// task, not a redesign of the verification below.
func canonicalIdentityString(id, email, groups string) string {
	// groups is passed through verbatim (the raw comma-separated header value)
	// so signer and verifier canonicalize identically without agreeing on any
	// group ordering/whitespace normalization.
	return "scrim-forwarded-identity-v1\n" + id + "\n" + email + "\n" + groups
}

// verifyForwardedIdentity verifies the HMAC-signed X-Forwarded-User-* headers on
// h against secret and, when valid, returns the trusted actor. It is the sole
// trust gate for the ContextForge identity plane: a missing secret, a missing
// signature, or a signature that fails the constant-time compare all yield
// (zero actor, false) -- the caller then treats the request as anonymous. There
// is deliberately no partial trust: identity is all-or-nothing per request.
func verifyForwardedIdentity(h http.Header, secret string) (actor, bool) {
	if secret == "" || h == nil {
		return actor{}, false
	}
	sig := h.Get(hdrFwdUserSig)
	if sig == "" {
		return actor{}, false
	}
	id := h.Get(hdrFwdUserID)
	email := h.Get(hdrFwdUserEmail)
	groups := h.Get(hdrFwdUserGroups)

	presented, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(sig, "="))
	if err != nil {
		return actor{}, false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(canonicalIdentityString(id, email, groups)))
	want := mac.Sum(nil)
	// hmac.Equal is constant-time and length-aware.
	if !hmac.Equal(presented, want) {
		return actor{}, false
	}
	return actor{ID: id, Email: email, Groups: parseGroups(groups)}, true
}

// signForwardedIdentity produces the base64url (unpadded) signature a gateway
// would set as X-Forwarded-User-Signature for the given identity values. It is
// the inverse of verifyForwardedIdentity's check and exists so tests (and, at
// the homelab layer, a CF adapter reference) can mint a valid signature; the
// production verify path never calls it.
func signForwardedIdentity(id, email, groups, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(canonicalIdentityString(id, email, groups)))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// parseGroups splits a comma-separated groups header value into a trimmed,
// empty-free slice (nil when there are none).
func parseGroups(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if g := strings.TrimSpace(p); g != "" {
			out = append(out, g)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// actorContext extracts and verifies the CF-forwarded identity from req's inbound
// HTTP headers (populated by the streamable-HTTP transport; nil on stdio and in
// unit tests) and, when trusted, returns ctx carrying it. An unverified/absent
// identity leaves ctx unchanged -- the call proceeds anonymously and the hub
// attributes it to the admin push token alone.
//
// Two-sided defense: this HMAC verification is one half. The other is a
// NetworkPolicy that pins scrim-mcp's ingress to the ContextForge gateway, so a
// pod that can't reach scrim-mcp can't present forged headers in the first
// place. Neither is sufficient alone (a compromised gateway, or a secret leak);
// together they bound the CF identity plane's trust to "the gateway, holding the
// shared secret, on the allowed network path" -- see #48/#51.
func (s *server) actorContext(ctx context.Context, req *mcp.CallToolRequest) context.Context {
	if req == nil || req.Extra == nil {
		return ctx
	}
	a, ok := verifyForwardedIdentity(req.Extra.Header, s.identitySecret)
	if !ok {
		return ctx
	}
	return ctxWithActor(ctx, a)
}
