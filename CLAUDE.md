# scrim

@CONTRIBUTING.md

A projection surface for coding agents: a shared self-starting daemon serves
agent-authored HTML canvases with SSE live-reload; a human views them in a
browser.

## Architecture

`scrim` is a single Go binary with no external services — the CLI and the
daemon are the same binary, dispatching on `os.Args`.

Key packages under `internal/`:

| Package | Responsibility |
|---------|---------------|
| `version` | Build-time version stamping via ldflags |
| `config` | Resolves --dir/--host/--port/--idle-timeout/--no-auth/--no-mdns from flags/env/defaults; derives on-disk paths; enforces owner-only filesystem permissions on --dir/state file/log file on Unix (Windows lacks an equivalent primitive, logged as a one-time warning instead of claiming success -- tracked in #19) |
| `state` | Daemon state file (`daemon.json`): atomic read/write, corruption handling |
| `canvas` | Canvas directory CRUD, ID validation, per-canvas metadata (title, description, icon) stored externally under `config.Config.MetaDir()`, and deterministic default icon/color derivation from a canvas's ID |
| `apiclient` | Thin HTTP client for the daemon's `/api/*` control surface |
| `daemon` | CLI-side lifecycle: health-check, self-start (with a spawn lock), stop, version-skew restart |
| `server` | The daemon itself: HTTP server, static canvas serving + SSE injection, per-canvas SSE, the card-gallery index page, `/api/*`, per-canvas favicon (agent-authored or generated from the canvas's icon), idle reaper, capability-token auth middleware (redirects a valid query token to a token-stripped URL), mDNS advertisement (opt-out via --no-mdns). Serve-time only (files on disk are never modified): an `index.md` directory-index is rendered via `goldmark`, and a bare HTML fragment (no `<!doctype`/`<html>`) is wrapped -- both in an embedded skeleton (`assets/skeleton.html`: CSS reset, `prefers-color-scheme` theming, viewport meta) before reload-script injection. A complete HTML document passes through unwrapped. Also implements **hub mode** (`NewHub`/`HubOptions`, `hubgate.go`, `handlers_push.go`): the same engine, plus `POST /api/push/<id>` (tar extraction into a staged-then-renamed canvas dir) and `withHubGate`, a gate that replaces `withAuth` in hub mode only. A valid push-token bearer authorizes ANY method (the machine-API credential, reads included); absent a bearer, writes are rejected and reads fall to a CIDR-allowlist match plus an optional read token (the browser gate). Hub mode also exposes a bearer-gated **machine API** for remote MCP clients (`scrim mcp --hub`): reuses `/api/canvases` + `/api/status`, plus `GET /api/canvases/{id}/files` (recursive `{path,size,modified_at}` listing, no content), `GET/PUT/PATCH /api/canvases/{id}/files/{path...}` (atomic temp+rename write, `safeJoin` traversal guard, 2 MiB cap; PUT accepts a `Content-Encoding: gzip` body and GET honors `Accept-Encoding: gzip`, inflated/deflated under the cap via `internal/gzipx`; PATCH = server-side exact-string edit via `internal/fileedit`, single or a transactional `edits` array, conflicts as 409), `POST /api/canvases/{id}/copy` (staged recursive copy via `internal/dircopy` + atomic swap, 409 unless `overwrite` snapshots the target first) and `GET/POST /api/canvases/{id}/snapshots` + `POST .../snapshots/{name}/revert` -- all hub-only routes, so the local daemon gains none of them. A hub also serves its own contract at `GET /api/openapi.yaml` (the hand-authored `api/openapi.yaml` spec, embedded via the root `api` package, gate-exempt so tooling can fetch it token-free). `hub.go` itself (no relation to hub *mode*) is the unrelated SSE client tracker. |
| `snapshot` | Canvas versioning: copy a canvas directory's current contents into a timestamped snapshot, list them, and revert a canvas back to one -- a pure filesystem operation against `config.Config.VersionsDir()`, independent of the daemon |
| `mdns` | Loopback-vs-LAN bind detection, and starting/stopping the `scrim.local` mDNS advertisement (`github.com/hashicorp/mdns`) |
| `logging` | Sole sanctioned logging surface for `server`/`daemon`: category+error only (no request paths/canvas IDs/tokens ever logged), wraps `http.Server.ErrorLog` |
| `openurl` | Cross-platform "launch the default browser" (`open`/`xdg-open`/`rundll32 url.dll,FileProtocolHandler`) |
| `pushclient` | Client side of `scrim push`: packs a local canvas directory into an uncompressed tar archive, POSTs it to a hub's push endpoint, and (via `Watch`) debounced-re-pushes on local changes. Self-contained -- does not import `internal/server`, and is imported only by `cli`'s push verb. |
| `mcpserver` | The `scrim mcp` server (`github.com/modelcontextprotocol/go-sdk`): exposes the CLI verbs as MCP tools over stdio (default) or streamable HTTP (`--http ADDR`, binds 127.0.0.1, non-loopback needs `--allow-lan`, unauthenticated pending #33). Dual-mode via a `backend` interface: `localBackend` drives the local daemon + on-disk canvas dir (the SAME `daemon`/`apiclient`/`canvas`/`snapshot` primitives the CLI verbs use); `hubBackend` (`--hub URL`, push-token auth via `SCRIM_PUSH_TOKEN`/`--hub-token-file`, fail-closed) drives a remote hub's machine API over HTTP. `list_files` (recursive path/size listing, no content), `read_file`/`write_file` (inline content, ~2 MiB cap, optional `gzip+base64` encoding for large/binary payloads), `edit_file` (server-side exact-string replacement, single or a transactional `edits` batch, shared semantics in `internal/fileedit` — token cost scales with the change, not the file) and `copy_canvas` (server-side duplication) are the remote-authoring primitives and exist in both modes; `share_canvas`/`list_grants` manage a canvas's view-only sharing grants (user/group/everyone/link; a link grant's secret is returned once); `path` is local-only (absent in hub mode). On the streamable-HTTP transport, `scrim mcp` verifies HMAC-signed `X-Forwarded-User-*` identity headers (shared secret in `SCRIM_MCP_IDENTITY_HMAC_SECRET`; canonicalization isolated in `internal/mcpserver/identity.go`, reconciled against ContextForge in #48) and re-emits the verified principal to the hub as `X-Scrim-Actor-*` on top of the admin bearer, so a canvas is attributed to the real user rather than the shared push token — unset secret ⇒ anonymous ⇒ admin attribution (fail-closed). Safety invariants identical either way: `link` returns URLs as data (never a browser), nothing logs URLs/content/tokens, `push` is local + one-shot. |
| `cli` | Verb parsing/dispatch for `add`, `path`, `list`, `link`, `open`, `rm`, `snap`, `snaps`, `revert`, `status`, `stop`, `serve`, `hub`, `push`, `mcp`; prints `?t=<token>`-qualified URLs (and, when mDNS is active, both the `scrim.local` and plain `ip:port` forms). `hub`/`push` are the two verbs that deliberately don't use the shared `commonFlags` (their defaults -- data dir, host, port -- differ on purpose) and don't self-start/talk to a local daemon at all. |

Phase 3 (auth via the state file's `token`/`no_auth` fields, mDNS
advertisement) and Phase 4 (`open` launching a browser, version-skew
restart) are both built. `internal/daemon.Ensure` compares its own
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
  (`github.com/modelcontextprotocol/go-sdk`, for `scrim mcp` only) only. Don't
  add a dependency without a real need.
- **Single binary, self-starting daemon**: no separate install/systemd step —
  the first verb that needs the daemon starts it if it isn't running.

### Hub (Phase 1)

`scrim hub` is the same serving engine as `scrim serve`, run at its own data
directory (`~/.scrim-hub` by default) and port (`7788`), with
`server.HubOptions`/`withHubGate` replacing the default daemon's
capability-token auth: a push-token bearer authorizes the entire machine API
(writes AND reads -- it's read+write, not write-only, since `scrim mcp --hub`
reads canvas/file/snapshot content with it); absent a bearer, writes are
rejected and browser reads require a CIDR-allowlist match (loopback-only by
default) plus an optional read token. `scrim push <id>
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
dispatches to `internal/cli` (once that package exists).

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
non-default `SCRIM_PORT` (a high, unlikely-to-collide port) for anything that
starts a daemon -- `scripts/e2e.sh` and every test in this repo already follow
this; match it in anything new.

## Build Variables

Version info (`Version`, `Commit`, `Date`) is injected into `internal/version`
via `-ldflags` at build time. `make build` handles this automatically. Use
`internal/version.Short()` / `version.Info()` — never hardcode a version
string.
