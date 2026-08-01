# scrim

@CONTRIBUTING.md

A projection surface for coding agents: a shared self-starting daemon serves
agent-authored HTML canvases with SSE live-reload; a human views them in a
browser.

## Documentation map

`README.md` covers the local product only (add → edit → link, flags, auth). The
hub and its identity plane live under `docs/`: [`docs/hub.md`](docs/hub.md) (hub
server, push, CIDR gate, container, machine API), [`docs/mcp.md`](docs/mcp.md)
(`scrim mcp`: tools, local/hub mode, streamable HTTP, OAuth),
[`docs/identity.md`](docs/identity.md) (OIDC reads, ownership/sharing/tokens,
the trusted-gateway forwarded-identity plane, Authentik feeder),
[`docs/threat-model.md`](docs/threat-model.md) (the hub's three trade-offs), and
[`docs/stability.md`](docs/stability.md) (pre-1.0 policy). The hub machine-API
contract is the OpenAPI spec at [`api/openapi.yaml`](api/openapi.yaml).

## Architecture

`scrim` is a single Go binary with no external services — the CLI and the
daemon are the same binary, dispatching on `os.Args`.

Key packages under `internal/`:

| Package | Responsibility |
|---------|---------------|
| `version` | Build-time version stamping via ldflags |
| `config` | Resolves --dir/--host/--port/--idle-timeout/--no-auth/--no-mdns from flags/env/defaults; derives on-disk paths; enforces owner-only filesystem permissions on --dir/state file/log file with each platform's native primitive (`harden_unix.go`: 0700/0600 mode bits; `harden_windows.go`: an inheritance-protected DACL granting only the owner, the process token user, SYSTEM, and Administrators, via `golang.org/x/sys/windows`) |
| `state` | Daemon state file (`daemon.json`): atomic read/write, corruption handling |
| `canvas` | Canvas directory CRUD, ID validation, per-canvas metadata (title, description, icon) stored externally under `config.Config.MetaDir()`, and deterministic default icon/color derivation from a canvas's ID |
| `apiclient` | Thin HTTP client for the daemon's `/api/*` control surface |
| `daemon` | CLI-side lifecycle: health-check, self-start (with a spawn lock), stop, version-skew restart |
| `server` | The daemon itself: HTTP server, static canvas serving + SSE injection, per-canvas SSE, the card-gallery index page, `/api/*`, per-canvas favicon (agent-authored or generated from the canvas's icon), idle reaper, capability-token auth middleware (redirects a valid query token to a token-stripped URL), mDNS advertisement (opt-out via --no-mdns). Serve-time only (files on disk are never modified): an `index.md` directory-index is rendered via `goldmark`, and a bare HTML fragment (no `<!doctype`/`<html>`) is wrapped -- both in an embedded skeleton (`assets/skeleton.html`: CSS reset, `prefers-color-scheme` theming, viewport meta) before reload-script injection. A complete HTML document passes through unwrapped. Also implements **hub mode** (`NewHub`/`HubOptions`, `hubgate.go`, `handlers_push.go`): the same engine, plus `POST /api/push/<id>` (tar extraction into a staged-then-renamed canvas dir) and `withHubGate`, a gate that replaces `withAuth` in hub mode only. A valid admin push-token bearer authorizes ANY method (the machine-API credential, reads included), and a user token authorizes the machine API bounded to canvases its owner may write; claim, `/api/tokens*`, and mutating a canvas's own `grants*` additionally accept a browser session. Browser reads go to the OIDC session gate when OIDC is configured, and otherwise to a CIDR-allowlist match plus an optional read token (the browser gate) -- the two are exclusive, not layered. Hub mode also exposes a bearer-gated **machine API** for remote MCP clients (`scrim mcp --hub`): reuses `/api/canvases` + `/api/status`, plus `GET /api/canvases/{id}/files` (recursive `{path,size,modified_at}` listing, no content), `GET/PUT/PATCH /api/canvases/{id}/files/{path...}` (atomic temp+rename write, `safeJoin` traversal guard, 2 MiB cap; PUT accepts a `Content-Encoding: gzip` body and GET honors `Accept-Encoding: gzip`, inflated/deflated under the cap via `internal/gzipx`; PATCH = server-side exact-string edit via `internal/fileedit`, single or a transactional `edits` array, conflicts as 409), `POST /api/canvases/{id}/copy` (staged recursive copy via `internal/dircopy` + atomic swap, 409 unless `overwrite` snapshots the target first) and `GET/POST /api/canvases/{id}/snapshots` + `POST .../snapshots/{name}/revert` -- all hub-only routes, so the local daemon gains none of them. A hub also serves its own contract at `GET /api/openapi.yaml` (the hand-authored `api/openapi.yaml` spec, embedded via the root `api` package, gate-exempt so tooling can fetch it token-free). `hub.go` itself (no relation to hub *mode*) is the unrelated SSE client tracker. |
| `snapshot` | Canvas versioning: copy a canvas directory's current contents into a timestamped snapshot, list them, and revert a canvas back to one -- a pure filesystem operation against `config.Config.VersionsDir()`, independent of the daemon |
| `mdns` | Loopback-vs-LAN bind detection, and starting/stopping the `scrim.local` mDNS advertisement (`github.com/hashicorp/mdns`) |
| `logging` | Sole sanctioned logging surface for `server`, plus `config`'s startup permission hardening and `authentik`'s directory feeder (`CategoryConfig`/`CategoryDirectory`): category+error only (no request paths/canvas IDs/tokens ever logged), wraps `http.Server.ErrorLog`. `daemon` does not use it |
| `identity` | The hub's request-time authorization policy: the `Claims` a request carries and the pure `CanView`/`CanWrite` decisions over a canvas's stored owner+grants. No I/O and no IdP calls, so access decisions hold with the IdP unreachable. See [`docs/identity.md`](docs/identity.md) for the plane as a whole |
| `oidc` | Generic OIDC login for hub reads (`github.com/coreos/go-oidc/v3` + `golang.org/x/oauth2`): discovery-driven authorization-code flow with state/nonce/PKCE, the `/auth/login`/`callback`/`logout` routes, and a signed session cookie. IdP-agnostic -- nothing provider-specific in the code |
| `usertoken` | The hub's user-minted bearer tokens: named credentials that act AS their owning principal on the machine plane. Whole-file JSON under the meta dir, SHA-256 hashes only (the raw secret is returned once at mint). The global admin push token deliberately does NOT live here |
| `principal` | Lazily-populated, display-only registry of the principals the hub has seen (logins, verified forwarded actors, grant targets), backing `GET /api/principals` autocomplete. Enforcement NEVER reads it |
| `authentik` | OPTIONAL read-only Authentik users/groups pull that enriches that same autocomplete, behind an in-memory TTL cache. Never persisted, never enforced; an unreachable Authentik degrades autocomplete and nothing else |
| `fileedit` | Exact-string file-edit semantics (occurrence counting, single vs `replace_all`, the edited-size cap) shared by the machine API's `PATCH` and the `edit_file` MCP tool, so the two can't drift. Pure leaf package; every error message is deliberately path-free |
| `gzipx` | Tiny gzip helper shared by the same two callers for the `Content-Encoding`/`gzip+base64` paths. Exists chiefly for `Inflate`'s decompression cap -- one gzip-bomb guard rather than one per inflate site |
| `dircopy` | Bounded recursive directory copy (regular files and directories only, byte + entry caps) behind `POST /api/canvases/{id}/copy`; any other entry type refuses the copy |
| `openurl` | Cross-platform "launch the default browser" (`open`/`xdg-open`/`rundll32 url.dll,FileProtocolHandler`) |
| `pushclient` | Client side of `scrim push`: packs a local canvas directory into an uncompressed tar archive, POSTs it to a hub's push endpoint, and (via `Watch`) debounced-re-pushes on local changes. Self-contained -- does not import `internal/server`, and is imported only by `cli`'s push verb. |
| `mcpserver` | The `scrim mcp` server (`github.com/modelcontextprotocol/go-sdk`): exposes the CLI verbs as MCP tools over stdio (default) or streamable HTTP (`--http ADDR`, binds 127.0.0.1; a non-loopback bind needs `--allow-lan` unless OAuth authenticates it — see below). Dual-mode via a `backend` interface: `localBackend` drives the local daemon + on-disk canvas dir (the SAME `daemon`/`apiclient`/`canvas`/`snapshot` primitives the CLI verbs use); `hubBackend` (`--hub URL`, push-token auth via `SCRIM_PUSH_TOKEN`/`--hub-token-file`, fail-closed) drives a remote hub's machine API over HTTP, with optional `--hub-public-url URL`/`SCRIM_HUB_PUBLIC_URL` as a link-only public base (`hubBackend.linkBase()`) for when the API endpoint isn't browser-reachable — it changes the URLs returned to callers, never where a request goes. `list_files` (recursive path/size listing, no content), `read_file`/`write_file` (inline content, ~2 MiB cap, optional `gzip+base64` encoding for large/binary payloads), `edit_file` (server-side exact-string replacement, single or a transactional `edits` batch, shared semantics in `internal/fileedit` — token cost scales with the change, not the file) and `copy_canvas` (server-side duplication) are the remote-authoring primitives and exist in both modes; `share_canvas`/`list_grants` manage a canvas's view-only sharing grants (user/group/everyone/link; a link grant's secret is returned once); `path` is local-only (absent in hub mode). In hub mode `add`/`list`/`link`/`copy_canvas` build the returned canvas URL from `--hub-public-url` when set, else from the `--hub` value itself — so an in-cluster `--hub` (`http://scrim-hub:7788`) needs the public base set, or it yields links that don't resolve outside the cluster. On the streamable-HTTP transport, `scrim mcp` verifies HMAC-signed `X-Forwarded-User-*` identity headers from a **trusted gateway** (shared secret in `SCRIM_MCP_IDENTITY_HMAC_SECRET`; canonicalization/wire format isolated in `internal/mcpserver/identity.go` — the gateway is any reverse proxy that authenticates the end user and forwards a signed principal in that format) and re-emits the verified principal to the hub as `X-Scrim-Actor-*` on top of the admin bearer, so a canvas is attributed to the real user rather than the shared push token — unset secret ⇒ anonymous ⇒ admin attribution (fail-closed). Orthogonally, `--http` can become an RFC 9728 OAuth 2.0 protected resource (`--oauth-issuer`/`SCRIM_MCP_OAUTH_ISSUER` + `--oauth-audience`, reference AS Authentik; `internal/mcpserver/oauth.go`): unauthenticated protected-resource metadata at `/.well-known/oauth-protected-resource`, per-request bearer-JWT validation (signature/issuer/audience/expiry via the AS JWKS) on `/mcp`, and per-tool `scrim:read`/`scrim:write` scope enforcement (401/403 with `WWW-Authenticate`). It authenticates the CLIENT connection AND, on the same validated-JWT path, derives per-user attribution from the token's `sub`/`email`/`groups`, re-emitted to the hub as the same `X-Scrim-Actor-*` headers — the JWT-derived actor is authoritative (independently verified), so the forwarded-identity HMAC header-trust plane is only the fallback when OAuth is off; stdio stays auth-free. Safety invariants identical either way: `link` returns URLs as data (never a browser), nothing logs URLs/content/tokens, `push` is local + one-shot. |
| `cli` | Verb parsing/dispatch for `add`, `path`, `list`, `link`, `open`, `rm`, `snap`, `snaps`, `revert`, `status`, `stop`, `serve`, `hub`, `push`, `mcp`; prints `?t=<token>`-qualified URLs (and, when mDNS is active, both the `scrim.local` and plain `ip:port` forms). `hub`/`push` are the two verbs that deliberately don't use the shared `commonFlags` (their defaults -- data dir, host, port -- differ on purpose) and don't self-start/talk to a local daemon at all. |

**Version-skew restart.** `internal/daemon.Ensure` compares its own
`internal/version.Short()` against a healthy daemon's reported version on
every self-start check; a mismatch stops that daemon and starts a fresh one
transparently (canvases are untouched -- they live on disk, independent of
the daemon process). The comparison is skipped entirely when the CLI's own
version is the "dev" sentinel (unset `Version` and no VCS revision, e.g. a
binary built outside a git checkout) -- otherwise every unversioned dev
build would restart any real daemon it found on every single invocation.

Data flow: `main.go` dispatches a verb → `cli` either talks to a
running daemon over its local HTTP API or starts one (`daemon`) → the daemon
serves canvases and pushes SSE reloads on file changes (`server`, via
`fsnotify`) → a browser (human) or an agent (`add`/`path`) is the other end.

### Architecture decisions

- **No CGO**: the binary must be cross-compilable without a C toolchain.
- **Dependencies stay minimal**: Go stdlib + `fsnotify` + one mDNS library +
  `goldmark` (serve-time markdown rendering) + the MCP SDK
  (`github.com/modelcontextprotocol/go-sdk`, for `scrim mcp` only) +
  `github.com/coreos/go-oidc/v3` and `golang.org/x/oauth2` (hub OIDC login and
  the MCP OAuth protected-resource mode) +
  `golang.org/x/sys` (Windows-only, for the `config` package's ACL hardening —
  there is no stdlib equivalent and CGO is off the table) only — that is the
  whole `require` block in `go.mod`. Don't add a dependency without a real
  need.
- **Single binary, self-starting daemon**: no separate install/systemd step —
  the first verb that needs the daemon starts it if it isn't running.

### Hub

`scrim hub` is the same serving engine as `scrim serve`, run at its own data
directory (`~/.scrim-hub` by default) and port (`7788`), with
`server.HubOptions`/`withHubGate` replacing the default daemon's
capability-token auth: the admin push-token bearer authorizes the entire
machine API (writes AND reads -- it's read+write, not write-only, since `scrim
mcp --hub` reads canvas/file/snapshot content with it), and a **user token**
minted by a logged-in principal authorizes the same API bounded to canvases
its owner may write. Browser reads require an OIDC session when
`--oidc-issuer` is set, and otherwise a CIDR-allowlist match (loopback-only by
default) plus an optional read token -- the two are exclusive, not layered, and
`scrim hub` warns that `--allow`/`--read-token` are dead config under OIDC.
Bearer-less writes are rejected except on the session plane: canvas claim,
`/api/tokens*`, and mutating a canvas's own `grants*` accept a browser session
cookie (and `POST /api/tokens` rejects a *user-token* bearer with 403 -- minting
is session-only). `scrim push <id>
--to URL --token TOKEN` (backed by `internal/pushclient`) tars a local
canvas and POSTs it to a hub, which extracts it into a staged temp dir
(outside the servable canvases tree) and atomically swaps it into place --
one clean filesystem event, one SSE reload, never a partial-serve. A
`Dockerfile` at the repo root packages `scrim hub` as a container
(`gcr.io/distroless/static-debian12:nonroot`, `/data` volume);
`release.yml` publishes it multi-arch (amd64/arm64) to
`ghcr.io/jedwards1230/scrim` on every semver-labeled release. Deployment
(Kubernetes manifests, ingress/Traefik routing) deliberately lives outside
this repo -- the hub itself must stay fully usable standalone.

**Hard invariant**: the default daemon path (`scrim add`/`serve`/...) gets
zero new behavior, dependencies, or HTTP surface from hub mode --
`server.New`'s `hubCfg` is always nil, the push route is only ever
registered when `NewHub` was used, and `withAuth` (not `withHubGate`, and
no CIDR check) still gates the default daemon exactly as before. Enforced
by `internal/server/hub_test.go`.

## Conventions

### Package organization

All business logic lives under `internal/`. `main.go` stays thin — it only
dispatches to `internal/cli`.

### Adding a new internal package

1. Create `internal/<name>/<name>.go`.
2. Export only the types and functions used by other packages.
3. Write a `<name>_test.go` alongside — table-driven tests preferred.

## Test Isolation

Never point a manual test, script, or e2e run at the real `~/.scrim` (or the
default port `7777`) on a dev machine -- that's a developer's actual running
daemon and live canvases. A `scrim stop` (or `--dir ~/.scrim`) run against it
kills real work, not a fixture. Always use an isolated `--dir`/`SCRIM_DIR`
(e.g. a fresh `t.TempDir()` in Go tests, or a `mktemp -d` in shell) and a
non-default port for anything that starts a daemon; match it in anything new.

How each suite does it: Go tests bind `Port: 0` (the kernel picks a free one)
or never listen at all. `scripts/e2e.sh` gives **every** scenario its own port
via `use_port`, which allocates from a per-run block (base derived from the
run's PID, `SCRIM_E2E_PORT_BASE` to override) and exports it as `SCRIM_PORT`
for that scenario's verbs; `hub`/`push`, which don't take `commonFlags`, call
`alloc_port` and pass `--port` explicitly. None of it touches 7777 -- scenario 1
asserts that directly against the daemon's own state file, so the property is
tested rather than merely intended. New scenarios must call `use_port`, not
inherit a port from an earlier one.

Concurrent runs are *usually* safe, not guaranteed: the per-run block offset is
PID-derived, so two suites can land on the same block (~1/119) and collide. The
allocator narrows the race rather than closing it. A collision fails loudly
(exit 1) and can never produce a false green. Note that **exporting**
`SCRIM_E2E_PORT_BASE` collapses every run in that shell onto one block, which
makes collision certain rather than unlikely -- set it per-invocation if you set
it at all.

## Build Variables

Version info (`Version`, `Commit`, `Date`) is injected into `internal/version`
via `-ldflags` at build time. `make build` handles this automatically. Use
`internal/version.Short()` / `version.Info()` — never hardcode a version
string.
