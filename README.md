# scrim

A projection surface for coding agents.

`scrim` is a single Go binary: a shared, self-starting daemon serves
agent-authored HTML canvases with live reload over SSE, and a human views them
in a browser. An agent writes (or updates) an HTML file; `scrim` makes it
reachable at a stable URL and pushes reload events when the file changes.

The default experience is entirely local — a loopback daemon on your own
machine, zero setup. An optional [hub](docs/hub.md) adds a shared, remote store
when you want it; a localhost-only user never touches it.

## Install

```bash
go install github.com/jedwards1230/scrim@latest
```

## Quickstart

The everyday loop is **add → edit → link**:

```bash
scrim add report --title "Sales report"   # register a canvas, prints its dir + URL
# → write index.html (or index.md) into the printed directory
scrim link report                          # print the canvas URL to open in a browser
```

`scrim add` starts the daemon if it isn't already running and prints both the
canvas's on-disk directory and its URL. Write a plain `index.html` (or
`index.md`) into that directory — every save triggers a full-page reload in any
open browser tab over SSE, so you never re-run anything to see the next version.
`scrim link` reprints the URL any time.

**The URL carries a `?t=<token>` capability token** — that's expected. Every
daemon mints a random token at startup and every printed URL includes it; the
first request with a valid token sets a cookie and is redirected to the same URL
with the token stripped, so it doesn't linger in your URL bar or history. If you
open a canvas URL *without* the token (e.g. you copied the address bar after the
redirect, or typed the path by hand) you'll get a **401** — that's the auth
working, not a bug. Re-run `scrim link <id>` to get a fresh tokenized URL, or run
with `--no-auth` on a trusted machine to turn tokens off entirely.

