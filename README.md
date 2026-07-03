# scrim

A projection surface for coding agents.

`scrim` is a single Go binary: a shared, self-starting daemon serves
agent-authored HTML canvases with live reload over SSE, and a human views
them in a browser. An agent writes (or updates) an HTML file; `scrim` makes
it reachable at a stable URL and pushes reload events when the file changes.

> **Status**: Phase 3 (auth + mDNS). All CLI verbs below are implemented.
> A random capability token gates every request by default (`--no-auth` to
> disable), and the daemon advertises `scrim.local` over mDNS when bound
> beyond loopback. Browser auto-open / version-skew restart (Phase 4) are
> not yet built — see [CLAUDE.md](CLAUDE.md).

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
| `--idle-timeout` | `SCRIM_IDLE_TIMEOUT` | `30m`       | Idle time before the daemon exits   |
| `--no-auth`      | `SCRIM_NO_AUTH`      | `false`     | Disable local auth token            |
| `--dir`          | `SCRIM_DIR`          | `~/.scrim`  | Directory for canvases + state      |

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

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

MIT
