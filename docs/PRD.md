# scrim — Product Requirements Document

> **Status:** as-realized · 2026-08-22 · owner: justin · repo: [jedwards1230/scrim](https://github.com/jedwards1230/scrim)
> **Supersedes** the pre-build plan `home-orchestration:docs/projects/scrim-prd.md` (2026-07-02),
> written before this repo existed. Where the two disagree, this document is authoritative;
> §14 records the divergences deliberately.
>
> This describes the product **fully realized** — the contract scrim is meant to honor, not a
> task list. Open work lives in GitHub issues; §11 is a thin index into them.

## 1. What it is

`scrim` is a single Go binary that gives a coding agent a **projection surface**: the agent
writes plain HTML (or CSS/JS/assets) into a canvas directory, scrim serves it at a stable URL
with instant live-reload over SSE, and a human watches from a browser. No build step, no
framework, no chat window. Named for the theater fabric you project onto.

The default experience is entirely local: a self-starting loopback daemon on your own machine
with zero setup. An optional **hub** — the same binary in `scrim hub` mode — is a durable,
network-reachable store that canvases are *pushed* to, so work survives the machine that made
it and can be shared with other people. A localhost-only user never touches the hub and sees no
trace of it.

Lineage: scrim distills OpenClaw's canvas plugin (static serve + file-watch + reload-over-socket)
into a standalone tool, dropping the gateway/paired-node machinery it was entangled with.

## 2. Problem

A coding agent can produce a report, a dashboard, a diagram, or a working UI in seconds — and
then has nowhere to put it. The output lands as a wall of markdown in a terminal, or as a file
the human must find and open by hand, re-opening it after every edit. The feedback loop that
makes visual work worth doing is missing.

The 2026-07 survey found nothing that fit: `dvdsgl/claude-canvas` is tmux/TUI-bound;
`mcp-html-sync-server` was unproven; MCP Apps render only inside chat-window hosts; Claude
Desktop Preview is Desktop-only; Artifacts is cloud- and plan-gated. Every option either owned
the rendering surface, required a host that could render it, or lived somewhere the agent's
files did not.

scrim's wager is that the primitive is smaller than any of those: **a directory, a URL, and a
reload event**. The agent already knows how to write files. Everything else is plumbing that
should be invisible to it.

## 3. Users & core workflows

Two user classes, both first-class. The product succeeds only if it serves both at once.

### The coding agent (writer)

The agent must be able to project without thinking about serving. The loop is three steps and
never grows:

```
scrim add report --title "Sales report"   # register canvas → prints its directory + URL
# ...agent writes index.html into the printed directory...
scrim link report                          # print the URL to surface to the human
```

The daemon self-starts on the first verb that needs it. Every file save reloads the browser.
The agent never restarts anything, never picks a port, never manages a process.

Agents that speak MCP skip the shell entirely — `scrim mcp` exposes the same operations as
17 tools against the same code paths (§7.4). An agent running against a remote hub authors with
`write_file`/`edit_file` and never touches a local disk at all.

Two safety rules bound agent behavior, enforced by the surface itself rather than by
convention:

- **An agent must never launch a browser.** `link` prints a URL and *cannot* open anything;
  there is no MCP `open` tool and no flag that changes this. `open --browser` exists for a
  human at their own keyboard. Popping a tab on someone's machine unprompted is a surprise,
  not a convenience.
- **An agent must surface the URL in its own reply** after every `add` and after any major
  update. Verifying the render with the agent's own headless tooling is not a substitute —
  the human cannot see what the agent saw.

### The human (watcher)

The human's job is to *look*. They open one URL and leave the tab there; the page reloads
itself as the agent works. They never run a build, never refresh, never hunt for a file path.

On a hub they get more: a gallery of every canvas at `/`, private by default, with their own
identity attached. They can share one canvas with a person, a group, everyone, or an
unguessable link; claim canvases the machine plane created on their behalf; and mint scoped
tokens so an agent's output is attributed to them rather than to a shared admin credential.

### The operator (hub only)

Someone has to run the hub. That person deploys one container against one volume, sets a push
token, and points an IdP at it. Deployment topology (Kubernetes manifests, ingress) is
deliberately *not* scrim's concern — the hub must remain fully usable as a standalone binary.

## 4. Goals

1. **The agent loop is three steps and never grows.** `add` → write files → `link`. Any feature
   that adds a step to the common path has failed.
2. **Zero setup for the local case.** One binary, no config file, no daemon to install, no port
   to choose. First verb self-starts; idle daemon self-stops.
3. **Secure by default, everywhere.** Loopback bind, capability token always minted, owner-only
   file permissions. Every widening of exposure is an explicit opt-in flag, and every one of
   them fails closed when misconfigured.
4. **Raw web, no framework.** HTML/CSS/JS as authored. No build step, no bundler, no HMR, no
   declarative UI layer. A full `location.reload()` is the whole reload model.
5. **The hub is additive.** The local path gets zero new behavior, dependencies, or HTTP surface
   from hub mode — a hard invariant with a test that enforces it.
6. **Agent-native, not agent-adapted.** The MCP surface is a first-class transport for the same
   operations, not a wrapper bolted onto a CLI.
7. **Small and cross-compilable.** Seven direct dependencies, no CGO, five release platforms.

## 5. Non-goals

| Non-goal | Standing |
|---|---|
| Build steps, bundling, HMR, framework integration | **Firm.** Full-page reload is the model. |
| A declarative UI / A2UI layer | **Firm.** Agents write HTML; that is the point. |
| Rendering verification (screenshot/DOM assert) | **Firm.** The agent's own tooling (Playwright MCP, `curl`) does this. scrim serves; it does not inspect. |
| A reverse-tunnel relay to live local daemons | **Firm — and reaffirmed by construction.** The hub is a *central store* clients push to, not a proxy. This dissolved the URL-rewriting, token-vault, and node-spoofing problems a relay carried. |
| Public-internet exposure by default | **Firm.** LAN/Tailscale + IdP. Anything wider is the environment's job. |
| Deployment manifests (K8s, ingress, Traefik) in this repo | **Firm.** The hub must stay usable standalone. |
| Generated OpenAPI / client codegen | **Firm.** The spec is hand-authored and CI-linted. |
| Browser-driven tests of the reload script | **Firm.** Four lines of JS, already covered by e2e; a real browser would be the flakiest thing in the repo. |
| Load-generation harness (`k6`/`vegeta`) | **Firm.** It would measure GitHub's runners, not scrim. |
| Auto-issue-filing from nightly benchmarks | **Firm.** The machinery would outlive the signal. |
| Markdown rendering | **Reversed, narrowly.** `index.md` renders as a directory-index fallback via goldmark. Per-file `.md` rendering is proposed, not shipped (`jedwards1230/scrim#101`). |
| Snapshot/versioning | **Reversed.** `snap`/`snaps`/`revert` shipped as filesystem versioning. |
| Cross-network sharing | **Reversed.** The hub shipped and is deployed. |
| A trusted-proxy layer consuming validated forwarded addresses | **Deferred**, not refused. Today CIDR is checked on `RemoteAddr` only (§12). |

## 6. Locked product decisions

| Decision | Choice | Why |
|---|---|---|
| Language | Go, stdlib `net/http` + `embed`, no CGO | Must cross-compile without a C toolchain; single static binary is the whole distribution story. |
| Dependencies | 7 direct, and adding one needs a real justification | Every dependency is supply-chain surface on a tool that serves agent-authored content. |
| Process model | Shared self-starting daemon per machine, fixed default port `7777`, canvases at `/c/<id>/` | The agent must never pick a port or manage a process. |
| Lifecycle | Idle spindown is a hard requirement; canvas files persist | No week-old orphan daemons holding ports. An open kiosk tab (live SSE) counts as in-use and prevents reaping. |
| Reload | fsnotify → SSE → full `location.reload()` | No WebSocket, no HMR, no build step. Debounced 200ms, coalesced. |
| Content | Raw HTML/CSS/JS; bare fragments auto-wrapped in a themed skeleton; `index.md` renders | An agent should be able to write a `<div>` and get a readable page. |
| Bind | `127.0.0.1` default; `--host` opts into LAN | Secure by default. |
| Auth (local) | Capability token always minted, embedded in printed URLs (`?t=` → cookie), constant-time compared; `--no-auth` opts out | Trusted-LAN kiosk use is real, but must be the exception you asked for. |
| Auth (hub) | Push token required — the hub refuses to start without one | Write access places content on a trusted domain. Fail closed. |
| Identity (hub) | OIDC discovery-driven and IdP-neutral; canvases private by default with an owner + grants | Never special-case an IdP's URL shape. Audience pinned by value, scope names neutral, keyed on `sub`. |
| Browser launch | `link` (print-only, cannot launch) split from `open` (opt-in `--browser`) | The agent-safe path must be structurally incapable of the unsafe thing. |
| Discovery | mDNS `scrim.local` advertised only when bound beyond loopback | Zero broadcast footprint in the default case. |
| Distribution | `go install`, signed release binaries (5 platforms), GHCR multi-arch image | The binary is the source of truth; the plugin is a thin skill wrapper. |
| Plugin home | The scrim repo hosts its own marketplace (`jedwards1230-scrim`) | Version the skill with the tool it documents, not with an unrelated plugin fleet. |
| Versioning | Pre-1.0; on-disk layout may change between minors; migrations forward-only | Honest about the surface still settling. |

## 7. Product surface

The complete intended contract. This is the lookup section.

### 7.1 CLI verbs

| Verb | Purpose | Self-starts daemon |
|---|---|---|
| `add <id> [--title T] [--desc D] [--icon I]` | Register a canvas; print its directory + URL | yes |
| `path <id>` | Print a canvas's on-disk directory | no (pure filesystem) |
| `list` | List canvases and their URLs | yes |
| `link [<id>]` | Print a canvas URL, or the gallery URL. **Never launches a browser** | yes |
| `open [<id>] [--browser]` | Print the URL; launch a browser only with `--browser` or `SCRIM_OPEN_BROWSER` | yes |
| `rm <id>` | Delete a canvas (via daemon if healthy, else directly) | no |
| `snap <id> [--label L]` | Snapshot the canvas's current contents | no |
| `snaps <id>` | List snapshots, newest first | no |
| `revert <id> [<snap>]` | Restore from a snapshot (latest by default), safety-snapshotting first | no |
| `status` | Daemon health, port, token state | no |
| `stop` | Stop the daemon (waits ≤5s for exit); canvases persist | no |
| `serve` | Run the daemon in the foreground — what self-start re-execs | n/a |
| `hub` | Run the durable shared store (§7.6) | n/a |
| `push <id> --to URL --token T [--watch]` | Tar a local canvas and push it to a hub | no (never touches the local daemon) |
| `mcp [--http ADDR]` | Run the MCP server (§7.4) | lazily, in local mode |

### 7.2 Flags & environment

Common to every local verb:

| Flag | Env | Default |
|---|---|---|
| `--dir DIR` | `SCRIM_DIR` | `~/.scrim` |
| `--host HOST` | `SCRIM_HOST` | `127.0.0.1` |
| `--port PORT` | `SCRIM_PORT` | `7777` |
| `--idle-timeout DUR` | `SCRIM_IDLE_TIMEOUT` | `30m` (≤0 disables idle exit) |
| `--no-auth` | `SCRIM_NO_AUTH` | `false` |
| `--no-mdns` | `SCRIM_NO_MDNS` | `false` |
| — | `SCRIM_OPEN_BROWSER` | unset (`open` only) |

`scrim hub` carries its own flagset: `--data`/`SCRIM_HUB_DATA` (`~/.scrim-hub`), `--host`
(`0.0.0.0`), `--port`/`SCRIM_PORT` (`7788`), `--push-token`/`SCRIM_PUSH_TOKEN` (**required**),
`--read-token`/`SCRIM_READ_TOKEN`, `--allow`/`SCRIM_HUB_ALLOW` (`127.0.0.0/8,::1/128`),
`--idle-timeout` (disabled), `--no-mdns` (on), the `--oidc-*` family (`issuer`, `client-id`,
`client-secret`, `redirect-url`, `scopes`, `session-secret`, `session-ttl`, `secure-cookies`),
and `--authentik-{url,token,cache-ttl}` for the optional directory feeder.

`scrim mcp` adds `--http ADDR`, `--allow-lan`, `--hub URL`, `--hub-public-url`, `--hub-token-file`,
`--oauth-{issuer,audience,resource}`, plus `SCRIM_MCP_IDENTITY_HMAC_SECRET`.

### 7.3 HTTP surface

Registered on every daemon, local and hub:

| Method | Path | Purpose |
|---|---|---|
| GET | `/` | Gallery dashboard |
| GET | `/c`, `/c/` | 302 → `/` |
| GET | `/c/{id}` | 301 → `/c/{id}/` |
| GET | `/c/{id}/__events` | SSE live-reload stream |
| GET | `/c/{id}/favicon.ico` | Static or generated favicon |
| GET | `/c/{id}/{rest...}` | Static canvas serving + reload injection |
| GET | `/api/status` | Health/status JSON |
| GET/POST | `/api/canvases` | List / create |
| DELETE | `/api/canvases/{id}` | Delete |
| POST | `/api/stop` | Graceful shutdown (on a hub, the admin push token is the only accepted credential) |

Hub-only additions — the **machine API**, documented as a hand-authored OpenAPI 3.1 spec at
`api/openapi.yaml` and served at `GET /api/openapi.yaml`:

| Group | Routes |
|---|---|
| files | `GET .../files`, `GET`/`PUT`/`PATCH .../files/{path...}` |
| push/copy | `POST /api/push/{id}`, `POST /api/canvases/{id}/copy` |
| snapshots | `GET`/`POST .../snapshots`, `POST .../snapshots/{name}/revert` |
| grants | `GET`/`POST .../grants`, `DELETE .../grants/{grantRef}` |
| ownership | `POST /api/canvases/{id}/claim` |
| tokens | `GET`/`POST /api/tokens`, `DELETE /api/tokens/{id}`, `GET /tokens` (HTML) |
| principals | `GET /api/principals?q=` (autocomplete; display-only, never an authorization source) |
| ops | `GET /healthz` (gate-exempt), `GET /api/openapi.yaml` (gate-exempt) |
| auth | `GET /auth/login`, `GET /auth/callback`, `POST /auth/logout` (present only under OIDC) |

Documented caps: push archive ≤50 MiB uncompressed / ≤1000 entries / regular files and
directories only; per-file write ≤2 MiB decoded; PATCH body ≤6 MiB; edit conflicts return `409`.

### 7.4 SSE contract

`GET /c/{id}/__events`, `Content-Type: text/event-stream`, `Cache-Control: no-cache`.

- **Reload:** `event: reload\ndata: reload\n\n`, emitted after the per-canvas 200ms debounce fires.
- **Heartbeat:** comment-only `: heartbeat` every 15s.
- **Reconnect:** client-driven; browser `EventSource` handles it. The server holds no reconnect state.
- **Caps (hub only):** 256 connections global, 32 per canvas; over-cap returns `503` before any
  200 header is written. The local daemon is uncapped.
- The connection is registered with the hub *before* response headers are written, so a
  connection count is never observably behind the socket.

### 7.5 MCP tools

17 tools; `path` is local-mode only, so a hub-mode server exposes 16. Every tool carries
annotations, with `ReadOnlyHint` derived from the same scope map that enforces OAuth, so the two
cannot drift.

| Tool | Scope | Purpose |
|---|---|---|
| `add` | write | Register a canvas; returns its view URL |
| `list` | read | List canvases |
| `link` | read | Return a canvas or gallery URL. **Never launches a browser** |
| `path` | read | On-disk directory (local mode only) |
| `rm` | write | Remove a canvas (destructive) |
| `status` | read | Daemon/hub status |
| `snap` / `snaps` / `revert` | write / read / write | Snapshot, list, restore (revert is destructive, safety-snapshots first) |
| `copy_canvas` | write | Server-side duplication; `409` unless `overwrite` |
| `list_files` | read | Paths, sizes, mtimes — no content |
| `read_file` | read | One file inline (≤2 MiB UTF-8), or `gzip+base64` |
| `write_file` | write | Write one file (plain or `gzip+base64`) |
| `edit_file` | write | Exact-string replacement; single or transactional `edits` batch |
| `share_canvas` | write | Grant `user`/`group`/`everyone`/`link`; link returns a one-time secret |
| `list_grants` | read | Owner + current grants, no secrets |
| `push` | write | Pack from the MCP process's own disk and push once to a hub |

Transports: stdio by default; `--http ADDR` for streamable HTTP at `/mcp` (health at `/healthz`).
HTTP binds loopback unless `--allow-lan` or OAuth is configured — it fails closed rather than
silently exposing. There is deliberately **no `open` tool**.

### 7.6 Hub, identity & sharing

The hub runs the identical serving engine at its own data dir and port, with a push/read-token +
CIDR gate replacing the local capability token.

- **Push:** `scrim push` tars a local canvas directory and POSTs it. The hub extracts into a
  staging dir *outside* the servable tree, then atomically swaps it in (move-aside → rename-in →
  delete-aside, with rollback) — one filesystem event, one SSE reload, never a partial serve.
  `--watch` re-pushes on change with a 200ms debounce.
- **Ownership:** every canvas has an owner (a principal's email, or `admin` for bootstrap/legacy
  canvases). Private by default. Writes are owner-or-admin. A logged-in principal can `claim` an
  admin-owned canvas it can see.
- **Grants:** `user` (one email), `group`, `everyone` (any authenticated viewer), `link` (an
  unguessable secret stored only as a hash, shown once, redeemed as `?k=<secret>`). Grants widen
  *visibility* only.
- **User tokens:** a session mints a named bearer token that acts *as* its owner on the machine
  plane, so agent output attributes to a person rather than a shared credential. Tokens carry
  `auto_share` grants and an `allowed_grant_targets` allowance bounding what they may later share.
- **Two identity planes.** *Direct* — a browser session or a user token — carries identity
  natively. *Forwarded* — agent → trusted gateway → `scrim mcp` — carries it as HMAC-signed
  `X-Forwarded-User-*` headers that `scrim mcp` verifies and re-emits as `X-Scrim-Actor-*`. The
  hub trusts those re-emitted headers only when they ride a valid admin push token. An
  OAuth-validated JWT actor is authoritative and takes precedence over the HMAC plane.
- **Storage layout** under the data dir: `canvases/<id>/`, `meta/<id>.json`, `meta/tokens.json`
  (0600), `meta/principals.json`, `versions/<id>/<timestamp>[-label]/`, `push-staging/`.

## 8. Architecture

```
scrim add report ──(state ~/.scrim/daemon.json: pid, host, port, token, version)──┐
                   health-check pid + /api/status; dead/stale/version-skew → spawn ┤
                                                                                   ▼
                                    scrim daemon (same binary, detached, spawn-locked)
                                    ├─ HTTP :7777  /c/<id>/*   static + injected reload script
                                    │              /c/<id>/__events   SSE per canvas
                                    │              /                 gallery
                                    │              /api/*            CLI control plane
                                    ├─ fsnotify on ~/.scrim/canvases/** → 200ms debounce → SSE
                                    ├─ mDNS "scrim.local" (only when host ≠ loopback)
                                    └─ idle reaper: no SSE clients AND no HTTP/CLI activity
                                       for --idle-timeout → clean exit, state file removed
```

- **Self-start** is spawn-lock-coordinated (`daemon.lock`, `O_CREATE|O_EXCL`, stale after 60s)
  so concurrent `scrim add` calls produce exactly one daemon. The winner re-execs the binary as
  `serve`; losers wait and reuse it.
- **Version skew** is treated as staleness: a CLI whose version differs from the running
  daemon's `/api/status` stops it and starts a fresh one. Canvases are untouched. Unversioned
  dev builds skip this so `go run` never disturbs a real daemon.
- **Idle reaping** ticks at `idle-timeout/10` clamped to [250ms, 5s]. Any HTTP request is
  activity; SSE touches on both connect and disconnect, so the clock restarts from actual close.
  A connected SSE client blocks reaping outright.
- **Reload injection**: `.html`/`.htm` responses get a `<link rel="icon">` and the embedded
  four-line reload script inserted before `</body>`. A bare fragment with no doctype is first
  wrapped in a themed skeleton (CSS reset, `prefers-color-scheme`, viewport). Everything is
  served `Cache-Control: no-store` — content is agent-authored and may be sensitive.
- **Serving safety**: symlink-aware path resolution confines every request to the canvas root;
  escapes are rejected, not followed.
- **Permission hardening** runs on every daemon start: `0700`/`0600` on Unix; on Windows, a
  protected DACL blocking inherited access, granting full control to exactly the owner, the
  process user, Administrators, and SYSTEM. Filesystems without ACL support degrade to a
  one-time warning rather than a hard failure.
- **Logging never records a path, canvas ID, or token** — category and error only.
- **Shared primitives, not parallel implementations.** `internal/fileedit`, `internal/snapshot`,
  and `internal/gzipx` back the CLI, the HTTP handlers, and the MCP tools alike, so the three
  protocols cannot drift in semantics.

## 9. Deployment & operations

- **Install:** `go install github.com/jedwards1230/scrim@latest`, or a signed release binary.
- **Releases:** opt-in per PR via a `semver:patch|minor|major` label — no label, no release.
  Each release publishes an immutable tag, cross-built binaries for linux/amd64, linux/arm64,
  darwin/amd64, darwin/arm64, windows/amd64, a `SHA256SUMS` manifest keyless-signed with cosign
  (Sigstore/Fulcio, logged to Rekor), and a multi-arch GHCR image.
- **Container:** distroless-nonroot, `/data` volume, `EXPOSE 7788`, entrypoint `scrim hub`, so
  `docker run <image> --push-token ... --allow ...` appends flags naturally.
- **Local operations:** none. The daemon starts itself and stops itself. `scrim status` reports
  health; `scrim stop` ends it early; canvas files always persist.
- **Hub operations:** long-lived (idle exit disabled), `GET /healthz` for probes — never `/`,
  which correctly 401s a cookie-less prober. Metadata migrations run forward on startup, so an
  upgrade is a restart, not a manual step. Downgrade after a migration is unsupported; snapshot
  `/data` before a major upgrade.
- **Plugin:** the repo hosts marketplace `jedwards1230-scrim`; the skill is a thin wrapper
  (`/plugin marketplace add jedwards1230/scrim`, then `/plugin install scrim@jedwards1230-scrim`)
  that never bundles or self-installs the binary. Its version tracks the plugin-relevant surface,
  not the tool's release version — enforced in CI.
- **Deployment manifests live elsewhere by design.** The homelab runs the hub, an MCP server,
  and an OAuth DCR facade as separate workloads; none of that belongs in this repo.

## 10. Quality bar

A change is complete when all of the following hold. CI enforces each as a job, aggregated by a
`ci` gate job.

| Gate | Command |
|---|---|
| Vet, incl. Windows cross-target | `go vet ./...`; `GOOS=windows GOARCH=amd64\|arm64 go vet`/`build` |
| Tests, race-detected, cross-package coverage | `go test -race -coverprofile -covermode=atomic -coverpkg=./internal/...` |
| End-to-end | `./scripts/e2e.sh` (own parallel job, port-isolated, loud skips) |
| Lint | `golangci-lint` (standard set + `gosec`) |
| Module hygiene | `go mod tidy` + `git diff --exit-code` |
| API spec validity | `vacuum lint --fail-severity error api/openapi.yaml` |
| Vulnerabilities | `govulncheck ./...` |
| Build | `make build` + `./scrim --version` |

**Testing doctrine.** Go is the default; shell e2e is only for what needs a real process, a real
built binary, or real CLI ergonomics — daemon spawn, stale pid, double-start, version-skew
restart, idle self-exit, SIGTERM, browser-launch opt-in. Everything protocol-level (auth
matrices, grant enforcement, SSE payloads, machine API, MCP behavior) belongs in Go.

**Flake resistance is a standing rule, not a cleanup task.** Never assert after a fixed sleep —
poll an observable with a deadline. Assert the invariant, not a transient. Every daemon-starting
test gets its own `--dir` and its own port. No second-granularity timing assertions.

**Regression detection is deterministic.** Allocation-count and complexity-ceiling assertions
(bounded at 10× measured, to catch O(n)→O(n²), not noise) block per-PR; wall-clock benchmarks
run nightly and advisory, read by a human.

**Stability commitments.** Pre-1.0: verbs, flags, and on-disk layout may change between minors.
Canvas *content* is always the user's own and is never rewritten — the metadata, snapshot, and
state *layout* is what a release may change. Migrations are additive, idempotent, and
forward-only. Version skew is handled transparently by restarting the daemon.

**The hub-additivity invariant is a test, not a promise.** `server.New`'s hub config is always
nil on the default path; the push route only ever registers under `NewHub`; `withAuth` — not
`withHubGate`, and no CIDR check — still gates the default daemon. Enforced by
`internal/server/hub_test.go`.

## 11. End state vs today

Shipped is the large majority. This table lists only where intent and reality differ.

| Capability | End-state intent | Status | Tracking |
|---|---|---|---|
| Local daemon, SSE reload, auth, mDNS, snapshots | Complete | shipped | — |
| Hub, push, ownership, grants, user tokens, OIDC, MCP + OAuth | Complete | shipped | — |
| Write-level sharing (multi-editor) | Grants extend past view-only; per-grantee removal | not started | `jedwards1230/scrim#105` |
| Concurrent-write safety | Canvas version + `If-Match` optimistic concurrency | not started | `jedwards1230/scrim#109` |
| Efficient versioning | Content-addressed blobs; debounced auto-snapshot | not started | `jedwards1230/scrim#106`, `#107`, `#108` |
| Snapshot retention | `--prune keep=N`, `snap diff` — today snapshots grow unbounded | not started | `jedwards1230/scrim#45` |
| Canvas lifecycle | Archive tier, opt-in TTL, query/filter/paginate on list | not started | `jedwards1230/scrim#104`, `#103`, `#102` |
| Markdown rendering | Every `.md` gets a styled rendered URL (today: `index.md` only) | partial | `jedwards1230/scrim#101` |
| Diagrams | Native mermaid in the goldmark pipeline | not started | `jedwards1230/scrim#100` |
| Observability | Opt-in Prometheus `/metrics`, counts/gauges only, separate bind | not started | `jedwards1230/scrim#44` |
| Test coverage of load-bearing seams | SSE delivery, daemon spawn, MCP grant handlers at 0% today | not started | `jedwards1230/scrim#78`, `#79`, `#80` |
| Merge protection | `ci` aggregate is advisory — nothing blocks a red merge | **gap** | `jedwards1230/scrim#92` |
| Windows verification | ACL code type-checks but never executes in CI | **gap** | `jedwards1230/scrim#89`, `#90` |
| Spawn-lock race | `spawnLockTimeout` (15s) below worst-case hold (~15.4s) | **bug** | `jedwards1230/scrim#8` |
| e2e tempfile hygiene | Two stderr temp files escape `$WORKDIR` | **bug** | `jedwards1230/scrim#93` |
| Gateway-neutral naming | ContextForge-era identifiers remain post-de-federation | not started | `jedwards1230/scrim#72` |
| MCP spec currency | Adopt MCP `2026-07-28` | not started | `jedwards1230/scrim#71` |

## 12. Risks & accepted limits

**Deliberate trade-offs, each with a mitigation.**

- **The push token is admin — read *and* write.** A holder can read every canvas's bytes, not
  just push. Size its trust accordingly, distribute it narrowly, and rotate it as a
  read-capable secret. A principal wanting a scoped credential mints a user token instead.
  Browser reads stay separately gated so the push token is not the only thing between the
  network and canvas content.
- **CIDR is checked on `RemoteAddr`, never `X-Forwarded-For`.** The allowlist is meaningful only
  about the directly connected peer. Behind a reverse proxy every request arrives from the
  proxy, so the CIDR gate cannot distinguish clients — gate reads with OIDC instead. A
  trusted-proxy layer is a later phase.
- **OIDC sessions are stateless and non-revocable.** Logout clears one browser's cookie; a
  stolen cookie is valid until TTL. Keep `--oidc-session-ttl` modest (12h default); rotating
  `--oidc-session-secret` invalidates every session at once and is the deliberate kill switch.

**Accepted operational limits.**

- **Pre-1.0 layout churn.** On-disk metadata layout may change between minors; downgrade after
  a migration is unsupported. Not every change gets a migration — the v0.1 `.scrim.json` sidecar
  was replaced, not migrated, so upgraded canvases kept content but silently lost title/desc/icon.
- **Snapshots grow without bound.** Full recursive copies, no retention (`#45`). Agent iteration
  accumulates them quickly.
- **Load-bearing seams are untested.** SSE broadcast delivery, the daemon self-start path (the
  main defect surface by design), and the MCP grant handlers sit at 0% coverage. This is known
  and phased (`#78`–`#80`), not overlooked.
- **Known performance ceilings**: the local daemon's SSE connections are uncapped; push latency
  includes an inline delete of the previous canvas under the push lock; token and principal
  stores are whole-file rewrites under one global mutex.
- **Hub cold-boot coupling.** OIDC discovery fails closed at startup, so a hub will not start
  until its IdP is reachable. Correct, but it makes IdP availability a hard dependency.
- **mDNS is blocked on some networks.** The printed `ip:port` always works.
- **A hub-mode MCP server returns the hub's own address as the link base** unless
  `--hub-public-url` is set — an in-cluster URL is dead to a human.
- **No cross-network viewing without a hub.** Tailscale and friends solve this at the
  environment layer, as designed.

## 13. Open decisions

Forks only the owner can settle. Each changes what this document should say.

1. **Windows ACL failure posture** (`jedwards1230/scrim#90`). Hardening currently hard-fails on
   `ERROR_ACCESS_DENIED`, which roaming/redirected profiles can trigger.
   *Options:* (a) keep hard-fail — secure by default, may lock out legitimate corporate profiles;
   (b) degrade to a loud warning like the no-ACL-filesystem path — always starts, may serve from
   a world-readable directory; (c) hard-fail unless an explicit `--allow-weak-permissions` flag
   is passed. **Recommendation: (c)** — preserves the fail-closed default while giving the
   affected user a documented, deliberate escape hatch.

2. **Write-sharing model** (`jedwards1230/scrim#105`). Extending grants past view-only.
   *Options:* (a) an `edit` grant kind alongside the existing four; (b) a per-grant permission
   field (`view`/`edit`) on every kind; (c) defer until optimistic concurrency (`#109`) lands.
   **Recommendation: (c) then (b)** — multi-editor without `If-Match` is silent data loss, and a
   permission field generalizes better than a fifth kind that cannot compose with `link`.

3. **Merge protection** (`jedwards1230/scrim#92`). The `ci` aggregate is advisory; a red PR can merge.
   *Options:* (a) require the `ci` aggregate as the sole status check; (b) require each job
   individually; (c) leave advisory for a solo-maintainer repo. **Recommendation: (a)** — the
   aggregate already exists precisely to be the single required check, and (b) makes adding a
   job a two-place change.

4. **Product scope of the lifecycle trio** (`#102`–`#104`: archive, TTL, list filtering).
   *Options:* (a) build all three as one coherent lifecycle feature; (b) ship `#102` (list
   filtering) alone as an ergonomics fix and leave archive/TTL unbuilt; (c) drop archive/TTL and
   solve growth with snapshot pruning (`#45`) only. **Recommendation: (b) now, (a) later** —
   list filtering pays off at any canvas count, while archive and TTL only matter once the
   collection is genuinely large, which it is not yet.

5. **Fate of the pre-build PRD** in home-orchestration. It is now contradicted on several locked
   decisions by shipped behavior. *Options:* (a) delete it, this document supersedes it;
   (b) keep it with a superseding banner pointing here; (c) leave as-is.
   **Recommendation: (b)** — it is the only record of the 2026-07 alternatives survey and of
   *why* the relay model was rejected, both of which this document cites but does not reproduce.

## 14. Divergences from the 2026-07 plan

Recorded deliberately: the built product wins, but the reversals should be visible.

| 2026-07 decision | What shipped | Read |
|---|---|---|
| "Relay / cross-network: out of scope **forever**" | The hub shipped and is deployed | **Partial reversal.** A *relay* — a reverse tunnel proxying live local daemons — remains firmly out of scope and was rejected on its merits. The hub is a central store clients push to, which is a different design that avoids the problems the relay had. The "forever" applied to the mechanism; the goal was met another way. |
| "Snapshot/verify: out of scope" | `snap`/`snaps`/`revert` shipped | **Split.** Snapshotting shipped as filesystem versioning. *Verification* stayed out of scope exactly as planned — agents still use their own headless tooling. |
| "Raw HTML/CSS/JS only. No markdown" | `index.md` renders via goldmark | **Narrow reversal.** Only as a directory-index fallback; a direct `.md` request still returns raw source. Broader rendering is proposed (`#101`), not assumed. |
| "Plugin is a thin skill wrapper. No MCP" | A 17-tool MCP server ships in the binary; the skill tells agents to prefer it | **Reversal, and the most consequential one.** MCP turned out to be the better agent transport than shelling out. The plugin stayed thin — the MCP server lives in the tool, not the plugin. |
| Plugin home: `repos/claude-plugins/plugins/scrim/` | The scrim repo hosts its own marketplace `jedwards1230-scrim` | **Changed.** Versioning the skill with the tool it documents beat versioning it with an unrelated plugin fleet. |
| "deps ≈ fsnotify + mDNS lib only" | 7 direct dependencies | **Grew as scope grew** — go-oidc, oauth2, the MCP go-sdk, and goldmark are each load-bearing for a feature that did not exist in the plan. The minimalism *policy* held. |
| `scrim open` as the URL verb | `link` (print-only) split from `open` (opt-in `--browser`) | **Refined.** The agent-safe path became structurally incapable of launching a browser rather than merely discouraged from it. |
| No identity model | OIDC login, ownership, grants, user tokens, two-plane attribution, OAuth resource mode | **Additive.** Entirely absent from the plan; the largest single body of work in the repo. |
