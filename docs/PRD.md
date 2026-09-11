# scrim — Product Requirements Document

> **Status:** as-realized, plus collaboration intent · 2026-09-05 · owner: justin · repo: [jedwards1230/scrim](https://github.com/jedwards1230/scrim)
> **Supersedes and replaces** the pre-build plan `home-orchestration:docs/projects/scrim-prd.md`
> (2026-07-02), written before this repo existed. That document has been **deleted** — everything
> worth keeping from it, including the 2026-07 alternatives survey (§2.1) and the reasoning that
> rejected the relay model (§8.1), is absorbed here. §14 records where the built product diverged
> from that plan.
>
> This describes the product **fully realized** — the contract scrim is meant to honor, not a
> task list. Open work lives in GitHub issues; §11 is a thin index into them.
>
> **How to read a claim here.** Except in §11 and §12, this document describes the *intended* end
> state. Where intent and reality differ the text says so inline and §11 carries the tracking
> issue; anything marked **(not built)** does not exist in the binary today. A sentence with no
> such marker describes shipped behavior.
>
> **Amended 2026-09-05.** The 2026-08-22 document described a product with two users — an agent
> that writes and a human that looks — and treated identity as plumbing for attribution. That
> under-served a third role the hub already creates the moment anything is shared: a
> **collaborator** who did not author the canvas, arrives from a link, and needs to respond. §3
> now names that role, §7.9 specifies the surface it needs, and §13.6–§13.13 record the eight
> decisions taken to close the gap. Almost all of it is **(not built)** — the amendment fixes the
> contract, not the binary.

### Where this sits among scrim's docs

This is the top of the tree: the product contract, and the only document that states intent for
surfaces that do not exist yet. It does not restate the reference docs, and where it disagrees
with one of them, the disagreement is a bug in one of the two — not a layering rule.

| Doc | Holds |
|---|---|
| `docs/PRD.md` (this) | Product contract, end state, locked decisions, intent vs. today |
| [`docs/hub.md`](hub.md) | Operating the hub: deployment, flags, storage, upgrade |
| [`docs/identity.md`](identity.md) | OIDC, ownership, grants, user tokens, the two identity planes |
| [`docs/mcp.md`](mcp.md) | The MCP server: transports, tools, OAuth resource mode |
| [`docs/stability.md`](stability.md) | Pre-1.0 compatibility, migrations, what a release may change |
| [`docs/testing-strategy.md`](testing-strategy.md) | Test doctrine, coverage gaps, the *proposed* benchmark regime (§10) |
| [`docs/threat-model.md`](threat-model.md) | Assets, adversaries, trust boundaries (§12.1) |

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

The hub is also where scrim stops being a single-player tool. Once a canvas can be handed to
someone else, the person receiving it needs more than bytes over HTTP — they need to know what
they are looking at, how it got there, and how to say something back. That surface is specified
in §7.9 and is largely **(not built)**.

Lineage: scrim distills OpenClaw's canvas plugin (static serve + file-watch + reload-over-socket)
into a standalone tool, dropping the gateway/paired-node machinery it was entangled with.

## 2. Problem

A coding agent can produce a report, a dashboard, a diagram, or a working UI in seconds — and
then has nowhere to put it. The output lands as a wall of markdown in a terminal, or as a file
the human must find and open by hand, re-opening it after every edit. The feedback loop that
makes visual work worth doing is missing.

scrim's wager is that the primitive is smaller than anything on offer: **a directory, a URL, and
a reload event**. The agent already knows how to write files. Everything else is plumbing that
should be invisible to it.

That solves the outbound half. The inbound half is still missing: a person looking at the canvas
has nowhere to put a reaction. "The third chart is wrong" travels back through whatever chat
window the link arrived in, gets retyped by hand, and reaches the agent stripped of the context
that made it obvious — the same out-of-band shuffling the outbound half exists to remove. A
projection surface that only projects leaves the human a spectator. The loop closes when the
response travels the same path as the artifact (§7.9).

### 2.1 Alternatives considered and rejected

A survey in 2026-07, before any code was written, found nothing that fit. Recorded here because
the gaps it identified are the requirements scrim exists to meet — a future reader asking "why
not just use X?" should find the answer without archaeology.

**Every verdict below is as surveyed in 2026-07 and is not maintained.** These are third-party
projects that move; the star counts and capability claims were true then and may not be now. They
are kept for the *shape* of the gap each one left, not as current competitive intelligence. The
paragraph after the table is the durable part — re-derive a verdict before relying on one.

| Alternative | Verdict as surveyed 2026-07 |
|---|---|
| [`dvdsgl/claude-canvas`](https://github.com/dvdsgl/claude-canvas) | tmux/TUI-bound. Renders in a terminal, so it cannot show a real web page — the exact thing agents are good at producing. |
| [`yujiosaka/mcp-html-sync-server`](https://github.com/yujiosaka/mcp-html-sync-server) | Unproven (1★, no track record). Wrong risk profile for something in the path of every agent's output. |
| MCP Apps (MCP-ecosystem UI extension; no single repo surveyed) | Renders only inside chat-window hosts. Requires the human to be looking at the chat client, which defeats the "leave a browser tab open on a second screen" workflow. |
| Claude Desktop Preview (Anthropic product) | Desktop-only. Cannot serve a phone, a tablet, a TV, or another machine on the LAN. |
| Artifacts (Anthropic product) | Cloud- and plan-gated. Content leaves the machine, and availability depends on a subscription tier rather than on a binary being on `PATH`. |

The common failure runs through all five: each either **owned the rendering surface** (so the
agent writes into someone else's format), **required a specific host to be watching** (so the
human must be in a particular app), or **lived somewhere the agent's files were not** (so the
files must be uploaded rather than simply written). scrim inverts all three — the agent writes
ordinary files to an ordinary directory, and any browser on any device can watch.

## 3. Users & core workflows

Three user classes, all first-class. The product succeeds only if it serves them at once.

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
17 tools against the same code paths (§7.5). An agent running against a remote hub authors with
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

The moment they use any of that they create the third role below — and every capability in this
paragraph is reached from the *gallery*, which is exactly the page a person arriving from a
shared link has never seen.

### The collaborator (recipient)

The role this document under-served until 2026-09-05, and the one every grant creates. A
collaborator did not author the canvas and may not know what scrim is. They arrive from a link
someone sent them, so **that one URL is the entire product to them**: they will not run the CLI,
will not see the gallery first, and will not read this document.

Their loop mirrors the agent's, and is also three steps:

```
open the link → look → say something back
```

Only the first two work today. **(Not built: the third.)**

Three things this role needs that the 2026-08 design did not provide:

- **Proof of what they are looking at.** A canvas renders as bare agent-authored HTML with a
  reload script and nothing else — no title, no owner, no age, no indication it is a scrim canvas
  at all. It is indistinguishable from a stray file someone emailed.
- **A way in when they are not signed in.** A grant is silent, so their first contact is often a
  URL that bounces them to an IdP login form with no explanation of what they were reaching for.
- **A way to respond in place.** See §7.9. Without it the round trip leaves the product.

A fourth need is a *property* rather than a feature: **their lower privilege must be legible.**
Grants are view-only, but nothing says so — today a grantee discovers it by finding that nothing
they try to change persists.

### The operator (hub only)

Someone has to run the hub. That person deploys one container against one volume, sets a push
token, and points an IdP at it. A Helm chart in this repo packages that for Kubernetes (§9); the
cluster-*specific* topology around it is deliberately not scrim's concern — the hub must remain
fully usable as a standalone binary, chart or no chart.

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
8. **The human's reply is as cheap as the agent's push. (Not built.)** A person who opens a shared
   canvas can respond to it — in place, on the page, in a form the agent that made it can read —
   without leaving the canvas or opening another tool. A feedback surface the human must exit the
   canvas to reach has already lost the round trip goal 1 protects on the writing side.

## 5. Non-goals

| Non-goal | Standing |
|---|---|
| Build steps, bundling, HMR, framework integration | **Firm.** Full-page reload is the model. |
| A declarative UI / A2UI layer | **Firm.** Agents write HTML; that is the point. |
| Rendering verification (screenshot/DOM assert) | **Firm.** The agent's own tooling (Playwright MCP, `curl`) does this. scrim serves; it does not inspect. |
| A reverse-tunnel relay to live local daemons | **Firm — and reaffirmed by construction.** The hub is a *central store* clients push to, not a proxy. This dissolved the URL-rewriting, token-vault, and node-spoofing problems a relay carried. |
| Public-internet exposure by default | **Firm, reaffirmed 2026-09-05** (§13.8). LAN/Tailscale + IdP. Anything wider is the environment's job. |
| An anonymous `public` grant kind | **Firm** (§13.8). `everyone` means every *authenticated* principal; `link` is the widest anonymous reach and stays a bearer secret rather than an open door. A canvas readable with no credential at all makes the hub a publishing platform, which is a different product with a different threat model. |
| Outbound notification (email/SMTP/webhook) when a canvas is shared | **Deferred by decision** (§13.10). Sharing stays silent and the sharer sends the link. A new outbound dependency in a seven-dependency binary needs a stronger case than the one that exists today; revisit once the collaborator flow is real enough to have a complaint. |
| Deployment manifests (K8s, ingress, Traefik) in this repo | **Reversed, narrowly.** A Helm chart ships at `deploy/charts/scrim` — two workloads (`hub`, `mcp`), their ingresses, a NetworkPolicy and a `OnePasswordItem` — CI-gated by `chart-lint` (§10) and published to `oci://ghcr.io/jedwards1230/charts` on every release (§9). What stays out is the cluster-*specific* wiring. The chart is optional packaging: the hub is still fully usable standalone. |
| Generated OpenAPI / client codegen | **Firm.** The spec is hand-authored and CI-linted. |
| Browser-driven tests of the reload script | **Firm.** Four statements of JS, already exercised end-to-end (`scripts/e2e.sh` scenarios 3 and 15 assert a real `event: reload` reaches a real client; 17 and the hub scenario assert the injection itself); a real browser would be the flakiest thing in the repo. |
| Load-generation harness (`k6`/`vegeta`) | **Firm.** It would measure GitHub's runners, not scrim. |
| Auto-issue-filing from benchmark results | **Firm.** Conditional on the benchmark regime in §10 ever being built — **it is not today**. If it is, it reports to a human and never opens issues: the machinery would outlive the signal. |
| Markdown rendering | **Reversed, narrowly.** `index.md` renders as a directory-index fallback via goldmark. Per-file `.md` rendering is proposed, not shipped (`jedwards1230/scrim#101`). |
| Snapshot/versioning | **Reversed.** `snap`/`snaps`/`revert` shipped as filesystem versioning. |
| Cross-network sharing | **Reversed.** The hub shipped and is deployed. |
| A trusted-proxy layer consuming validated forwarded addresses | **Deferred**, not refused. Today CIDR is checked on `RemoteAddr` only (§12). |
| Canvas archive tier and opt-in TTL | **Deferred by decision** (§13.4). List filtering (`#102`) ships alone; archive and TTL only earn their complexity once the collection is genuinely large. |

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
| Permission hardening failure | Hard-fail, unless `--allow-weak-permissions` **(not built)** is passed explicitly | Fail closed by default; never silently downgrade to a world-readable directory, but never strand a user with no recourse either. |
| Write-level sharing | A per-grant `view`/`edit` permission field, gated behind optimistic concurrency landing first | A permission field composes with all four grant kinds; an `edit` kind cannot compose with `link`. Multi-editor without `If-Match` is silent data loss. |
| Canvas chrome **(not built)** | A scrim-owned wrapper page at `/c/<id>/` holding the canvas in a sandboxed iframe; the unwrapped content keeps its own sub-path | Share, history, and comment controls must not run inside agent-authored JS. §12.1 commits to never protecting the viewer from the canvas, so chrome injected into the document would be readable, tamperable, and hideable by the very content it describes. |
| Roles **(not built)** | Three principal roles — `admin`, `member`, `guest` — derived from an IdP claim, never a user list scrim stores itself | Ownership plus a shared admin *token* is an ACL, not a user model: no principal can see everything, reassign an orphaned canvas, or be de-privileged. Deriving from a claim preserves "any user the IdP authenticates is accepted, no list to pre-seed". |
| Public sharing | No anonymous `public` grant; `link` is the widest anonymous reach | See §5 and §13.8. Reversing it would put canvas bytes on the open internet, which the threat model does not cover and the deployment cannot currently serve (§12). |
| Comments **(not built)** | One thread per canvas, each comment stamped with the canvas version it was written against; readable and resolvable by agents over MCP | The version stamp keeps "this looks wrong" meaningful after the next push. Pinning to a DOM node breaks the moment an agent rewrites the page, which is the normal case, not the exception. Agent-readable is what makes this scrim's loop rather than a comment widget. |
| Grantee directory **(not built)** | IdP-neutral and display-only; no provider-specific client | The Authentik feeder was written against the IdP scrim no longer uses and is dead config (§7.2). Rebuilding it against the *next* provider by name would repeat the mistake the "never special-case an IdP" decision above already names. |

### 6.1 What 1.0 means

Everything above is stated pre-1.0, which is a real caveat and not a hedge: §6 says the on-disk
layout may change between minors, and §10 says migrations are forward-only. That licence has to
end somewhere, and this is where. **(Not built — this is the target, not a shipped promise.)**

1.0 is the release at which these freeze under semver, and a break in any of them requires 2.0:

| Surface | Frozen at 1.0 | Still free to change |
|---|---|---|
| CLI | Verb names, their positional arguments, exit-code meanings (§7.7), and the fact that stdout carries only the verb's output | Help text, warning wording, new optional flags, new verbs |
| Flags & env | Every flag and `SCRIM_*` name in §7.2, and its default | Additional flags; a default may change only in a major |
| HTTP | Every path, method, and status in §7.3; the JSON error envelope; the documented caps as *minimums* | Response fields may be added, never removed or retyped; caps may rise |
| SSE | The `reload` event name, the heartbeat form, and reconnect being client-driven | Heartbeat interval, debounce window |
| MCP | Tool names, their required arguments, and their read/write scopes | Descriptions, annotations, optional arguments, new tools |
| On disk | The data-dir shape in §7.6, and that a canvas directory holds exactly what was written into it | Metadata *contents* within a migrated schema |

Two commitments outlive the version number and are **not** waiting on 1.0, because breaking them
is a data-loss bug at any version: **canvas content is never rewritten by scrim**, and **an
upgrade never requires a manual migration step**.

What 1.0 does *not* require: feature completeness. Every **(not built)** item in this document may
still be unbuilt at 1.0. The gate is that the surface has stopped moving, not that it has stopped
growing — plus the merge protection in §13.3 *enforcing*, not merely existing. The ruleset is
configured and requires the `CI` check (§10), so the remaining precondition is narrower than it
was: closing `jedwards1230/scrim#113`, after which a skipped job can no longer satisfy that check.
Until then a frozen contract still rests on a gate a skipped job passes.

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
`client-secret`, `redirect-url`, `post-logout-redirect-url`, `scopes`, `session-secret`,
`session-ttl`, `session-max-lifetime`, `secure-cookies`), and
`--authentik-{url,token,cache-ttl}` for the optional directory feeder.

`--oidc-session-ttl` (default `168h`) is an **idle** window, not an absolute lifetime: using a
session slides its deadline forward, so an active user is never signed out mid-use.
`--oidc-session-max-lifetime` (default `720h`) is the absolute cap from login that renewal can
never cross.

`--oidc-post-logout-redirect-url` is optional and off by default. Set it only to a URL the IdP
has registered as a valid post-logout redirect — an unregistered value is rejected by the
provider — and with it unset the IdP simply shows its own signed-out page. The deployed hub
points it at scrim's own `/logged-out` (§7.3).

**The `--authentik-*` feeder is dead config.** It names the IdP scrim migrated off. The deployed
hub sets none of the three, and there is no Authentik left in the cluster to pull from, so
grantee autocomplete runs on the observed-principal registry alone: you get people the hub has
already seen, and type a full email address for anyone else. Replacing it with an IdP-neutral
source is §13.11.

`scrim mcp` adds `--http ADDR`, `--allow-lan`, `--hub URL`, `--hub-public-url` (`SCRIM_HUB_PUBLIC_URL`),
`--hub-token-file` (falls back to `SCRIM_PUSH_TOKEN`), and `--oauth-{issuer,audience,resource}`
(`SCRIM_MCP_OAUTH_ISSUER` / `_AUDIENCE` / `_RESOURCE`), plus `SCRIM_MCP_IDENTITY_HMAC_SECRET` for
the forwarded-identity plane.

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
| tokens | `GET`/`POST /api/tokens`, `DELETE /api/tokens/{id}`, `GET /tokens` (HTML †) |
| sessions | `GET /api/sessions`, `DELETE /api/sessions/{id}` (session-only) |
| agent connections | `GET /api/agent-connections`, `DELETE /api/agent-connections/{id}` (session-only) |
| principals | `GET /api/principals?q=` (autocomplete; display-only, never an authorization source) |
| ops | `GET /healthz` (gate-exempt), `GET /api/openapi.yaml` (gate-exempt) |
| auth | `GET /auth/login`, `GET /auth/callback`, `POST /auth/logout` (RP-initiated: clears local cookies, then redirects to the IdP's discovered `end_session_endpoint`), `GET /logged-out` (HTML †) — present only under OIDC. `/logged-out` is gate-exempt by exact match and renders no identity, and it is currently the **only** page an anonymous visitor can reach at all, which is exactly §13.9's problem statement. |

† **Outside the spec by design.** `/tokens` and `/logged-out` are server-rendered HTML pages, not
machine API, so `api/openapi.yaml` does not describe them — and §7.7's "`api/openapi.yaml` is
normative" is bounded to the machine surface accordingly. Every other route in the table is in the
spec, `/healthz` and the `/auth/*` trio included.

Documented caps: push archive ≤50 MiB uncompressed / ≤1000 entries / regular files and
directories only; per-file write ≤2 MiB decoded; PATCH body ≤6 MiB; edit conflicts return `409`.

**Intended and (not built)** — the collaboration surface of §7.9. The path *shapes* below are
illustrative; what is decided is that each capability needs an addressable route, not the spelling:

| Group | Routes |
|---|---|
| landing | `GET /` for an unauthenticated visitor — gate-exempt, identity-free, the same disclosure posture as `/logged-out` |
| canvas shell | `GET /c/{id}/` becomes the wrapper page; the unwrapped canvas keeps a sub-path of its own |
| history | A read-only URL per snapshot, so an old version is viewable without reverting to it |
| comments | `GET`/`POST /api/canvases/{id}/comments`, `POST .../comments/{commentID}/resolve`, `DELETE .../comments/{commentID}` |

Roles add no route: a role is a claim on the session, surfaced as a field on `GET /api/principals`
and on the gallery's own render context.

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

17 tools today; `path` is local-mode only, so a hub-mode server exposes 16. Every tool carries
annotations, with `ReadOnlyHint` derived from the same scope map that enforces OAuth, so the two
cannot drift. Two rules bound that map rather than appearing in it: a tool absent from it requires
`scrim:write` (fail closed — a newly added tool is never accidentally callable with a read-only
token), and a `scrim:write` grant satisfies a `scrim:read` requirement, write strictly dominating
read. §6.1 freezes tool scopes at 1.0; these two defaults freeze with them.

Two comment tools are intended and **(not built)**. They would be hub-only — unlike
`share_canvas`/`list_grants`, which register in both modes — so they would take hub mode to 18 and
leave local mode at 17.

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
| `list_comments` | read | Open threads on a canvas, each with the version it was written against. **(Not built)** — hub only, `#129` |
| `resolve_comment` | write | Mark one comment resolved. **(Not built)** — hub only, `#129` |

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
- **Agent connections.** The forwarded plane is also *listed and revocable*, so all three ways
  something reaches a canvas appear on one page. On the OAuth path `scrim mcp` forwards the
  token's client id and `iat` alongside the actor; the hub keeps one record per (subject, client)
  in `internal/agentconn` and refuses a revoked one at the gate. Revocation is scrim-side only —
  it blocks the client here immediately and does **not** delete the IdP grant, so re-authorizing
  (a token issued after the revocation) restores access by design. The page states that bound
  rather than implying more. `scrim mcp` stays stateless: the hub owns the state and the
  enforcement.
- **Roles (not built).** Three roles — `admin`, `member`, `guest` — derived from an IdP claim on
  each request, never stored as a scrim-side user list (§6). `member` is today's behavior and the
  default for anyone the IdP authenticates. `admin` is a *principal* who can see every canvas,
  reassign an orphaned one, and revoke another principal's tokens — the capability the shared
  admin push token has today with nobody's name on it. `guest` may view what is shared with it and
  nothing else: no canvas creation, no token minting, no sharing onward. The role bounds what a
  principal may do; ownership and grants still decide which canvas it may do it to, so this adds a
  ceiling rather than a second ACL.
- **Storage layout** under the data dir: `canvases/<id>/`, `meta/<id>.json`, `meta/tokens.json`
  (0600), `meta/sessions.json` (0600), `meta/agent-connections.json` (0600),
  `meta/principals.json`, `versions/<id>/<timestamp>[-label]/`, `push-staging/`. Comments
  would add `meta/comments/<id>.json` (§7.9).

**Collaboration end state.** Sharing is finished when a canvas can be handed to another person
without the owner thinking about mechanism, *and the person receiving it can act on it*. The
second clause is the 2026-09-05 amendment: the 2026-08 text treated delivery as the whole problem,
so it called sharing nearly done while the recipient still had no way to respond.

Two properties already hold and must keep holding: `group` membership is resolved from the
presented claims at check time (`identity.CanView`), never cached into an authorization decision —
any grantee directory is display-only autocomplete and must stay that way; and an `everyone` grant
requires a genuinely authenticated principal, so a link secret never escalates into one.

What is still missing, all **(not built)** and indexed in §11:

- Grants carrying a `view`/`edit` permission (§6, gated on `#109`, tracked by `#105`).
- The entire collaborator-facing surface of §7.9 — landing page, canvas shell, history, comments.
- Roles, and with them a principal who can act on the canvases of others.
- Any handling of a principal's departure from the IdP. Today an owner who no longer exists leaves
  a canvas writable by nobody but `admin`, and nothing detects or reassigns it. **Roles are the
  prerequisite** — reassignment needs a named principal entitled to perform it, which is precisely
  what the shared admin token is not.

### 7.7 Error contract

The failure surface is product surface: an agent that cannot tell "you asked for the wrong thing"
from "try again in a second" will either retry a permanent error forever or surface a transient
one to the human. Both are defects.

| Plane | Contract |
|---|---|
| CLI | `0` success · `1` runtime failure (daemon unreachable, I/O, remote rejected) · `2` usage error (unknown verb, bad flag, missing argument) · `3` reserved for a credential that is present but rejected. `--help` is `0`, never `2`. Errors go to stderr prefixed `error: `; warnings `warning: `; stdout carries only the verb's output, so `$(scrim path x)` is always safe to substitute. |
| HTTP / machine API | The status carries the meaning: `400` malformed · `401` no/invalid credential · `403` valid credential, not permitted · `404` no such canvas or path · `409` conflict (canvas exists, already owned, stale edit) · `413` over a documented cap · `503` over an SSE connection cap. Handler-level errors use the JSON envelope `{"error": "..."}`; **gate-level denials are `text/plain` by design**, so a client must not assume JSON on a `401`/`403`. `api/openapi.yaml` is normative for which is which, and for the deliberate cases where a read the caller may not see answers `404` rather than `403`. |
| MCP | A user-facing failure returns `IsError` with a readable message (`errorResult`, `mcpserver.go:937`) and a **nil** Go error; a non-nil Go error is reserved for genuine internal faults the SDK surfaces as a protocol error. That split is the contract: an agent can tell "your call was wrong" from "the server broke", and its next move differs. |

**Retry guidance is part of the contract**: `5xx` and connection failures are retryable with
backoff; `4xx` never is, except `409` after a re-read. The HTTP and MCP rows describe shipped
behavior. On the CLI, only exit code `3` is **(not built)** — today a rejected credential exits
`1`, indistinguishable from an unreachable daemon.

### 7.8 Canvas data lifecycle

What the hub owes a canvas over time, independent of the archive/TTL features deferred in §13.4:

- **Nothing is deleted without an explicit act.** No implicit expiry, no LRU eviction, no
  reaping on disk pressure. A canvas that stops being pushed to stays served, verbatim, forever.
- **Deletion is owner-or-admin, and it is complete** — canvas bytes, metadata, grants, and
  snapshots go together. There is no tombstone and no undelete; `snap` before `rm` is the
  recovery story.
- **A push replaces content, never identity.** Owner, grants, and snapshot history survive a
  re-push; the atomic swap means a reader sees the old canvas or the new one, never neither.
  *Descriptive* metadata is the exception and does not inherit that guarantee: title, description,
  and icon are read from the push's own query parameters and written unconditionally, so a re-push
  that omits them clears them.
- **Snapshots are the user's, not the system's.** Nothing prunes them automatically today
  (`#45`), and any future retention must be opt-in per canvas rather than a global default —
  silently discarding a snapshot someone took deliberately is the one unacceptable outcome.
- **The hub is not a backup.** It holds what was pushed. Durability of the *source* is the
  authoring machine's problem, and `/data` is the operator's to back up (§9).

### 7.9 The collaboration surface (not built)

Everything in this section is intent. None of it exists in the binary today, and §11 carries the
tracking issues. It is specified here because the pieces only make sense together: a comment needs
somewhere to live, which needs a page that is not the agent's HTML, which needs a way in for
someone who is not signed in — and all of it needs the recipient to be looking at a version the
owner chose rather than whatever the agent pushed a second ago.

**The problem in one line.** A collaborator's entire contact with scrim is a canvas URL that
serves agent-authored HTML and nothing else. There is no page that says what this is, no way to
see how it got here, and no way to reply.

#### Landing page

An unauthenticated visitor gets an identity-free page at `/` explaining what scrim is, why they
are seeing it, and offering a Log in control — instead of today's immediate redirect into the IdP.
A denied canvas read does the same rather than bouncing: *this canvas was shared with you; sign in
to view it.*

The precedent already exists. `/logged-out` (§7.3) is gate-exempt by exact match, renders no
identity, and reads no session, and its own implementation note states the problem plainly: scrim
has no other page an anonymous visitor can reach. The landing page generalizes that route's
posture rather than inventing one.

**Disclosure rule:** the landing page and any denied-read explainer reveal nothing about whether a
given canvas exists, who owns it, or who it was shared with. "Sign in to continue" is the whole
message. §7.7's deliberate `404`-over-`403` behavior for reads the caller may not see is not
weakened by a friendlier page.

#### Canvas shell

`/c/<id>/` becomes a scrim-owned wrapper page carrying the canvas in a **sandboxed iframe**, with
the unwrapped content still reachable at its own sub-path. The shell holds the title, owner, last
push time, the viewer's own access level, and the entry points to sharing, history, and comments.

The iframe is the security argument, not a layout preference. §12.1 commits to never protecting
the viewer from the canvas, so any chrome injected into the agent's document would be readable and
tamperable by that document — a canvas could hide the comment button, misreport its owner, or read
a comment draft. A separate browsing context is what makes the chrome's claims trustworthy.

**The sandbox itself is blocked on the reload channel (`jedwards1230/scrim#138`).** A sandbox
strong enough to matter — one *without* `allow-same-origin` — puts the canvas on an opaque origin,
and the reload script scrim injects into it can then no longer open its `EventSource` back to
`/c/{id}/__events`: live reload, the product's core loop, breaks. A sandbox *with*
`allow-same-origin` keeps reload working and buys nothing against the threat above, since the
framed document can still reach `parent.document`. Both branches fail, so a first shell
implementation frames the canvas **unsandboxed** and the isolation lands only once the reload
channel stops depending on same-origin (`postMessage` from the shell). The intent above is
unchanged; what is qualified is when it can be honored.

Three constraints this must satisfy:

- **Live reload keeps working inside the frame.** The SSE contract of §7.4 is unchanged; the frame
  reloads itself. The shell is not in the reload path.
- **The raw content stays addressable.** An agent verifying its own output, a `curl`, and any
  deep link into a sub-page must still reach the bytes without the shell.
- **The local daemon gains nothing.** Hub-additivity (§4 goal 5, §10) binds here like everywhere:
  the shell is hub-only, and `internal/server/hub_test.go` must keep passing unchanged.

#### Version history

A history panel lists the canvas's snapshots, and each is viewable **read-only at its own URL**
without reverting to it. Restore stays the existing `revert` primitive, reached from the panel.

This is a UI over machinery that already ships — `snap`/`snaps`/`revert` exist on the CLI, the
machine API, and MCP — so it is not blocked on the storage work in `#106`–`#108`. Those improve
how snapshots are *stored*; this is about a human being able to see one. The two are independent,
and the ordering between them is a scheduling question, not a dependency.

**History is browsing, not sharing.** The panel changes what *the person looking at it* sees, and
nothing else: opening a past version is a private act with no effect on any other viewer, and
`revert` remains the only control that moves the canvas itself. Deciding what a *recipient* sees
is the separate control specified next. The two read the same version list and must agree on how
it is numbered and labelled, but they are not the same affordance and neither implies the other.

#### Shared-version pinning

A share is bound to a **version**, not only to a canvas. The share panel carries a **Shared
version** control: its default is `Latest`, and its alternatives are the canvas's own numbered
versions, each shown with the canvas title as of that version and a relative age — *Version 5 ·
HTPC Stream Postmortem · 17m ago*. Every grant kind of §7.6 is bound the same way; the pin is a
property of the canvas's sharing, not of any one grantee.

**Why this exists.** §7.8 guarantees that a push replaces content, never identity — the URL is
stable and what it serves is whatever the agent pushed most recently. That is exactly right for
the watcher, who wants the newest thing the moment it lands, and exactly wrong for the
collaborator, who opens the link whenever they get to it and can therefore land mid-edit,
mid-refactor, or mid-breakage. Today the owner's only defence is to stop the agent before sharing,
which is a workflow instruction rather than a feature. Pinning is what lets an owner hand over a
stable artifact while the agent keeps working on the same canvas.

**`Latest` is a tracking mode, not a version number.** This is the part most likely to be built
wrong, so it is stated as a requirement rather than left to the implementation: `Latest` must not
be resolved to the newest version at share time and stored as that number. It re-resolves on every
read, so each push changes what a recipient's next load shows, with no act by the owner — that is
what the tooltip means by *everyone with access sees new versions as soon as you publish them*. A
numbered pin is the opposite: frozen through any number of pushes until the owner moves it. The
control has two states, tracking and pinned; it is not one state with a distinguished value.

**Moving the pin is an explicit act, and it is the owner's.** Nobody else can move it, nothing
moves it implicitly, and a `revert` — which changes the canvas rather than the share — does not
silently repoint a pinned share. A recipient sees which version they are on; they cannot change it.
Consistently with §13.10, moving the pin notifies nobody.

**This depends on `jedwards1230/scrim#108`.** Nothing scrim does routinely produces a version.
`snap` is a deliberate act, and the only automatic snapshots are defensive ones taken to make a
destructive operation undoable — `prerevert` before a `revert`, `precopy` before an overwriting
`copy`. The load-bearing half is unchanged: **a push creates nothing**, so the canvas an agent is
actively pushing to accumulates no versions at all, and on today's storage there is usually
nothing to pin to. `jedwards1230/scrim#108` (*versioning 2/3: auto-snapshot on write, debounced*) is what makes a
push produce a version, and without it this control would list the handful of moments someone
happened to run `snap` — a sparse, arbitrary list that is worse than no list, because it looks
authoritative. Shipping the pin ahead of `#108` is therefore not a smaller first step; it is the
feature with its content removed. This is the one part of §7.9 that *is* gated on the versioning
epic — the history panel above is not.

**It also shifts what a version is, and §7.8 should be read accordingly.** §7.8 says snapshots are
the user's, not the system's. Auto-snapshot on write makes that only half true: after `#108` the
version list is mostly system-created, with deliberate `snap` entries mixed in. The retention half
of that guarantee is unchanged and still binding — nothing prunes automatically, and any future
retention is opt-in per canvas, because silently discarding a snapshot someone took deliberately
remains the one unacceptable outcome. What changes is authorship, not durability, and the PRD
records the shift rather than leaving §7.8 quietly contradicted.

**Sequential numbering is a display concern that does not exist today.** Snapshots are directories
named by timestamp (`versions/<id>/<timestamp>[-label]/`, §7.6), so *Version 5* is a rank derived
from that ordering at render time, not an identity stored on disk. Numbers must therefore be
treated as unstable labels — a stored pin references the snapshot, never its ordinal — and the
title shown beside a version is the canvas title as of that version, which means the per-canvas
metadata of §7.6 has to be captured into the snapshot rather than read live.

#### Comments

One thread per canvas. Each comment records its author, its body, and **the canvas version it was
written against**, so "the third chart is wrong" stays interpretable after the agent pushes again;
a comment whose version is no longer current is shown as such rather than silently re-pointed at
new content. Comments are not pinned to DOM nodes: agents rewrite whole pages as the normal case,
and an anchor that survives that is a research project, not a feature.

Storage is `meta/comments/<id>.json`, deleted with the canvas like grants and snapshots (§7.8).

**Agents read and resolve them.** `list_comments` and `resolve_comment` (§7.5) are what make this
scrim's own loop rather than a comment widget: the human comments on the canvas, the agent reads
the thread on its next turn, fixes the thing, pushes, and resolves. Comments a machine cannot see
would push the round trip back out to a chat window, which is the failure §2 describes.

**Visibility follows the canvas.** Anyone who can view a canvas can read its comments; anyone who
can view it can add one, including a `link`-grant viewer, whose comments are attributed to the
anonymous link rather than to a principal. Resolving is owner, admin, or the comment's author.

**Trust boundary.** Comment bodies are user-authored text and are rendered as text, never as
markup — the same rule the existing share dialog already follows for grant targets and owner
labels. A comment is not a second way to inject script into a page.

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
  four-statement reload script inserted before `</body>`. A bare fragment with no doctype is first
  wrapped in a themed skeleton (CSS reset, `prefers-color-scheme`, viewport). **Canvas content**
  is served `Cache-Control: no-store` — it is agent-authored and may be sensitive. (The generated
  favicon and the SSE stream use `no-cache`; SSE's headers are specified in §7.4.)
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

### 8.1 Why the hub is a store, not a relay

The first design for cross-machine viewing (2026-07) was a **relay**: a central reverse-tunnel
proxy that would forward browser requests through to whichever local daemon was actually holding
the canvas. It was rejected in review and replaced with the central-store model that shipped.
The reasoning is recorded here because "just proxy to the live daemon" is the obvious idea, and
a future contributor will propose it again.

A relay fails on four counts at once:

1. **URL rewriting is unbounded.** Proxying someone else's document root means rewriting every
   URL it emits — the SSE reload endpoint, the favicon, the token-strip redirect, and every
   relative asset link in agent-authored HTML. Agents write arbitrary markup; there is no
   finite set of URLs to rewrite, so the proxy is wrong in a way that only shows up on real
   content.
2. **It needs a token vault.** Each local daemon mints its own capability token. To reach them,
   the relay must hold every one of them — turning a convenience feature into the highest-value
   credential store in the system.
3. **Node identity is spoofable.** Tunnel-based designs need a way for a daemon to claim "I am
   machine X." That is an authentication problem the tool would have to solve from scratch,
   with a content-injection payoff for anyone who beats it.
4. **Durability is not delivered.** The canvas is only viewable while the machine that made it
   is awake and connected — which is precisely the problem cross-machine viewing was meant to
   solve.

The central store dissolves all four **by construction, not by mitigation**. The hub serves its
*own* files from its *own* root at `/c/<id>/`, so every URL it generates is correct with no
rewriting; it holds one push credential rather than a vault of borrowed ones; there is no node
identity to spoof because nothing is proxied; and a canvas stays viewable after the machine that
made it is off. The cost — the hub holds what was *pushed*, so local edits diverge until pushed —
is real and accepted, mitigated by `--watch` and by surfacing last-pushed time in the gallery.

This is why §5 lists a reverse-tunnel relay as a **firm** non-goal while cross-network sharing
shipped: the goal was met, the mechanism was not the one originally sketched.

## 9. Deployment & operations

- **Install:** `go install github.com/jedwards1230/scrim@latest`, or a signed release binary.
- **Releases:** opt-in per PR via a `semver:patch|minor|major` label — no label, no release.
  Each release publishes an immutable tag, cross-built binaries for linux/amd64, linux/arm64,
  darwin/amd64, darwin/arm64, windows/amd64, a `SHA256SUMS` manifest keyless-signed with cosign
  (Sigstore/Fulcio, logged to Rekor), a multi-arch GHCR image, and the Helm chart at
  `oci://ghcr.io/jedwards1230/charts/scrim` — its `version`/`appVersion` stamped from the tag, so
  the chart is never versioned by hand.
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
- **The deployment surface is a chart here; the cluster wiring is elsewhere.**
  [`deploy/charts/scrim`](../deploy/charts/scrim/README.md) is one chart with two workloads — `hub`
  (stateful, browser-facing) and `mcp` (stateless, agent-facing) — plus their ingresses, a
  NetworkPolicy, and a `OnePasswordItem`. What deliberately does *not* live here is anything true
  of one cluster and not of the product: the homelab's app-of-apps wiring, its per-cluster values,
  and the OAuth DCR facade it runs alongside these two workloads.
- **What an operator should be able to see** — the end state, **(not built)**, tracked by
  `jedwards1230/scrim#44`. An opt-in Prometheus endpoint on a *separate* bind (never the serving
  port, so metrics are not gated by — or exposed through — the read gate), publishing counts and
  gauges only: canvases, SSE clients global and per canvas, pushes and push failures, gate
  rejections by reason, snapshot count and bytes on disk. Deliberately **no per-canvas label and
  no path or ID in any metric name or value** — the §8 logging rule ("never a path, canvas ID, or
  token") binds metrics identically, and a cardinality explosion keyed on user data would break
  both privacy and Prometheus at once. Until it exists, `GET /api/status` and `/healthz` are the
  whole story.

## 10. Quality bar

A change is complete when all of the following hold. CI enforces every row, aggregated by a `ci`
gate job — though the mapping is not one row to one job: the nine rows below run as seven jobs
(`test`, `e2e`, `lint`, `spec-lint`, `govulncheck`, `build`, `chart-lint`), with vet and the
Windows cross-targets as steps of `test` and module hygiene as a step of `lint`.

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
| Helm chart | `helm lint deploy/charts/scrim`, then `helm template` against both the default values and `ci/homelab-values.yaml`; plus two guard assertions — a `dataDir` outside the mounted volume must fail to render, and so must an `/mcp` ingress with no OAuth issuer |

Coverage is **computed and reported, not gated**: the `test` job prints a total to the job summary
and no threshold fails the build. The number is a signal for a human, and §11 tracks the seams
where it matters.

**Merge protection — configured (§13.3, `jedwards1230/scrim#92`).** The `ci` aggregate is the
**sole required status check** on `main`: it exists to be exactly that, so adding a CI job never
becomes a two-place change.

> **The ruleset is active.** `main` carries an active ruleset with `deletion`, `non_fast_forward`,
> `pull_request`, and `required_status_checks`, and that last rule requires exactly one context —
> `CI`, the `ci` aggregate job at `.github/workflows/ci.yml:278-279`. Decision §13.3 is
> implemented as specified: a PR whose `ci` job *fails* cannot be merged.
>
> **The residual hole is `jedwards1230/scrim#113`.** The aggregate fails only on `failure` or
> `cancelled` (`.github/workflows/ci.yml:287-291`), so a `skipped` need passes it — and passing
> the aggregate is passing the one check the ruleset requires. Merge protection therefore stops a
> *red* build but not a *silently-skipped* one, which is why §6.1 still conditions 1.0 on `#113`.

**Testing doctrine.** Go is the default; shell e2e is only for what needs a real process, a real
built binary, or real CLI ergonomics — daemon spawn, stale pid, double-start, version-skew
restart, idle self-exit, SIGTERM, browser-launch opt-in. Everything protocol-level (auth
matrices, grant enforcement, SSE payloads, machine API, MCP behavior) belongs in Go.

**Flake resistance is a standing rule, not a cleanup task.** Never assert after a fixed sleep —
poll an observable with a deadline. Assert the invariant, not a transient. Every daemon-starting
test gets its own `--dir` and its own port. No second-granularity timing assertions.

**Regression detection should be deterministic — and today none of it exists. (Not built.)**
The repo currently contains **zero benchmarks and no scheduled workflow**; nothing detects a
performance regression, and this paragraph describes the target design, not a gate you can rely
on. [`docs/testing-strategy.md`](testing-strategy.md) works the design through in full and is
explicit that it is a proposal; the two documents must not drift on that point. Tracked by
`jedwards1230/scrim#80`.

The intended shape, when it is built: allocation-count and complexity-ceiling assertions
(bounded at 10× measured, to catch O(n)→O(n²), not noise) block per-PR — those are the half that
actually catches regressions, because they are deterministic; wall-clock benchmarks run nightly
and advisory, read by a human and never auto-filed (§5).

**Stability commitments.** Pre-1.0: verbs, flags, and on-disk layout may change between minors.
Canvas *content* is always the user's own and is never rewritten — the metadata, snapshot, and
state *layout* is what a release may change. Migrations are additive, idempotent, and
forward-only. Version skew is handled transparently by restarting the daemon.

**The hub-additivity invariant is a test, not a promise.** `server.New`'s hub config is always
nil on the default path; the push route only ever registers under `NewHub`; `withAuth` — not
`withHubGate`, and no CIDR check — still gates the default daemon. Enforced by
`internal/server/hub_test.go`.

## 11. End state vs today

This table lists only where intent and reality differ.

The **agent-facing** product is shipped nearly whole. The **collaborator-facing** product added in
the 2026-09-05 amendment (§7.9) is almost entirely unbuilt, and the rows for it are grouped at the
bottom under `#124`. Read the two halves separately: "shipped is the large majority" was true of
the product as scoped on 2026-08-22 and is not true of the product as scoped now.

| Capability | End-state intent | Status | Tracking |
|---|---|---|---|
| Local daemon, SSE reload, auth, mDNS, snapshots | Complete | shipped | — |
| Hub, push, ownership, grants, user tokens, OIDC, MCP + OAuth | Complete | shipped | — |
| Write-level sharing (multi-editor) | Per-grant `view`/`edit` permission field; per-grantee removal. **Blocked on `#109` by decision** (§13.2) | not started | `jedwards1230/scrim#105` |
| Concurrent-write safety | Canvas version + `If-Match` optimistic concurrency | not started | `jedwards1230/scrim#109` |
| Efficient versioning | Content-addressed blobs; debounced auto-snapshot | not started | `jedwards1230/scrim#106`, `#107`, `#108` |
| Snapshot retention | `--prune keep=N`, `snap diff` — today snapshots grow unbounded | not started | `jedwards1230/scrim#45` |
| Canvas discovery | Query/filter/paginate on `list` and `GET /api/canvases` | not started | `jedwards1230/scrim#102` |
| Archive tier / opt-in TTL | **Deferred by decision** (§13.4) — not part of the end state until the collection is large | deferred | `jedwards1230/scrim#104`, `#103` |
| Markdown rendering | Every `.md` gets a styled rendered URL (today: `index.md` only) | partial | `jedwards1230/scrim#101` |
| Diagrams | Native mermaid in the goldmark pipeline | not started | `jedwards1230/scrim#100` |
| Observability | Opt-in Prometheus `/metrics`, counts/gauges only, separate bind | not started | `jedwards1230/scrim#44` |
| Test coverage of load-bearing seams | Cover the five functions still at 0%: `server.hub.broadcast` (SSE fan-out), `daemon.spawnAndWait` + `daemon.detach` (self-start), and the `handleShareCanvas`/`handleListGrants` MCP handlers. Their surrounding code is well covered — `handleSSE` 84%, `withSpawnLock` 86%, the grant backends 86-89%, 79% overall — so this is five specific holes, not an untested subsystem (§12) | not started | `jedwards1230/scrim#78`, `#79`, `#80` |
| Merge protection | `ci` aggregate required as the sole status check on `main` (§13.3). The ruleset is **active** and requires exactly the `CI` context; the residual hole is that a `skipped` need still passes the aggregate (§10) | shipped, less `#113` | `jedwards1230/scrim#92`, `#113` |
| `ci` gate fails open on a skipped job | The aggregate must treat any non-`success` need as a failure; today a `skipped` need passes it. Latent while no job is conditional, load-bearing the moment `#92` makes `ci` the only gate | **bug** | `jedwards1230/scrim#113` |
| Windows verification | ACL code actually executes in CI | **gap** | `jedwards1230/scrim#89` |
| Windows ACL escape hatch | `--allow-weak-permissions` opt-out of the hard-fail (§13.1) | not started | `jedwards1230/scrim#90` |
| Spawn-lock race | `spawnLockTimeout` (15s) below worst-case hold (~15.4s) | **bug** | `jedwards1230/scrim#8` |
| e2e tempfile hygiene | Two stderr temp files escape `$WORKDIR` | **bug** | `jedwards1230/scrim#93` |
| Gateway-neutral naming | ContextForge-era identifiers remain post-de-federation | not started | `jedwards1230/scrim#72` |
| MCP spec currency | Adopt MCP `2026-07-28` | not started | `jedwards1230/scrim#71` |
| **Collaboration surface** (umbrella) | The recipient half of the product: a collaborator can tell what they are looking at, get in, see how it got here, and respond (§7.9) | not started | `jedwards1230/scrim#124` |
| Landing page | An identity-free page an unauthenticated visitor can read, instead of an immediate IdP bounce. Today `/logged-out` is the *only* page they can reach | not started | `jedwards1230/scrim#125` |
| Canvas shell | `/c/<id>/` becomes a scrim-owned wrapper with the canvas in an iframe; **sandboxing it is blocked on the reload-channel rework** (`#138`, §7.9). **Structural dependency** for comments, history, and a legible access level | not started | `jedwards1230/scrim#126` |
| Version history UI | A read-only URL per snapshot, browsable from the shell. `snap`/`snaps`/`revert` ship on CLI, API, and MCP with **zero web UI**; independent of `#106`–`#108` | not started | `jedwards1230/scrim#127` |
| Shared-version pinning | A share is bound to a version, `Latest` (a tracking mode) by default; the owner may freeze it to a numbered one (§13.13). **Depends on `jedwards1230/scrim#108`** — pushes create no snapshots today, so there is nothing to pin to | not started | `jedwards1230/scrim#134` |
| Comments | Canvas-threaded, stamped with the version written against, rendered as text | not started | `jedwards1230/scrim#128` |
| Comments over MCP | `list_comments` + `resolve_comment`, so the human's reply reaches the agent that made the canvas | not started | `jedwards1230/scrim#129` |
| Roles | `admin`/`member`/`guest` from an IdP claim. Today ownership + a shared admin *token* is an ACL, not a user model | not started | `jedwards1230/scrim#130` |
| Owner departure | Detect and reassign canvases whose owner left the IdP. **Blocked on `#130`** — reassignment needs a named principal entitled to do it | not started | `jedwards1230/scrim#131` |
| Grantee directory | IdP-neutral replacement for the `--authentik-*` feeder, which is **dead config** (§7.2) | not started | `jedwards1230/scrim#132` |

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
- **OIDC sessions are server-side and individually revocable** — *reversed 2026-09-08; this bullet
  previously read "stateless and non-revocable", with secret rotation as the only kill switch.*
  Each login is recorded in a registry under the hub's meta dir (`internal/session`), the request
  gate consults it, and `/tokens` lists every browser a principal is signed in from with a sign-out
  control per entry. Signing one out takes effect on that browser's next request, so a copied
  cookie dies with the session rather than living out its TTL. What is recorded is the User-Agent
  string and the timestamps — never an IP address. The costs, accepted deliberately: a fifth piece
  of hub state to keep consistent; an unreadable-or-corrupt registry fails **closed**, refusing
  every session-authenticated request until an operator repairs or removes the file (a *missing*
  file is an empty registry, not a failure — that is a hub's first boot); and the upgrade
  invalidated every pre-existing cookie, so everyone logged in once more. Rotating
  `--oidc-session-secret` still works as the all-at-once lever, and `--oidc-session-ttl` still
  bounds a session that is never signed out — as an **idle** window, with
  `--oidc-session-max-lifetime` as the absolute cap on one that is used continuously. The
  registry is also what makes that sliding window possible: it owns the `created_at` the cap is
  measured from. The admin push token deliberately never consults the
  registry, so it remains the recovery path when the registry itself is broken.
- **An OAuth agent connection is revocable at scrim, and only at scrim.** The MCP clients that
  reach a hub through `scrim mcp`'s OAuth plane are recorded per (principal, OAuth client) and
  listed on `/tokens` with a Revoke control, which blocks that client on its very next request.
  What it deliberately does not do is touch the IdP: the client keeps its refresh token, and a
  token issued after the revocation is admitted — that is how re-authorizing restores access. The
  page says so in as many words rather than implying an IdP-side kill. The same fail-closed rules
  as the session registry apply (corrupt registry refuses the plane, missing registry is empty),
  and the bare admin push token — which carries no forwarded actor — is untouched by every
  revocation, keeping it the recovery path here too.

**Accepted operational limits.**

- **Permission hardening can refuse to start, and today there is no way around it.** On Windows,
  `ERROR_ACCESS_DENIED` while applying the owner-only DACL is fatal by design — roaming and
  redirected profiles can trigger it. `internal/config/harden_windows.go` hard-fails on it
  unconditionally: only a filesystem that cannot do ACLs at all degrades to a warning. **An
  affected user currently has no recourse.** The `--allow-weak-permissions` escape hatch decided
  in §13.1 is **(not built)** — `jedwards1230/scrim#90`. Until it lands, the honest workaround is
  `--dir` pointed at a local (non-redirected) path.
- **Pre-1.0 layout churn.** On-disk metadata layout may change between minors; downgrade after
  a migration is unsupported. Not every change gets a migration — the v0.1 `.scrim.json` sidecar
  was replaced, not migrated, so upgraded canvases kept content but silently lost title/desc/icon.
- **Snapshots grow without bound.** Full recursive copies, no retention (`#45`). Agent iteration
  accumulates them quickly.
- **Five load-bearing functions are untested.** `server.hub.broadcast`, `daemon.spawnAndWait`,
  `daemon.detach`, and the `handleShareCanvas`/`handleListGrants` MCP handlers sit at exactly 0%.
  Note the scope: their neighbours are covered (`handleSSE` 84%, `withSpawnLock` 86%, the grant
  backends 86-89%, 79% overall), so what is missing is fan-out delivery, the fork/exec itself, and
  two tool wrappers — not SSE, spawn, or grants as subsystems. Known and phased (`#78`-`#80`).
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
- **A publicly reachable canvas cannot be logged into.** The MCP door is Cloudflare-proxied and
  reachable from the internet, but the IdP it points at is LAN-only, so an OIDC login can only be
  completed from the LAN or Tailscale. Anyone off-network reaching a shared canvas gets a login
  they cannot finish. A `link` grant is the only sharing path that works for them today, and this
  is the concrete reason §13.8 declines an anonymous `public` grant: widening the *grant* without
  widening the *login* would produce a canvas people can be pointed at but not admitted to.
- **A grant is silent.** Nothing tells the grantee they were given access (§13.10). The sharer
  sends the link out of band, and if they forget, the grant exists and is never used.

### 12.1 Trust boundaries

The full analysis is [`docs/threat-model.md`](threat-model.md); this is the shape the end state
commits to, so a change that moves one of these lines is a product decision and not an
implementation detail.

| Boundary | Trusted side | Untrusted side | Enforced by |
|---|---|---|---|
| Local daemon | The user's own account on that machine | Anything else on the LAN | Loopback bind + capability token; `--host` is the explicit widening |
| Canvas content | Nobody — **agent-authored HTML is never trusted** | The content itself | Served `no-store` from its own path prefix; scrim renders it, never interprets it |
| Hub reads | An authenticated principal, or a CIDR-allowed peer | The internet | OIDC session or CIDR + read token (§12: CIDR sees only the directly connected peer) |
| Hub writes | Admin push token, user token, or browser session | Everything else | `withHubGate`, fail-closed |
| Forwarded identity | A gateway holding the admin push token *and* the HMAC secret | Any header not carrying both | HMAC-signed `X-Forwarded-User-*`, re-emitted as `X-Scrim-Actor-*`; an OAuth JWT actor outranks it |
| The push token | The operator | Every canvas's bytes | Nothing — it is read-and-write admin by design (§12) |
| Canvas chrome **(not built)** | scrim's own wrapper page | The canvas it wraps | A sandboxed iframe (§7.9) — the shell's claims about owner and access level must not be forgeable by the document they describe |

The load-bearing asymmetry: **scrim protects the canvas from the network, and the network from
the daemon — it never protects the viewer from the canvas.** A canvas can contain any script its
author wrote, so sharing one is a decision to run someone's code in your browser. Grants widen
visibility on that understanding, and per-canvas isolation is path-based, not origin-based.

That asymmetry is exactly why the collaboration surface of §7.9 cannot be injected into the
canvas. Every control it adds — who owns this, what may I do here, what did I just comment —
makes a claim the viewer is being asked to believe. A claim rendered inside untrusted content is
a claim the content can rewrite. The iframe does not protect the viewer from the canvas, which
remains a non-goal; it protects **scrim's own statements** from it, which is a different and
narrower promise the product can actually keep.

### 12.2 Scale envelope

What the product is *meant* to serve. Beyond these numbers the design is not wrong, it is simply
untested — and §13.4's judgement that the collection "is not genuinely large" needs a number to
be falsifiable at all.

| Dimension | Design target | Where it bends first |
|---|---|---|
| Canvases per hub | ~1,000 | The gallery renders every canvas unpaginated (`#102`); metadata is one file per canvas |
| Canvases per local daemon | ~100 | Same gallery; one fsnotify watch per canvas directory |
| Concurrent SSE viewers | 256 hub-wide / 32 per canvas (enforced) | The caps themselves — deliberate, and a `503` is the correct answer |
| Canvas size | ≤50 MiB, ≤1,000 entries per push (enforced) | The push cap; an inline delete of the previous canvas holds the push lock |
| Single file | ≤2 MiB (enforced) | The per-file cap |
| Snapshots per canvas | ~100 before it is a problem | Full recursive copies with no retention (`#45`) |
| Principals / user tokens | ~1,000 | Whole-file rewrites under one global mutex (§12) |

These are targets, **not** measured limits — there are no benchmarks (§10), so nothing has
confirmed where the knees actually are. Treat a number here as the point past which someone should
measure before assuming it holds.

## 13. Decisions resolved

Decisions 1–5 were settled 2026-08-22; decisions 6–13 on 2026-09-05, when §3 gained the
collaborator role. No open forks remain in this document; each choice below is binding and is
reflected in the sections it touches.

| # | Question | Choice | Consequence |
|---|---|---|---|
| 1 | Windows ACL failure posture (`jedwards1230/scrim#90`) — hardening hard-fails on `ERROR_ACCESS_DENIED`, which roaming/redirected profiles can trigger | **Hard-fail, unless an explicit `--allow-weak-permissions` flag is passed** | The fail-closed default is preserved; an affected user gets a documented, deliberate escape hatch rather than a silent downgrade. Rejected: keeping the unconditional hard-fail (locks out legitimate corporate profiles with no recourse) and degrading to a warning (may serve from a world-readable directory without the operator ever choosing that). |
| 2 | Write-sharing model (`#105`) — extending grants past view-only | **Defer until optimistic concurrency (`#109`) lands, then add a per-grant `view`/`edit` permission field** — not a fifth grant kind | Multi-editor without `If-Match` is silent data loss, so ordering is load-bearing, not preference. A permission field composes with all four existing kinds; an `edit` kind cannot compose with `link`. |
| 3 | Merge protection (`#92`) — the `ci` aggregate is advisory, so a red PR can merge | **Require the `ci` aggregate as the sole required status check** | The aggregate job exists precisely to be the single required check. Requiring each job individually would make adding a CI job a two-place change and drift silently. |
| 4 | Scope of the lifecycle trio (`#102`–`#104`) | **Ship `#102` (query/filter/paginate) alone; archive and TTL stay unbuilt** | List filtering pays off at any canvas count. Archive and TTL only earn their complexity once the collection is genuinely large, which it is not. Revisit when it is. |
| 5 | Fate of the pre-build PRD in home-orchestration | **Delete it** | This document supersedes it as the product contract — the top of scrim's doc tree, above the six reference docs mapped in the header, not a replacement for them. Its irreplaceable content — the 2026-07 alternatives survey and the relay rejection — is absorbed into §2.1 and §8.1, so the deletion loses nothing. |
| 6 | Does the hub need a role model? (`#130`) | **Yes — `admin`/`member`/`guest`, derived from an IdP claim, never a scrim-side user list** | Ownership plus a shared admin *token* is an ACL, not a user model: nobody can see everything, reassign an orphan, or be de-privileged. Claim-derived keeps "no user list to pre-seed" and keeps the IdP the source of truth. Rejected: an admin role alone (leaves no lesser tier for a recipient who should not create canvases) and keeping ownership-only (leaves `#131` unbuildable, since reassignment needs a named principal entitled to do it). |
| 7 | Where does human-facing chrome live? (`#126`) | **A scrim-owned wrapper page at `/c/<id>/` with the canvas in a sandboxed iframe** | §12.1 commits to never protecting the viewer from the canvas, so chrome injected into the agent's document would be readable, tamperable, and hideable by that document — a canvas could misreport its own owner. The iframe protects *scrim's statements*, not the viewer. Rejected: injecting a toolbar like the reload script (untrustworthy for exactly that reason) and gallery-only controls (invisible to a recipient arriving from a link, who has never seen the gallery). |
| 8 | Should there be an anonymous `public` grant? (§5) | **No — `link` is the widest anonymous reach** | A canvas readable with no credential makes the hub a publishing platform, with a threat model scrim has not written. It is also unbuildable today: the deployment's IdP is LAN-only, so widening the grant without widening the login yields a canvas people can be pointed at but not admitted to (§12). The §5 non-goal is reaffirmed rather than reversed. |
| 9 | What does an unauthenticated visitor see? (`#125`) | **A hub landing page — identity-free, with a Log in control** | Today every human-facing route bounces an anonymous browser into the IdP, so a silently-granted recipient meets a login form with no idea what they were reaching for. `/logged-out` already proves the pattern is safe: gate-exempt by exact match, no session read, nothing disclosed. Rejected: keeping the bare redirect (the recipient's first impression is an unexplained login) and an explainer only on a denied canvas (leaves `/` still unreachable). |
| 10 | Is a grantee notified when a canvas is shared? (§5) | **No, for now** | Sharing stays silent and the sharer sends the link. SMTP is an outbound dependency and a new failure mode in a seven-dependency binary, and an in-app inbox is visible only to someone who already logs in — which is the person who least needs telling. Revisit when the collaborator flow is real enough to generate the complaint. |
| 11 | Fate of the Authentik grantee directory (`#132`) | **Replace it with an IdP-neutral source; keep it display-only** | It names the IdP scrim migrated off and is dead config — the deployed hub sets none of its three variables and there is no Authentik left to pull from (§7.2). Rebuilding it against Keycloak by name would repeat the mistake the "never special-case an IdP" decision in §6 already names. Rejected: deleting the feeder outright (makes the first share to a new person a typing exercise, at exactly the moment the flow should be smoothest). |
| 12 | What is a comment anchored to, and who can read it? (`#128`, `#129`) | **One thread per canvas, each comment stamped with the version it was written against; agents read and resolve them over MCP** | The version stamp keeps "this looks wrong" interpretable after the next push. Pinning to a DOM node breaks the moment an agent rewrites the page, which is the normal case rather than the exception — an anchor that survives that is a research project. Agent-readability is the half that makes this scrim's own loop: comments a machine cannot see push the round trip back out to a chat window, which is the failure §2 describes. Rejected: DOM-pinned comments, and a human-only thread. |
| 13 | What version does a recipient see? (`#134`) | **A share is pinned to a version, defaulting to `Latest` — a tracking mode that re-resolves on every read, not a stored pointer to the newest snapshot.** Gated on `jedwards1230/scrim#108` | §7.8 makes a push replace content under a stable URL, so a shared link shows whatever the agent pushed most recently and a recipient can open a canvas mid-edit; the pin is how an owner hands over a stable artifact while the agent keeps working. `Latest` must stay a mode rather than a resolved number, or the default silently becomes a freeze. It is gated because auto-snapshot on write (`#108`) is what makes a push produce a version at all — without it the list shows only the moments someone ran `snap`, which is arbitrary rather than useful. Auto-created versions mean §7.8's "snapshots are the user's" now covers a mostly system-created list; its retention guarantee is untouched. Rejected: latest-only with history as a view-only panel (leaves the owner no way to stabilise what a recipient sees, which is the actual complaint) and a publish/draft model where pushes land on a draft the owner promotes (turns every push into a two-step for the agent-only case that is the overwhelming majority, and gives the watcher of §3 a stale canvas by default). |

## 14. Divergences from the 2026-07 plan

Recorded deliberately: the built product wins, but the reversals should be visible.

| 2026-07 decision | What shipped | Read |
|---|---|---|
| "Relay / cross-network: out of scope **forever**" | The hub shipped and is deployed | **Partial reversal.** A *relay* — a reverse tunnel proxying live local daemons — remains firmly out of scope and was rejected on its merits. The hub is a central store clients push to, which is a different design that avoids the problems the relay had. The "forever" applied to the mechanism; the goal was met another way. |
| "Snapshot/verify: out of scope" | `snap`/`snaps`/`revert` shipped | **Split.** Snapshotting shipped as filesystem versioning. *Verification* stayed out of scope exactly as planned — agents still use their own headless tooling. |
| "Raw HTML/CSS/JS only. No markdown" | `index.md` renders via goldmark | **Narrow reversal.** Only as a directory-index fallback; a direct `.md` request still returns raw source. Broader rendering is proposed (`#101`), not assumed. |
| "Plugin is a thin skill wrapper. No MCP" | A 17-tool MCP server ships in the binary; the skill tells agents to prefer it | **Reversal, and the most consequential one.** MCP turned out to be the better agent transport than shelling out. The plugin stayed thin — the MCP server lives in the tool, not the plugin. |
| Plugin home: `repos/claude-plugins/plugins/scrim/` | The scrim repo hosts its own marketplace `jedwards1230-scrim` | **Changed.** Versioning the skill with the tool it documents beat versioning it with an unrelated plugin fleet. |
| "deps ≈ fsnotify + mDNS lib only" | 7 direct dependencies | **Grew as scope grew** — the planned two (fsnotify, mdns) plus five that are each load-bearing for a feature the plan did not have: go-oidc and oauth2 (identity), the MCP go-sdk (§7.5), goldmark (`index.md`), and `golang.org/x/sys` (the Windows owner-only DACL). The minimalism *policy* held. |
| `scrim open` as the URL verb | `link` (print-only) split from `open` (opt-in `--browser`) | **Refined.** The agent-safe path became structurally incapable of launching a browser rather than merely discouraged from it. |
| No identity model | OIDC login, ownership, grants, user tokens, two-plane attribution, OAuth resource mode | **Additive.** Entirely absent from the plan; the largest single body of work in the repo. |
