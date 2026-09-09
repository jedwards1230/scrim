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
- `--oidc-session-ttl` (env `SCRIM_OIDC_SESSION_TTL`, default `12h`) — how long
  a session lives if nobody signs it out. Sessions are **server-side records**
  and individually revocable; see [Browser sessions](#browser-sessions-devices)
  below and the [threat model](threat-model.md#revocable-oidc-sessions-at-the-cost-of-a-piece-of-state).
- `--oidc-secure-cookies` (env `SCRIM_OIDC_SECURE_COOKIES`, default `true`) —
  leave on in production; pass `=false` only for a plain-HTTP local test hub.
  Despite the `oidc-` prefix it governs **every** cookie the hub sets: the OIDC
  cookies *and* the `--read-token` capability cookie, so the two can't drift.
  It has to be operator configuration rather than something derived from the
  request — a hub is normally behind a TLS-terminating proxy, so `r.TLS` is nil
  on exactly the requests that need `Secure` set.
- `--oidc-post-logout-redirect-url` (env `SCRIM_OIDC_POST_LOGOUT_REDIRECT_URL`) —
  optional, **off by default**; see [Logout](#logout) below.

### Browser sessions (devices)

Every login is recorded server-side, in a whole-file JSON registry
(`sessions.json`) under the hub's meta dir — the `internal/session` package,
shaped exactly like `internal/usertoken` with the parsed records additionally
held in memory, since the gate consults them on every authenticated request.
Each record holds the session id its cookie carries, the principal, the
**User-Agent string**, and the created/last-seen/expiry timestamps. **No IP
address is recorded**, and none is displayed. `last_seen` is persisted at most
once every five minutes, so it lags slightly rather than costing a disk write
per request.

That registry is what makes a session revocable before it expires. The
"Devices & access" page at `/tokens` lists a principal's live sign-ins above
its tokens, labels the one making the request **This browser**, and offers a
sign-out per entry (`GET /api/sessions`, `DELETE /api/sessions/{id}` — see
[`api/openapi.yaml`](../api/openapi.yaml)). Ending a session takes effect on
that browser's very next request. The device label is derived from the
User-Agent with deliberately crude client-side parsing; an unrecognised agent
is shown raw rather than labeled with a guess.

Three properties are load-bearing:

- **Session-only plane.** `/api/sessions*` accepts a browser session and
  nothing else: a user token is `403`, and the admin push token — which has no
  sign-ins of its own — gets an empty list and `404`. Another principal's
  session id is `404`, never `403`, exactly like `DELETE /api/tokens/{id}`.
- **Fail closed, but only on a real failure.** A registry that exists and
  cannot be read or parsed rejects every session-authenticated request until an
  operator fixes it. A *missing* file is an empty registry — a hub's first
  boot — and permits logins normally. The admin push token never consults the
  registry, so it stays the recovery path either way.
- **The upgrade signs everyone out once.** A session cookie with no session id
  in it — every cookie minted before the registry existed — is rejected
  outright. That is intended: a cookie no record backs is a session nothing can
  revoke.

### Agent connections

A third thing can reach a principal's canvases, and until recently it appeared on
no list at all: an **MCP client authenticated by OAuth**. When an agent connects
to `scrim mcp --http` with `--oauth-issuer` set, its calls arrive at the hub as
the admin push token plus verified `X-Scrim-Actor-*` headers (see [the
forwarded-identity plane](#the-forwarded-identity-plane) below) — neither a
browser session nor a user token, so an agent holding a live refresh token had
ongoing access that nothing on the devices page mentioned.

`scrim mcp` therefore forwards two more verified values off the *already
validated* JWT, on that same trusted plane: the OAuth client (`azp`, falling
back to `client_id`) as `X-Scrim-Actor-Client-Id`, and the token's `iat` as
`X-Scrim-Actor-Token-Issued-At` (Unix seconds). The hub keeps one record per
**(IdP subject, client id)** pair in `agent-connections.json` under its meta dir
— the `internal/agentconn` package, shaped exactly like `internal/session`,
in-memory with a throttled `last_seen` write — and the gate consults it on every
forwarded-actor request. `GET /api/agent-connections` and
`DELETE /api/agent-connections/{id}` list and revoke them from the same
session-only plane `/api/sessions*` uses; the devices page renders them between
the browser sign-ins and the tokens.

What revoking does, exactly:

- **It blocks that client at scrim, on its very next request** (`403`). That
  half is immediate and unconditional.
- **It does not touch the identity provider.** The client keeps the grant and
  the refresh token it already holds. Authorizing scrim again mints a token
  whose `iat` postdates the revocation, and the hub admits that one — clearing
  the revocation, so the page stops calling a working connection revoked. That
  is deliberate and the page says so outright, because a control that looks like
  it did more than it did is precisely the bug
  [#146](https://github.com/jedwards1230/scrim/pull/146) had to fix. Cutting an
  agent off for good means removing scrim's authorization at the IdP too.

The same three properties the session registry has hold here, for the same
reasons:

- **Session-only plane.** A user token and the machine plane (including the
  agent itself) get `403`; the admin push token, which has no agent connections
  of its own, gets an empty list and `404`. Another principal's connection id
  is `404`, never `403`.
- **Fail closed, but only on a real failure.** An unreadable or corrupt registry
  refuses every forwarded-actor request (`503`) until an operator fixes it; a
  *missing* file is an empty registry. The bare admin push token — no actor
  headers — never consults the store and is unaffected by every revocation in
  it, which is what keeps `scrim push` and CI working while the rest fails
  closed.
- **A revocation is never silently unenforceable.** A caller with no client id
  (the HMAC forwarded-identity plane, which carries no JWT) or no `iat` cannot
  satisfy the re-authorization exemption, so a matching revocation blocks it
  unconditionally. A client-id-less caller keys on the principal's empty-client
  row, so revoking that row cuts off that whole plane for the principal.

### Logout

Logging out performs **RP-initiated logout** ([OIDC RP-Initiated Logout 1.0][rpl]):
`POST /auth/logout` clears scrim's own cookies, **drops the session's registry
record** (so a copy of that cookie taken elsewhere stops working too), and then
redirects the browser to the IdP's `end_session_endpoint` so the **IdP session
ends too**.

That second half is the whole point. Clearing only scrim's cookie is not a
logout while the IdP's SSO cookie survives — the next request bounces through
`/auth/login`, the IdP recognises the still-valid SSO session, and the user is
silently signed back in. To anyone pressing the button, the app just refreshed
itself.

The endpoint is **discovered**, never constructed: scrim reads
`end_session_endpoint` from the issuer's `/.well-known/openid-configuration`,
so this works against whichever IdP the deployment points at, with no
provider-specific URL shape in the code. An issuer that advertises none gets a
local-only logout (scrim's cookies are cleared; the IdP session is untouched),
which is the honest best it can do.

To let the IdP end the exact session without prompting, scrim retains the login's
raw ID token in a separate signed, HttpOnly cookie and presents it as
`id_token_hint`. It is deliberately **not** folded into the session cookie: an ID
token carrying many group claims can exceed the ~4KB a browser will store, and
browsers drop an oversized cookie *silently*. Kept apart, a token too large to
retain costs a smoother logout and nothing else — the login still works, and
logout still reaches the IdP identifying itself by `client_id` instead.

**`post_logout_redirect_uri` is omitted by default**, and that default is the
recommended one. IdPs validate the parameter against a per-client registration
list and answer an unregistered value with an error page — a worse outcome than
the provider's own "you have been logged out" page, which is what omitting it
produces. Set `--oidc-post-logout-redirect-url` only *after* registering that
exact URL with the IdP (in Keycloak, the client's `post.logout.redirect.uris`
attribute; in Authentik, the provider's allowed redirect URIs). The URL is
validated at startup, so a value that could never work fails the hub at boot
rather than at someone's first logout.

Logout is `POST`-only — a `GET` logout is CSRF-able via an `<img>` or a link, so
the route answers `GET` with `405`. A session-less `POST` (what a forged
cross-site request produces, since `SameSite=Lax` withholds the cookie) clears
cookies but does **not** redirect to the IdP, so scrim can't be used as an open
"end this person's IdP session" redirector.

[rpl]: https://openid.net/specs/openid-connect-rpinitiated-1_0.html

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
- **User tokens** (the "Devices & access" page at `/tokens`; `POST`/`GET /api/tokens`,
  `DELETE /api/tokens/{id}`) — a logged-in session mints a named bearer token
  that acts AS its owner on the Direct plane: canvases it creates or writes (via
  `scrim push --token` or `scrim mcp --hub`) are owned by that principal, not
  the shared admin credential. A token can carry `auto_share` grants (applied to
  every canvas it creates) and an `allowed_grant_targets` allowance bounding what
  it may later share interactively; minting a token for another principal is
  admin-only (no privilege escalation). The page reads as an account's
  "devices / active sessions" view — it leads with what currently has access,
  ordered most-recently-used first, flags long-unused credentials, collapses
  revoked ones behind a disclosure, and keeps minting below the list. Browser
  sign-ins lead the page above the tokens, each individually signable-out — see
  [Browser sessions](#browser-sessions-devices).
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
- **Duplicate & delete from the canvas shell** — `POST /api/canvases/{id}/copy`
  and `DELETE /api/canvases/{id}`. Both accept a browser session that may
  *write* the canvas named in the path (CSRF-safe for the same reason sharing
  is: HttpOnly + SameSite=Lax), and the shell renders each menu item only for a
  viewer that same decision admits — so a view-only grantee is offered neither
  and would be `403` if they forged the call anyway. Delete always confirms in
  the browser first, naming the canvas.

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

The mechanism is generic and names no particular product: the gateway is any
reverse proxy that authenticates the end user and forwards a signed principal in
scrim's wire format — the `X-Forwarded-User-*` header names plus the
canonicalization + HMAC scheme in `internal/mcpserver/identity.go`. Pointing a
given gateway at it is a configuration/adapter task, not a code change.

An unset secret is fail-closed: identity is not verified and every call is
attributed to whatever hub credential scrim mcp itself holds. This is why a
deployment that wants agent output visible to a human *without* wiring up
per-request header signing instead mints the agent a [user
token](#ownership-sharing--tokens) with an `auto_share` grant to that human's
email or group — the agent's calls own their own canvases under its own service
identity, auto-shared to the human, rather than depending on per-request
forwarded identity.

On the OAuth path the same handoff additionally carries the OAuth client id and
the token's `iat` (`X-Scrim-Actor-Client-Id` / `X-Scrim-Actor-Token-Issued-At`),
which is what makes the connection listable and revocable — see [Agent
connections](#agent-connections). The HMAC plane has no JWT, so it sets neither
header, and the hub reads that absence fail-closed rather than as consent.

The hub trusts the re-emitted `X-Scrim-Actor-*` headers ONLY when they ride a
valid admin push token; a spoofed header on any other request is ignored by
construction. A network policy pinning the hub's ingress to scrim-mcp is the
second half of the defense — see the
[threat model](threat-model.md#cidr-checked-on-remoteaddr).

## Authentik directory (optional, and dead in practice)

> **This feeder names an IdP scrim no longer uses.** As of 2026-09-05 the
> deployed hub sets none of the three variables below, and there is no Authentik
> instance left to pull from, so grantee autocomplete runs on the observed-principal
> registry alone: you get people the hub has already seen, and type a full email
> address for anyone else. The flags still work if you point them at an Authentik
> that exists — nothing was removed — but nothing here is exercised today.
> Replacing it with an IdP-neutral source is
> [#132](https://github.com/jedwards1230/scrim/issues/132); the reasoning is PRD §13.11.

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
