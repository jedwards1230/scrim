# Identity: login, ownership & sharing

This covers the hub's identity plane: OIDC login for reads, canvas ownership and
sharing, per-user tokens, the trusted-gateway forwarded-identity mechanism, and
the optional Authentik directory feeder. All of it is hub-only and opt-in — a
localhost-only user never touches any of it.

Design rationale: [#48](https://github.com/jedwards1230/scrim/issues/48).

## OIDC login for reads

Setting `--oidc-issuer` turns on native OpenID Connect login for hub **reads**,
replacing the CIDR/read-token gate with proven identity (so people can browse
from anywhere with a login, not just the allowlisted network). It's **opt-in and
fail-closed**: with no `--oidc-issuer` the hub behaves as documented in
[hub.md](hub.md); with it set the hub performs OIDC discovery at startup and
**refuses to start** if the issuer is unreachable or a required field is
missing, so there's no half-configured state. Writes stay push-token only,
unaffected.

The flow is a standard authorization-code login with state, nonce, and PKCE; the
ID token is verified (signature via JWKS, issuer, audience, nonce) before a
signed, HttpOnly session cookie is minted. Unauthenticated reads redirect a
browser to `/auth/login` and return `401` to non-browser clients (the SSE stream
authenticates with the same cookie). Any user the IdP authenticates is accepted
on first login — identity keys on the standard `sub` claim, there's no user list
to pre-seed.

```bash
scrim hub \
  --push-token "$(openssl rand -hex 32)" \
  --oidc-issuer https://auth.example.com/application/o/scrim/ \
  --oidc-client-id scrim-hub \
  --oidc-client-secret "$CLIENT_SECRET" \
  --oidc-redirect-url https://scrim.example.com/auth/callback
```

- `--oidc-issuer` (env `SCRIM_OIDC_ISSUER`) — the single switch that enables OIDC.
- `--oidc-client-id` / `--oidc-client-secret` (env `SCRIM_OIDC_CLIENT_ID` /
  `SCRIM_OIDC_CLIENT_SECRET`) — required when OIDC is on.
- `--oidc-redirect-url` (env `SCRIM_OIDC_REDIRECT_URL`) — the hub's full external
  `/auth/callback` URL; must match the IdP registration exactly (the hub can't
  derive it behind a TLS-terminating proxy). Required.
- `--oidc-scopes` (env `SCRIM_OIDC_SCOPES`, default `openid,profile,email`).
- `--oidc-session-secret` (env `SCRIM_OIDC_SESSION_SECRET`) — HMAC key for the
  session cookie; if empty a random one is generated (sessions then reset on
  restart). Set a stable value (**at least 32 bytes**, else the hub refuses to
  start) to persist sessions across restarts/replicas.
- `--oidc-session-ttl` (env `SCRIM_OIDC_SESSION_TTL`, default `12h`). Sessions
  are **stateless** — see the [threat model](threat-model.md#stateless-non-revocable-oidc-sessions).
- `--oidc-secure-cookies` (env `SCRIM_OIDC_SECURE_COOKIES`, default `true`) —
  leave on in production; pass `=false` only for a plain-HTTP local test hub.

**Authentik gotcha:** Authentik's default scope mapping returns
`email_verified: false` unless you fix it per-application. scrim does **not**
gate on `email_verified` — access keys on `sub` alone — so this locks nobody out
and needs no workaround. (If you rely on `email` elsewhere, set Authentik's
provider to emit `email_verified: true`.)

## Ownership, sharing & tokens

Every canvas has an **owner** (a principal's email, or `admin` for the push
token and legacy canvases) and a **grant list** — private by default, visible
only to the owner, admin, and explicit grantees until shared.

- **Migration & claim.** On every hub startup, any canvas whose metadata
  predates ownership is stamped `owner: admin`. A logged-in principal reclaims
  one it actually created via the gallery's Claim button
  (`POST /api/canvases/{id}/claim`, any authenticated caller); a canvas already
  owned by someone else is `409`, claiming your own is an idempotent `200`.
- **User tokens** (`/tokens` page; `POST`/`GET /api/tokens`,
  `DELETE /api/tokens/{id}`) — a logged-in session mints a named bearer token
  that acts AS its owner on the Direct plane: canvases it creates or writes (via
  `scrim push --token` or `scrim mcp --hub`) are owned by that principal, not
  the shared admin credential. A token can carry `auto_share` grants (applied to
  every canvas it creates) and an `allowed_grant_targets` allowance bounding what
  it may later share interactively; minting a token for another principal is
  admin-only (no privilege escalation).
- **Sharing** — `GET`/`POST /api/canvases/{id}/grants`,
  `DELETE .../grants/{grantRef}`. Grant kinds: `user` (one email), `group`,
  `everyone` (any authenticated viewer), `link` (an unguessable secret, shown
  once at creation, redeemed as `?k=<secret>`). The browser's share dialog drives
  these natively for a session that owns the canvas — safe against CSRF because
  the session cookie is HttpOnly + SameSite=Lax. The `share_canvas`/`list_grants`
  MCP tools do the same over the machine API. Grantee autocomplete comes from
  `GET /api/principals?q=` — principals the hub has *observed* (logins, verified
  forwarded-identity headers, grant targets), display-only, never an
  authorization source.

**Two planes attribute identity differently.** Direct requests (a browser
session, or `scrim push --token <user-token>`) carry identity natively.
Forwarded-identity-plane requests (agent → trusted gateway → scrim-mcp) carry it
via HMAC-signed headers that scrim-mcp itself must verify (see below).

Private-by-default *visibility* (owner/admin/grant matching) is enforced on
reads only when `--oidc-issuer` is set — without OIDC the hub's CIDR/read-token
gate is unchanged and every canvas stays visible to anyone who passes it.
Ownership always governs *writes*: a user token (or a forwarded actor) may only
create or mutate a canvas its owner can write; the admin push token is
unrestricted either way.

## The forwarded-identity plane

When scrim mcp sits behind a trusted gateway, an end-user's identity reaches the
hub through a signed-header handoff rather than a shared credential. The gateway
authenticates the end user and forwards the principal as HMAC-signed
`X-Forwarded-User-*` headers (shared secret in
`SCRIM_MCP_IDENTITY_HMAC_SECRET`); scrim mcp verifies them and re-emits the
verified principal to the hub as `X-Scrim-Actor-*` on top of its own hub bearer,
so a canvas is attributed to the real caller rather than the shared credential.

The mechanism is generic: the gateway is any reverse proxy that authenticates
the end user and forwards a signed principal in scrim's wire format — the
`X-Forwarded-User-*` header names plus the canonicalization + HMAC scheme in
`internal/mcpserver/identity.go`. (In this project's own deployment that gateway
is the ContextForge MCP gateway.)

An unset secret is fail-closed: identity is not verified and every call is
attributed to whatever hub credential scrim mcp itself holds. This is why a
deployment that wants agent output visible to a human *without* wiring up
per-request header signing instead mints the agent a [user
token](#ownership-sharing--tokens) with an `auto_share` grant to that human's
email or group — the agent's calls own their own canvases under its own service
identity, auto-shared to the human, rather than depending on per-request
forwarded identity.

The hub trusts the re-emitted `X-Scrim-Actor-*` headers ONLY when they ride a
valid admin push token; a spoofed header on any other request is ignored by
construction. A network policy pinning the hub's ingress to scrim-mcp is the
second half of the defense — see the
[threat model](threat-model.md#cidr-checked-on-remoteaddr).

## Authentik directory (optional)

Setting **both** `--authentik-url` and `--authentik-token` turns on a read-only
pull of Authentik users/groups that enriches `GET /api/principals` with display
names and groups for people who haven't shown up in the observed registry yet.
Setting only one of the pair leaves the feeder off (a startup warning is
logged).

- `--authentik-url` (env `SCRIM_AUTHENTIK_URL`) — the Authentik instance's base
  URL.
- `--authentik-token` (env `SCRIM_AUTHENTIK_TOKEN`) — a **read-only** Authentik
  API token; the client only ever issues GETs.
- `--authentik-cache-ttl` (env `SCRIM_AUTHENTIK_CACHE_TTL`, default `5m`) — how
  long pulled entries are cached in memory.

Pulled data is cached in memory only, **never persisted**, and **never consulted
for enforcement** — an unreachable or misconfigured Authentik silently degrades
autocomplete and never fails a request or the hub.
