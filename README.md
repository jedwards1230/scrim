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
| Reads gated by | Capability token → cookie         | CIDR allowlist (+ optional read token)  |
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
- Writes (`POST /api/push/<id>`, and any other non-GET/HEAD `/api/*` call)
  are gated by the push token as a standard bearer credential
  (`Authorization: Bearer <token>`) -- not by the CIDR allowlist, since a
  legitimate push client is commonly outside the read allowlist entirely
  (e.g. a laptop pushing to a homelab hub it isn't itself permitted to
  browse from).
- The hub is long-lived by default (`--idle-timeout` defaults to disabled)
  and doesn't advertise over mDNS by default (`--no-mdns` defaults to true).

`scrim push <id> --to URL --token TOKEN [--watch]` tars a **local** canvas
directory (read straight off disk via `--dir`/`SCRIM_DIR` -- it never talks
to a local daemon) and POSTs it to a hub's push endpoint, printing the
hub's canvas URL on success. `--watch` re-pushes on every local change
(200ms debounce) until interrupted. `push` never launches a browser.

A multi-arch (amd64/arm64) container image running `scrim hub` against a
`/data` volume is published to GHCR on every release:

```bash
docker run -p 7788:7788 -v scrim-hub-data:/data ghcr.io/jedwards1230/scrim:latest \
  --push-token "$(openssl rand -hex 32)" --allow 192.168.1.0/24
```

(Or build it yourself from the repo's `Dockerfile`: `docker build -t scrim-hub .`)

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
