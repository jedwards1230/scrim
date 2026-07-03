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
```

`link` is the recommended everyday verb for getting a canvas's URL --
especially for an agent, since it can never trigger a browser launch on the
user's machine. Reach for `open` only when you (or, if you're an agent, the
user themself) actually wants the URL opened.

The daemon self-starts on first use of any verb that needs it (`add`,
`link`, `open`, etc.) and idles down after `--idle-timeout` of inactivity.
`snap`/`snaps`/`revert` are pure filesystem operations (like `path`/`rm`'s
fallback) and never self-start the daemon.

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

| Flag             | Env var             | Default     | Description                        |
|------------------|----------------------|-------------|-------------------------------------|
| `--port`         | `SCRIM_PORT`         | `7777`      | Port the daemon listens on          |
| `--host`         | `SCRIM_HOST`         | `127.0.0.1` | Host the daemon binds to            |
| `--idle-timeout` | `SCRIM_IDLE_TIMEOUT` | `30m`       | Idle time before the daemon exits. `0` or negative disables idle exit entirely — the daemon then only stops via `scrim stop` or a signal |
| `--no-auth`      | `SCRIM_NO_AUTH`      | `false`     | Disable local auth token            |
| `--dir`          | `SCRIM_DIR`          | `~/.scrim`  | Directory for canvases + state. A relative value is resolved to an absolute path immediately (against the CLI's cwd), before it's ever handed to a self-started daemon |

Run `scrim --help` (or `scrim <verb> --help`) for the full flag/verb reference.

## Auth & discovery

Every daemon mints a random capability token at startup. The URLs `add`,
`list`, `link`, `open`, and `status` print carry it as `?t=<token>`; the first
request with a valid token sets a cookie so the browser's own follow-up
requests (including live-reload's SSE connection) don't need it repeated.
Requests with neither a valid token nor a valid cookie get 401. Pass
`--no-auth` (or set `SCRIM_NO_AUTH=1`) to disable this entirely.

When `--host` binds beyond loopback, the daemon also advertises itself as
`scrim.local` over mDNS; printed URLs then show both the `scrim.local` form
and the plain `ip:port` fallback, since mDNS can be blocked on some networks.

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
