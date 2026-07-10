# Hub: a shared central store

The **hub** is scrim's optional, additive layer for sharing. A localhost-only
user never runs it and sees zero difference — the default daemon path gets no
new behavior, dependencies, or HTTP surface from hub mode (enforced by
`internal/server/hub_test.go`). Reach for a hub only when you want a durable,
network-reachable store that many machines push to and others can browse.

`scrim hub` runs the exact same serving engine as `scrim serve`, but at its own
data directory and port, with a push/read-token + CIDR gate in place of the
local daemon's capability-token auth. It serves its own durable storage at its
own root (`/c/<id>/`), so every URL it produces (SSE, favicon, redirects,
relative paths) is correct with zero rewriting — clients `push` canvases to it
rather than the hub reading from a remote filesystem.

```bash
scrim hub --push-token "$(openssl rand -hex 32)" --allow 192.168.1.0/24
```

## Local vs. hub

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

Both run happily on one box — separate data dirs and ports by design. `push` is
the only bridge between the two, and it's explicit per-canvas.

## Hub flags

- `--data DIR` (env `SCRIM_HUB_DATA`, default `~/.scrim-hub`) — deliberately
  separate from the local daemon's `~/.scrim`, so both can run on one box.
- `--host` defaults to `0.0.0.0` — a hub binds beyond loopback by design; the
  CIDR allowlist below is the read security, not the bind address.
- `--port` (env `SCRIM_PORT`) defaults to `7788` — distinct from the local
  daemon's `7777`.
- `--push-token TOKEN` (env `SCRIM_PUSH_TOKEN`) is **required**: a hub fails
  closed (refuses to start) with no push token, rather than ever running a
  write-accepting server with no write gate.
- `--read-token TOKEN` (env `SCRIM_READ_TOKEN`) is optional, and additionally
  gates reads once the CIDR check below passes.
- `--allow CIDR[,CIDR...]` (env `SCRIM_HUB_ALLOW`) is the read allowlist,
  checked against the client's `RemoteAddr` (never `X-Forwarded-For` — that's
  trivially spoofable; a trusted-proxy layer is a later phase). Defaults to
  loopback-only (`127.0.0.0/8,::1/128`) when unset.
- The push token is a standard bearer credential (`Authorization: Bearer
  <token>`) and is **not** subject to the CIDR allowlist, since a legitimate
  machine client is commonly outside the read allowlist entirely (e.g. a laptop
  or in-cluster MCP server pushing to a hub it isn't itself permitted to browse
  from).
- Because a hub binds beyond loopback, hub mode adds two resource-exhaustion
  guards the local daemon doesn't have: request read timeouts
  (`ReadTimeout` 60s / `IdleTimeout` 120s, so a slow-trickle body can't pin a
  connection forever — SSE responses stay unbounded, `WriteTimeout` is 0) and a
  concurrent SSE (live-reload) connection cap (256 total, 32 per canvas;
  `/c/<id>/__events` returns 503 past the ceiling). Both are hub-only, so the
  local daemon is byte-identical.
- The hub is long-lived by default (`--idle-timeout` defaults to disabled) and
  doesn't advertise over mDNS by default (`--no-mdns` defaults to true).

The push token is **read+write, not write-only**, and is the hub's
admin/bootstrap credential — see the [threat model](threat-model.md#push-token-is-admin-readwrite)
for what that means and how to size its trust.

For OIDC login, per-user tokens, ownership, and sharing, see
[identity.md](identity.md).

## Push

`scrim push <id> --to URL --token TOKEN [--watch]` tars a **local** canvas
directory (read straight off disk via `--dir`/`SCRIM_DIR` — it never talks to a
local daemon) and POSTs it to a hub's push endpoint, printing the hub's canvas
URL on success. The hub extracts it into a staged temp dir (outside the
servable canvases tree) and atomically swaps it into place — one clean
filesystem event, one SSE reload, never a partial-serve.

`TOKEN` is the admin push token or a [user token](identity.md#ownership-sharing--tokens) —
either way the pushed canvas is owned by whichever principal the token resolves
to. `--watch` re-pushes on every local change (200ms debounce) until
interrupted. `push` never launches a browser.

## Container image

A multi-arch (amd64/arm64) container image running `scrim hub` against a
`/data` volume is published to GHCR on every release:

```bash
docker run -p 7788:7788 -v scrim-hub-data:/data ghcr.io/jedwards1230/scrim:latest \
  --push-token "$(openssl rand -hex 32)" --allow 192.168.1.0/24
```

(Or build it yourself from the repo's `Dockerfile`: `docker build -t scrim-hub .`)

Deployment (Kubernetes manifests, ingress/Traefik routing) deliberately lives
outside this repo — the hub itself must stay fully usable standalone.

## Machine API reference

The hub's machine API — the bearer-gated HTTP surface `scrim mcp --hub` drives
(canvas CRUD, per-file read/write/edit, push, copy, and snapshots) — is
documented as a hand-authored OpenAPI 3.1 spec at
[`../api/openapi.yaml`](../api/openapi.yaml). It is the canonical route
reference and is kept current with the handlers (a CI `vacuum lint` gate guards
its validity).

A running hub also serves the spec at `GET /api/openapi.yaml` (embedded in the
binary, gate-exempt, hub-only), so standard OpenAPI tooling can read the
contract straight from a live instance — `curl http://<hub>/api/openapi.yaml`.
Only YAML is served (scrim adds no YAML-to-JSON dependency; modern tools read
YAML natively).

The machine API is gated by the push token on **every** call, reads included —
separate from the browser read gate (CIDR/read-token). File PUTs may carry a
`Content-Encoding: gzip` body and GETs an `Accept-Encoding: gzip` request; the
hub inflates/deflates transparently (the per-file cap applies to the decoded
size).
