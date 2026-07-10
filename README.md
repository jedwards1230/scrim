# scrim

A projection surface for coding agents.

`scrim` is a single Go binary: a shared, self-starting daemon serves
agent-authored HTML canvases with live reload over SSE, and a human views
them in a browser. An agent writes (or updates) an HTML file; `scrim` makes
it reachable at a stable URL and pushes reload events when the file changes.

> A random capability token gates every request by default (`--no-auth` to
> disable), the daemon advertises `scrim.local` over mDNS when bound beyond
> loopback, `link` prints a canvas's URL and never launches a browser, `open`
> prints the same URL but can also launch your default browser (opt-in, via
> `--browser` or `SCRIM_OPEN_BROWSER=1`), and a version-mismatched daemon is
> replaced automatically the next time the CLI self-starts one.

## Install

```bash
go install github.com/jedwards1230/scrim@latest
```

## Usage

```bash
# Local (the default experience — loopback daemon, zero setup)
scrim add <id> [--title T] [--desc D] [--icon I]  # Register a canvas
scrim path <id>               # Print the filesystem path for a canvas
scrim list                    # List registered canvases
scrim link [<id>]             # Print a canvas's (or the dashboard's) URL; never opens a browser
scrim open [<id>]             # Print a canvas's (or the dashboard's) URL; add --browser to also open it
scrim rm <id>                 # Remove a canvas
scrim snap <id> [--label L]   # Snapshot a canvas's current contents
scrim snaps <id>              # List a canvas's snapshots
scrim revert <id> [<snap>]    # Restore a canvas from a snapshot (latest by default)
scrim status                  # Show daemon status
scrim stop                    # Stop the daemon
scrim serve                   # Run the daemon in the foreground
scrim mcp [--http ADDR]       # Serve scrim's verbs as MCP tools (stdio; --http for streamable HTTP)

# Hub (optional, additive — a deployed central store; see "Local vs. hub")
scrim hub --push-token TOKEN [--data DIR] [--host HOST] [--port PORT] [--allow CIDR,...]
                               # Run a hub: a shared, network-reachable central store
scrim push <id> --to URL --token TOKEN [--watch]
                               # Tar a LOCAL canvas and push it to a hub
```

`link` is the recommended everyday verb for getting a canvas's URL --
especially for an agent, since it can never trigger a browser launch on the
user's machine. Reach for `open` only when you (or, if you're an agent, the
user themself) actually wants the URL opened.

The daemon self-starts on first use of any verb that needs it (`add`,
`link`, `open`, etc.) and idles down after `--idle-timeout` of inactivity.
`snap`/`snaps`/`revert` are pure filesystem operations (like `path`/`rm`'s
fallback) and never self-start the daemon.

## Local vs. hub

scrim has two modes, and the local one is the product — the hub is an
optional, additive layer for sharing. A localhost-only user never runs
`hub`/`push` and sees zero difference; the default daemon path gets no new
behavior, dependencies, or HTTP surface from hub mode (enforced by
`internal/server/hub_test.go`).

|                | Local daemon                      | Hub                                    |
|----------------|-----------------------------------|----------------------------------------|
| Purpose        | Live preview on *this* machine    | Durable, shared store for many machines |
| Starts         | Self-starts on first verb         | Run explicitly (`scrim hub`, container) |
| Data dir       | `~/.scrim`                        | `~/.scrim-hub` (`--data`, `/data` in the container) |
| Bind / port    | `127.0.0.1:7777`                  | `0.0.0.0:7788`                          |
| Reads gated by | Capability token → cookie         | CIDR allowlist (+ optional read token), or OIDC login |
| Writes gated by| Same token                        | Push bearer token (required, fail-closed) |
| Content source | Files you edit on disk (live)     | Whatever was last `push`ed              |
| Lifecycle      | Idles out after `--idle-timeout`  | Long-lived (idle-exit disabled)         |
| mDNS           | On when bound beyond loopback     | Off by default                          |

Both run happily on one box — separate data dirs and ports by design. Local
file-saves live-reload a local tab exactly as always, with or without a hub
anywhere in your life; `push` is the only bridge between the two, and it's
explicit per-canvas.

