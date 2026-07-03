# scrim

A projection surface for coding agents.

`scrim` is a single Go binary: a shared, self-starting daemon serves
agent-authored HTML canvases with live reload over SSE, and a human views
them in a browser. An agent writes (or updates) an HTML file; `scrim` makes
it reachable at a stable URL and pushes reload events when the file changes.

> **Status**: Phase 4 (polish). All CLI verbs below are implemented. A random
> capability token gates every request by default (`--no-auth` to disable),
> the daemon advertises `scrim.local` over mDNS when bound beyond loopback,
> `open` launches your default browser, and a version-mismatched daemon is
> replaced automatically the next time the CLI self-starts one — see
> [CLAUDE.md](CLAUDE.md).

## Install

```bash
go install github.com/jedwards1230/scrim@latest
```

## Usage

```bash
scrim add <id>      # Register a canvas
scrim path <id>     # Print the filesystem path for a canvas
scrim list          # List registered canvases
scrim open [<id>]   # Open a canvas (or the dashboard) in a browser
scrim rm <id>       # Remove a canvas
scrim status        # Show daemon status
scrim stop          # Stop the daemon
scrim serve         # Run the daemon in the foreground
```

The daemon self-starts on first use of any verb that needs it (`add`,
`open`, etc.) and idles down after `--idle-timeout` of inactivity.

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
`list`, `open`, and `status` print carry it as `?t=<token>`; the first
request with a valid token sets a cookie so the browser's own follow-up
requests (including live-reload's SSE connection) don't need it repeated.
Requests with neither a valid token nor a valid cookie get 401. Pass
`--no-auth` (or set `SCRIM_NO_AUTH=1`) to disable this entirely.

When `--host` binds beyond loopback, the daemon also advertises itself as
`scrim.local` over mDNS; printed URLs then show both the `scrim.local` form
and the plain `ip:port` fallback, since mDNS can be blocked on some networks.

## Browser auto-open

`scrim open [<id>]` launches the resolved URL in your platform's default
browser (`open` on macOS, `xdg-open` on Linux, `cmd /c start` on Windows).
The URL is always printed to stdout too — if auto-open isn't supported or
fails (e.g. no browser installed, headless environment), `open` prints a
one-line notice on stderr and still exits `0`.

## Version-skew restart

Every self-start check (`add`, `list`, `open`, or any other verb that talks
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