`link` is the recommended everyday verb for getting a URL — especially for an
agent, since it can *never* trigger a browser launch on the user's machine. Use
[`open`](#link-vs-open) only when you actually want the URL opened in a browser.

## Verbs

```bash
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
scrim mcp [--http ADDR]       # Serve scrim's verbs as MCP tools (see docs/mcp.md)
```

The daemon self-starts on first use of any verb that needs it (`add`, `list`,
`link`, `open`) and idles down after `--idle-timeout` of inactivity.
`snap`/`snaps`/`revert` (and `path`/`rm`'s fallback) are pure filesystem
operations and never self-start the daemon. Run `scrim --help` (or
`scrim <verb> --help`) for the full reference.

## Gallery dashboard & canvas metadata

The dashboard at `/` is a card gallery: each canvas gets an emoji icon (explicit
via `--icon`, or a deterministic default derived from its ID when omitted), an
accent color (always derived from the ID), its title/description, last-modified
time, and a live SSE viewer count that updates in place via a short client-side
poll of `GET /api/canvases` — no page reload. A canvas's served pages also get a
matching favicon generated from that same icon, unless the canvas itself ships
its own `favicon.ico`.

Canvas metadata (title, description, icon) is stored **outside** the canvas
directory, under `~/.scrim/meta/<id>.json` — not as a sidecar file inside the
canvas directory — since anything under the canvas directory is servable and
filesystem-watched, and metadata must be neither.

## Snapshots

`scrim snap <id> [--label L]` copies a canvas's current contents into
`~/.scrim/versions/<id>/<timestamp>[-<label>]/`. `scrim snaps <id>` lists them,
newest first. `scrim revert <id> [<snapshot>]` replaces the canvas's current
contents with a snapshot's — entirely, not merged — defaulting to the latest
snapshot when none is named; it takes its own `prerevert` snapshot of whatever
was there first, so a revert is itself undoable via another revert.

An `index.md` is rendered to HTML at serve time; raw HTML embedded in it is
passed through unsanitized, the same trust model as a `.html` canvas.

## Flags & environment variables

These are the **local daemon's** flags and defaults. (The [hub](docs/hub.md)
overrides several — data dir, host, port, idle timeout, mDNS — and replaces
`--no-auth` with a push-token/CIDR gate.)

| Flag             | Env var             | Default     | Description                        |
|------------------|----------------------|-------------|-------------------------------------|
| `--port`         | `SCRIM_PORT`         | `7777`      | Port the daemon listens on          |
| `--host`         | `SCRIM_HOST`         | `127.0.0.1` | Host the daemon binds to            |
| `--idle-timeout` | `SCRIM_IDLE_TIMEOUT` | `30m`       | Idle time before the daemon exits. `0` or negative disables idle exit entirely — the daemon then only stops via `scrim stop` or a signal |
| `--no-auth`      | `SCRIM_NO_AUTH`      | `false`     | Disable local auth token            |
| `--no-mdns`      | `SCRIM_NO_MDNS`      | `false`     | Don't advertise over mDNS, even when `--host` binds beyond loopback |
| `--dir`          | `SCRIM_DIR`          | `~/.scrim`  | Directory for canvases + state. A relative value is resolved to an absolute path immediately (against the CLI's cwd), before it's ever handed to a self-started daemon |

## Auth & privacy

Every daemon mints a random capability token at startup and every printed URL
carries it as `?t=<token>` (see [Quickstart](#quickstart) for the token→cookie
redirect and the first-run 401). Requests with neither a valid token nor a valid
cookie get 401. Pass `--no-auth` (or set `SCRIM_NO_AUTH=1`) to disable this
entirely.

When `--host` binds beyond loopback, the daemon also advertises itself as
`scrim.local` over mDNS unless `--no-mdns` (or `SCRIM_NO_MDNS=1`) is set; printed
URLs then show both the `scrim.local` form and the plain `ip:port` fallback,
since mDNS can be blocked on some networks. There's no cross-network relay —
Tailscale or similar handles viewing from off-network.

Beyond the token-stripping redirect: every response carries `Referrer-Policy:
no-referrer`, canvas content responses are marked `Cache-Control: no-store` so
browsers never retain them, and the daemon's own logging never includes a
request path, canvas ID, query string, or the raw token, at any log level.

On Unix-like systems (Linux, macOS), `~/.scrim` (or whatever `--dir` points to)
is tightened to owner-only permissions on every startup (0700 for the directory,
0600 for the state and log files) — even if an older scrim created it looser.
This isn't implemented on Windows yet (`os.Chmod` there only toggles the
read-only attribute); a one-time warning is logged instead of silently claiming
success, tracked in [#19](https://github.com/jedwards1230/scrim/issues/19).

## link vs. open

Both verbs resolve and print the same URL for a canvas (or the dashboard, with
no id) — they only differ in what happens after.

`scrim link [<id>]` is permanently print-only: no flag or environment variable
can make it launch a browser. It's the verb to reach for by default, and the one
an agent should always use.

`scrim open [<id>]` prints the same URL, and can *also* launch it in your
platform's default browser (`open` on macOS, `xdg-open` on Linux, `rundll32
url.dll,FileProtocolHandler` on Windows) — but that launch is **opt-in**, since
scrim's daemon is commonly self-started by an agent on the user's behalf and a
browser tab popping up unprompted is a surprise, not a convenience. By default
`open` just prints the URL plus a one-line stderr hint about how to opt in; pass
`--browser` for a one-off launch, or set `SCRIM_OPEN_BROWSER=1` to make that the
default. When mDNS is active, the URL handed to the browser is the `scrim.local`
one, so it still works when the daemon is bound to an unaddressable host like
`0.0.0.0`. If the launch is requested but fails (e.g. headless environment),
`open` prints a one-line notice on stderr and still exits `0`.

## Version-skew restart

Every self-start check (`add`, `list`, `link`, `open`, or any verb that talks to
the daemon) compares this CLI binary's own version against the version a running
daemon reports on `/api/status`. A mismatch is treated the same as a stale/dead
daemon: the old one is stopped gracefully and a fresh one is started
transparently — canvases are untouched, since they live on disk independent of
the daemon process. This check is skipped for an unversioned "dev" build (no
`-ldflags` version and no VCS revision), so a `go run`/`go test` build doesn't
restart a real daemon on every invocation. See [docs/stability.md](docs/stability.md).

## Hub & remote access

Everything above is the local product. When you want a durable, shared store
that many machines push to and others browse — with OIDC login, per-user tokens,
and canvas ownership/sharing — run a **hub**. It's an optional, additive layer;
the local daemon path gets no new behavior or HTTP surface from it. See:

- **[docs/hub.md](docs/hub.md)** — running a hub, `scrim push`, the CIDR/read-token gate, the container image, and the machine API.
- **[docs/mcp.md](docs/mcp.md)** — the `scrim mcp` server: tools, local vs hub mode, streamable HTTP, OAuth resource mode.
- **[docs/identity.md](docs/identity.md)** — OIDC login, ownership/sharing/tokens, the trusted-gateway identity plane, and the Authentik feeder.
- **[docs/threat-model.md](docs/threat-model.md)** — the hub's three documented security trade-offs and their mitigations.
- **[docs/stability.md](docs/stability.md)** — the pre-1.0 stability policy and on-disk/`/data` upgrade story.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

MIT