## Gallery dashboard & canvas metadata

The dashboard at `/` is a card gallery: each canvas gets an emoji icon
(explicit via `--icon`, or a deterministic default derived from its ID when
omitted), an accent color (always derived from the ID), its title/description,
last-modified time, and a live SSE viewer count that updates in place via a
short client-side poll of `GET /api/canvases` -- no page reload. A canvas's
served pages also get a matching favicon generated from that same icon,
unless the canvas itself ships its own `favicon.ico`.

Canvas metadata (title, description, icon) is stored **outside** the canvas
directory, under `~/.scrim/meta/<id>.json` -- not as a sidecar file inside
the canvas directory (a v0.1 pattern removed in v0.2) -- since anything
under the canvas directory is servable and filesystem-watched, and metadata
must be neither.

## Snapshots

`scrim snap <id> [--label L]` copies a canvas's current contents into
`~/.scrim/versions/<id>/<timestamp>[-<label>]/`. `scrim snaps <id>` lists
them, newest first. `scrim revert <id> [<snapshot>]` replaces the canvas's
current contents with a snapshot's -- entirely, not merged -- defaulting to
the latest snapshot when none is named; it takes its own `prerevert`
snapshot of whatever was there first, so a revert is itself undoable via
another revert.

An `index.md` is rendered to HTML at serve time; raw HTML embedded in it is
passed through unsanitized, the same trust model as a `.html` canvas.

## Flags & environment variables

These are the **local daemon's** flags and defaults; the hub deliberately
overrides several (data dir, host, port, idle timeout, mDNS — see
[Hub](#hub-a-shared-central-store), and note `--no-auth` doesn't exist
there: hub auth is the push-token/CIDR gate instead).

| Flag             | Env var             | Default     | Description                        |
|------------------|----------------------|-------------|-------------------------------------|
| `--port`         | `SCRIM_PORT`         | `7777`      | Port the daemon listens on          |
| `--host`         | `SCRIM_HOST`         | `127.0.0.1` | Host the daemon binds to            |
| `--idle-timeout` | `SCRIM_IDLE_TIMEOUT` | `30m`       | Idle time before the daemon exits. `0` or negative disables idle exit entirely — the daemon then only stops via `scrim stop` or a signal |
| `--no-auth`      | `SCRIM_NO_AUTH`      | `false`     | Disable local auth token            |
| `--no-mdns`      | `SCRIM_NO_MDNS`      | `false`     | Don't advertise over mDNS, even when `--host` binds beyond loopback |
| `--dir`          | `SCRIM_DIR`          | `~/.scrim`  | Directory for canvases + state. A relative value is resolved to an absolute path immediately (against the CLI's cwd), before it's ever handed to a self-started daemon |

Run `scrim --help` (or `scrim <verb> --help`) for the full flag/verb reference.

## Auth & discovery

Every daemon mints a random capability token at startup. The URLs `add`,
`list`, `link`, `open`, and `status` print carry it as `?t=<token>`; the first
request with a valid token sets a cookie and is then redirected to the same
URL with the token stripped, so it never lingers in the URL bar, browser
history, or a copied/shared link -- the browser's own follow-up requests
(including live-reload's SSE connection) authenticate via that cookie
instead. Requests with neither a valid token nor a valid cookie get 401.
Pass `--no-auth` (or set `SCRIM_NO_AUTH=1`) to disable this entirely.

When `--host` binds beyond loopback, the daemon also advertises itself as
`scrim.local` over mDNS unless `--no-mdns` (or `SCRIM_NO_MDNS=1`) is set;
printed URLs then show both the `scrim.local` form and the plain `ip:port`
fallback, since mDNS can be blocked on some networks.

## Privacy

Beyond the token-stripping redirect above: every response carries
`Referrer-Policy: no-referrer`, canvas content responses are marked
`Cache-Control: no-store` so browsers never retain them, and the daemon's
own logging never includes a request path, canvas ID, query string, or the
raw token, at any log level.

On Unix-like systems (Linux, macOS), `~/.scrim` (or whatever `--dir` points
to) is also tightened to owner-only permissions on every startup (0700 for
the directory, 0600 for the state and log files -- even if it was created
looser by an older scrim version). This isn't implemented on Windows yet
(`os.Chmod` there only toggles the read-only attribute, not owner-only
access) -- a one-time warning is logged instead of silently claiming
success; tracked in [#19](https://github.com/jedwards1230/scrim/issues/19).

## link vs. open

Both verbs resolve and print the same URL for a canvas (or the dashboard,
with no id) — they only differ in what happens after.

`scrim link [<id>]` is permanently print-only: no flag or environment
variable can make it launch a browser. It's the verb to reach for by
default, and the one an agent should always use.

`scrim open [<id>]` prints the same URL, and can *also* launch it in your
platform's default browser (`open` on macOS, `xdg-open` on Linux,
`rundll32 url.dll,FileProtocolHandler` on Windows) — but that launch is
**opt-in**, since scrim's daemon is commonly self-started by an agent on
the user's behalf and a browser tab popping up unprompted is a surprise,
not a convenience. By default `open` just prints the URL plus a one-line
stderr hint about how to opt in; pass `--browser` for a one-off launch, or
set `SCRIM_OPEN_BROWSER=1` to make that the default for every `open`. When
mDNS is active, the URL handed to the browser is the same `scrim.local` one
printed first — not the plain `ip:port` fallback — so it still works when
the daemon is bound to an unaddressable host like `0.0.0.0`. If the launch
is requested but fails (e.g. no browser installed, headless environment),
`open` prints a one-line notice on stderr and still exits `0`.

## MCP server

`scrim mcp` exposes scrim's verbs as [MCP](https://modelcontextprotocol.io)
tools, so an agent drives scrim natively instead of shelling out. Tools:
`add`, `list`, `link`, `copy_canvas`, `rm`, `snap`, `snaps`, `revert`,
`status`, `list_files`, `read_file`, `write_file`, `edit_file`, `share_canvas`,
`list_grants`, `push` (plus `path` in local mode only — a server-local
directory has no remote meaning). `share_canvas`/`list_grants` manage a
canvas's view-only sharing grants (see [Ownership, sharing &
tokens](#ownership-sharing--tokens)) over the machine API.
`list_files` enumerates a canvas's files (paths + sizes, no content) so an
agent can discover what to read or edit. `edit_file` applies an exact-string
replacement server-side, so hub-mode edits cost tokens proportional to the
change, not the file, and accepts an `edits` array to apply many replacements
in one transactional (all-or-nothing) call. `write_file`/`read_file` accept an
optional `encoding: "gzip+base64"` to move large or binary content compressed
(the size cap applies to the decoded bytes). `copy_canvas` duplicates a canvas
server-side. Each maps to the same code path as the matching verb, so the same
safety invariants hold: `link` returns a URL as data and **never** launches a
browser, no tool logs URLs/canvas content/tokens, and `push` is one-shot —
and, whichever mode, `push` packs the canvas from the MCP server process's own
disk, so a remote/in-cluster deployment should author with `write_file`/
`edit_file` instead.

Transport is stdio by default; pass `--http ADDR` for streamable HTTP. The
HTTP endpoint is unauthenticated by default, so it fails closed: a
non-loopback bind is refused unless you pass `--allow-lan` or configure
[OAuth resource mode](#oauth-20-resource-mode---http-only), which
authenticates every request instead. `scrim mcp --http 127.0.0.1:9797` is the
safe default.

```jsonc
// e.g. an MCP client config — local mode
{ "command": "scrim", "args": ["mcp"] }
```

### Local vs hub mode

- **Local mode** (default): tools operate on the local daemon and the local
  canvas directory on disk. `add`/`path` return server-local filesystem
  paths, and `write_file`/`read_file` act on that directory — the right model
  when the agent and scrim share a machine.
- **Hub mode** (`--hub URL`): the same tool surface operates on a **remote**
  hub over its bearer-authenticated machine API — for a scrim mcp hosted away
  from the agent (e.g. in-cluster). Since there's no shared disk, authoring is
  done entirely through `write_file`/`read_file` (inline content, ~2 MiB cap);
  `path` is absent (a server-local path is meaningless remotely). A bearer
  token authenticates every call — from `SCRIM_PUSH_TOKEN` (the admin
  credential) or `--hub-token-file PATH` — and can be either the admin push
  token or a [user token](#ownership-sharing--tokens); `scrim mcp --hub` fails
  closed with no token. A user token attributes everything it creates/writes to
  its owner instead of the shared admin credential — see [Identity on the
  streamable-HTTP transport](#identity-on-the-streamable-http-transport) for
  how a per-request end-user is instead attributed when scrim mcp sits behind
  ContextForge.

```jsonc
// hub mode — SCRIM_PUSH_TOKEN in the environment
{ "command": "scrim", "args": ["mcp", "--hub", "https://scrim-hub.example"] }
```

The tradeoff is disk vs token: local mode trusts the shared filesystem; hub
mode trusts the bearer token and moves bytes over HTTP. The hub's machine API
(canvas list/add/rm/status/copy, per-canvas file listing, per-file
GET/PUT/PATCH, snapshot create/list/revert) is gated by the push token on
**every** call, reads included — separate from the browser read gate
(CIDR/read-token). File PUTs may carry a `Content-Encoding: gzip` body and GETs
an `Accept-Encoding: gzip` request; the hub inflates/deflates transparently
(the per-file cap applies to the decoded size).

### Identity on the streamable-HTTP transport

Two identity layers apply to `--http`, and they're orthogonal:

- **ContextForge header-trust** (existing): when scrim mcp sits behind a
  trusted gateway, it verifies HMAC-signed `X-Forwarded-User-*` headers
  (shared secret in `SCRIM_MCP_IDENTITY_HMAC_SECRET`) and re-emits the
  verified principal to the hub as `X-Scrim-Actor-*` on top of its own hub
  bearer, so a canvas is attributed to the real caller rather than the shared
  credential. An unset secret is fail-closed: identity is not verified and
  every call is attributed to whatever hub credential scrim mcp itself holds.
  This is why a deployment that wants agent output visible to a human without
  wiring up per-request header signing instead mints the agent a [user
  token](#ownership-sharing--tokens) with an `auto_share` grant to that
  human's email or group — the agent's calls own their own canvases under its
  own service identity, auto-shared to the human, rather than depending on
  per-request forwarded identity.
- **OAuth 2.0 resource mode** (below): authenticates the *client connection*
  itself (the MCP host presenting a bearer JWT), independent of which end
  user or service the ContextForge layer attributes a call to.

### OAuth 2.0 resource mode (`--http` only)

Setting `--oauth-issuer` turns `--http`'s `/mcp` endpoint into an
[RFC 9728](https://www.rfc-editor.org/rfc/rfc9728) OAuth 2.0 protected
resource ([#33](https://github.com/jedwards1230/scrim/issues/33)):
unauthenticated protected-resource metadata is served at
`/.well-known/oauth-protected-resource`, and every request to `/mcp` must
carry a bearer JWT that's validated (signature/issuer/audience/expiry, via
the issuer's JWKS) before it's served. A `tools/call` additionally needs the
tool's required scope — `scrim:read` for lookups, `scrim:write` for
everything else (a write-scoped token also satisfies a read requirement). A
missing/invalid token is `401`; an insufficient scope is `403`; both carry a
`WWW-Authenticate` challenge pointing at the metadata document. stdio is
unaffected — it carries no inbound HTTP request for this layer to check.

```bash
scrim mcp --http 0.0.0.0:9797 \
  --oauth-issuer https://auth.example.com/application/o/scrim-mcp/ \
  --oauth-audience scrim-mcp
```

- `--oauth-issuer` (env `SCRIM_MCP_OAUTH_ISSUER`) — the authorization
  server's issuer URL; setting it turns OAuth resource mode on. A
  bad/unreachable issuer fails startup (one-shot OIDC discovery, like
  `--oidc-issuer` below). Once set, a non-loopback `--http` bind no longer
  needs `--allow-lan` — the endpoint is authenticated.
- `--oauth-audience` (env `SCRIM_MCP_OAUTH_AUDIENCE`) — the expected `aud`
  claim (the resource id the AS mints tokens for). Required whenever the
  issuer is set; a resource with no pinned audience would accept any token
  the AS ever issued, for any resource.
- `--oauth-resource` (env `SCRIM_MCP_OAUTH_RESOURCE`) — the canonical
  resource URL advertised in the metadata document. Optional: derived from
  the inbound request (honoring `X-Forwarded-Proto`) when unset; set it
  explicitly when that can't be derived correctly (e.g. behind a
  TLS-terminating proxy scrim can't see through).

## Hub: a shared central store

`scrim hub` runs the exact same serving engine as `scrim serve`, but at its
own data directory and port, with a push/read-token + CIDR gate in place of
the local daemon's capability-token auth. It serves its own durable storage
at its own root (`/c/<id>/`), so every URL it produces (SSE, favicon,
redirects, relative paths) is correct with zero rewriting -- clients `push`
canvases to it rather than the hub reading from a remote filesystem.

```bash
scrim hub --push-token "$(openssl rand -hex 32)" --allow 192.168.1.0/24
```

- `--data DIR` (env `SCRIM_HUB_DATA`, default `~/.scrim-hub`) -- deliberately
  separate from the local daemon's `~/.scrim`, so both can run on one box.
- `--host` defaults to `0.0.0.0` -- a hub binds beyond loopback by design;
  the CIDR allowlist below is the read security, not the bind address.
- `--port` (env `SCRIM_PORT`) defaults to `7788` -- distinct from the local
  daemon's `7777`.
- `--push-token TOKEN` (env `SCRIM_PUSH_TOKEN`) is **required**: a hub fails
  closed (refuses to start) with no push token, rather than ever running a
  write-accepting server with no write gate.
- `--read-token TOKEN` (env `SCRIM_READ_TOKEN`) is optional, and additionally
  gates reads once the CIDR check below passes.
- `--allow CIDR[,CIDR...]` (env `SCRIM_HUB_ALLOW`) is the read allowlist,
  checked against the client's `RemoteAddr` (never `X-Forwarded-For` --
  that's trivially spoofable; a trusted-proxy layer is a later phase).
  Defaults to loopback-only (`127.0.0.0/8,::1/128`) when unset.
- The push token is a standard bearer credential (`Authorization: Bearer
  <token>`) and is **not** subject to the CIDR allowlist, since a legitimate
  machine client is commonly outside the read allowlist entirely (e.g. a
  laptop or in-cluster MCP server pushing to a homelab hub it isn't itself
  permitted to browse from).
- **The push token is now read+write, not write-only** — and it's the hub's
  **admin/bootstrap** credential: unrestricted over the whole machine API
  (`scrim mcp --hub`), a holder can read canvas content and file bytes
  (`GET /api/canvases/{id}/files/...`), list, and snapshot — not just push —
  and owns every legacy canvas. Size its trust accordingly when you
  distribute it (e.g. to an in-cluster MCP deployment) and rotate it as a
  read-capable secret. Browser reads remain separately gated by the CIDR
  allowlist (+ optional read token). A logged-in principal that wants its own
  scoped credential instead should mint a [user token](#ownership-sharing--tokens).
- Because a hub binds beyond loopback, hub mode adds two resource-exhaustion
  guards the local daemon doesn't have: request read timeouts
  (`ReadTimeout`/`IdleTimeout`, so a slow-trickle body can't pin a connection
  forever — SSE responses stay unbounded) and a concurrent SSE (live-reload)
  connection cap (256 total, 32 per canvas; `/c/<id>/__events` returns 503 past
  the ceiling). Both are hub-only, so the local daemon is unchanged.
- The hub is long-lived by default (`--idle-timeout` defaults to disabled)
  and doesn't advertise over mDNS by default (`--no-mdns` defaults to true).

`scrim push <id> --to URL --token TOKEN [--watch]` tars a **local** canvas
directory (read straight off disk via `--dir`/`SCRIM_DIR` -- it never talks
to a local daemon) and POSTs it to a hub's push endpoint, printing the
hub's canvas URL on success. `TOKEN` is the admin push token or a [user
token](#ownership-sharing--tokens) — either way it's the Direct plane, so
the pushed canvas is owned by whichever principal the token resolves to.
`--watch` re-pushes on every local change (200ms debounce) until
interrupted. `push` never launches a browser.

A multi-arch (amd64/arm64) container image running `scrim hub` against a
`/data` volume is published to GHCR on every release:

```bash
docker run -p 7788:7788 -v scrim-hub-data:/data ghcr.io/jedwards1230/scrim:latest \
  --push-token "$(openssl rand -hex 32)" --allow 192.168.1.0/24
```

(Or build it yourself from the repo's `Dockerfile`: `docker build -t scrim-hub .`)

### Machine API reference

The hub's machine API — the bearer-gated HTTP surface `scrim mcp --hub` drives
(canvas CRUD, per-file read/write/edit, push, copy, and snapshots) — is
documented as a hand-authored OpenAPI 3.1 spec at
[`api/openapi.yaml`](api/openapi.yaml). It is the canonical route reference and
is kept current with the handlers (a CI `vacuum lint` gate guards its validity).

A running hub also serves the spec at `GET /api/openapi.yaml` (embedded in the
binary, gate-exempt, hub-only), so standard OpenAPI tooling can read the
contract straight from a live instance — `curl http://<hub>/api/openapi.yaml`.
Only YAML is served (scrim adds no YAML-to-JSON dependency; modern tools read
YAML natively).

### Ownership, sharing & tokens

> This is the tail of the ownership/sharing/identity epic
> ([#48](https://github.com/jedwards1230/scrim/issues/48)); it lands *after*
> the MCP batch (#40–#43, #46) documented above, reusing that settled
> `internal/mcpserver` surface rather than changing it.

Every canvas has an **owner** (a principal's email, or `admin` for the push
token and legacy canvases) and a **grant list** — private by default,
visible only to the owner, admin, and explicit grantees until shared.

- **Migration & claim.** On every hub startup, any canvas whose metadata
  predates ownership is stamped `owner: admin`. A logged-in principal
  reclaims one it actually created via the gallery's Claim button
  (`POST /api/canvases/{id}/claim`, any authenticated caller); a canvas
  already owned by someone else is `409`, claiming your own is an idempotent
  `200`.
- **User tokens** (`/tokens` page; `POST`/`GET /api/tokens`,
  `DELETE /api/tokens/{id}`) — a logged-in session mints a named bearer
  token that acts AS its owner on the Direct plane: canvases it creates or
  writes (via `scrim push --token` or `scrim mcp --hub`) are owned by that
  principal, not the shared admin credential. A token can carry `auto_share`
  grants (applied to every canvas it creates) and an
  `allowed_grant_targets` allowance bounding what it may later share
  interactively; minting a token for another principal is admin-only (no
  privilege escalation).
- **Sharing** — `GET`/`POST /api/canvases/{id}/grants`,
  `DELETE .../grants/{grantRef}`. Grant kinds: `user` (one email), `group`,
  `everyone` (any authenticated viewer), `link` (an unguessable secret,
  shown once at creation, redeemed as `?k=<secret>`). The browser's share
  dialog drives these natively for a session that owns the canvas — safe
  against CSRF because the session cookie is HttpOnly + SameSite=Lax, the
  same reasoning that already makes `POST /api/tokens` session-writable. The
  `share_canvas`/`list_grants` MCP tools do the same over the machine API.
  Grantee autocomplete comes from `GET /api/principals?q=` — principals the
  hub has *observed* (logins, verified CF headers, grant targets), display-only,
  never an authorization source.
- **Two planes attribute identity differently.** Direct requests (a browser
  session, or `scrim push --token <user-token>`) carry identity natively.
  ContextForge-plane requests (agent → CF gateway → scrim-mcp) carry it via
  HMAC-signed headers that scrim-mcp itself must verify — see [Identity on
  the streamable-HTTP transport](#identity-on-the-streamable-http-transport)
  for how that plane attributes a call, and degrades, when unconfigured.

Private-by-default *visibility* (owner/admin/grant matching) is enforced on
reads only when `--oidc-issuer` is set — without OIDC the hub's CIDR/
read-token gate is unchanged and every canvas stays visible to anyone who
passes it, exactly as before this epic. Ownership always governs *writes*:
a user token (or a CF-forwarded actor) may only create or mutate a canvas
its owner can write; the admin push token is unrestricted either way.

### Authentik directory (optional)

Setting **both** `--authentik-url` and `--authentik-token` turns on a
read-only pull of Authentik users/groups that enriches `GET /api/principals`
with display names and groups for people who haven't shown up in the
observed registry yet. Setting only one of the pair leaves the feeder off
(a startup warning is logged).

- `--authentik-url` (env `SCRIM_AUTHENTIK_URL`) — the Authentik instance's
  base URL.
- `--authentik-token` (env `SCRIM_AUTHENTIK_TOKEN`) — a **read-only**
  Authentik API token; the client only ever issues GETs.
- `--authentik-cache-ttl` (env `SCRIM_AUTHENTIK_CACHE_TTL`, default `5m`) —
  how long pulled entries are cached in memory.

Pulled data is cached in memory only, **never persisted**, and **never
consulted for enforcement** — an unreachable or misconfigured Authentik
silently degrades autocomplete and never fails a request or the hub.

### OIDC login for reads

Setting `--oidc-issuer` turns on native OpenID Connect login for hub **reads**,
replacing the CIDR/read-token gate with proven identity (so people can browse
from anywhere with a login, not just the allowlisted network). It's **opt-in
and fail-closed**: with no `--oidc-issuer` the hub behaves exactly as above;
with it set the hub performs OIDC discovery at startup and **refuses to start**
if the issuer is unreachable or a required field is missing, so there's no
half-configured state. Writes stay push-token only, unaffected.

The flow is a standard authorization-code login with state, nonce, and PKCE;
the ID token is verified (signature via JWKS, issuer, audience, nonce) before a
signed, HttpOnly session cookie is minted. Unauthenticated reads redirect a
browser to `/auth/login` and return `401` to non-browser clients (the SSE
stream authenticates with the same cookie). Any user the IdP authenticates is
accepted on first login — identity keys on the standard `sub` claim, there's no
user list to pre-seed.

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
- `--oidc-redirect-url` (env `SCRIM_OIDC_REDIRECT_URL`) — the hub's full
  external `/auth/callback` URL; must match the IdP registration exactly (the
  hub can't derive it behind a TLS-terminating proxy). Required.
- `--oidc-scopes` (env `SCRIM_OIDC_SCOPES`, default `openid,profile,email`).
- `--oidc-session-secret` (env `SCRIM_OIDC_SESSION_SECRET`) — HMAC key for the
  session cookie; if empty a random one is generated (sessions then reset on
  restart). Set a stable value (**at least 32 bytes**, else the hub refuses to
  start) to persist sessions across restarts/replicas.
- `--oidc-session-ttl` (env `SCRIM_OIDC_SESSION_TTL`, default `12h`). Sessions
  are **stateless** — there is no server-side session store, so a session can't
  be revoked before it expires; `/auth/logout` only clears the cookie in that
  browser. A compromised cookie stays valid until its TTL lapses, so keep the
  TTL modest (and rotate `--oidc-session-secret` to invalidate all sessions at
  once in an emergency).
- `--oidc-secure-cookies` (env `SCRIM_OIDC_SECURE_COOKIES`, default `true`) —
  leave on in production; pass `=false` only for a plain-HTTP local test hub.

**Authentik gotcha:** Authentik's default scope mapping returns
`email_verified: false` unless you fix it per-application. scrim does **not**
gate on `email_verified` — access keys on `sub` alone — so this locks nobody
out and needs no workaround. (If you rely on `email` elsewhere, set Authentik's
provider to emit `email_verified: true`.)

## Version-skew restart

Every self-start check (`add`, `list`, `link`, `open`, or any other verb that talks
to the daemon) compares this CLI binary's own version against the version a
currently-running daemon reports on `/api/status`. A mismatch is treated the
same as a stale/dead daemon: the old one is stopped gracefully and a fresh
one is started transparently — canvases are untouched, since they live on
disk independent of the daemon process. This check is skipped for an
unversioned "dev" build (no `-ldflags` version and no VCS revision), so a
`go run`/`go test` build doesn't restart a real daemon on every invocation.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

MIT
