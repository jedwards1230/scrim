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
| `config` | Resolves --dir/--host/--port/--idle-timeout/--no-auth from flags/env/defaults; derives on-disk paths |
| `state` | Daemon state file (`daemon.json`): atomic read/write, corruption handling |
| `canvas` | Canvas directory CRUD, ID validation, per-canvas metadata (title) |
| `apiclient` | Thin HTTP client for the daemon's `/api/*` control surface |
| `daemon` | CLI-side lifecycle: health-check, self-start (with a spawn lock), stop |
| `server` | The daemon itself: HTTP server, static canvas serving + SSE injection, per-canvas SSE, index page, `/api/*`, idle reaper |
| `cli` | Verb parsing/dispatch for `add`, `path`, `list`, `open`, `rm`, `status`, `stop`, `serve` |

Phase 3 (auth via the state file's `token` field, mDNS advertisement) and
Phase 4 (`open` actually launching a browser, version-skew restart) are not
built yet.

Planned data flow: `main.go` dispatches a verb → `cli` either talks to a
running daemon over its local HTTP API or starts one (`daemon`) → the daemon
serves canvases and pushes SSE reloads on file changes (`server`, via
`fsnotify`) → a browser (human) or an agent (`add`/`path`) is the other end.

### Architecture decisions

- **No CGO**: the binary must be cross-compilable without a C toolchain.
- **Dependencies stay minimal**: Go stdlib + `fsnotify` + one mDNS library
  only. Don't add a dependency without a real need.
- **Single binary, self-starting daemon**: no separate install/systemd step —
  the first verb that needs the daemon starts it if it isn't running.

## Conventions

### Package organization

All business logic lives under `internal/`. `main.go` stays thin — it only
dispatches to `internal/cli` (once that package exists).

### Adding a new internal package

1. Create `internal/<name>/<name>.go`.
2. Export only the types and functions used by other packages.
3. Write a `<name>_test.go` alongside — table-driven tests preferred.

## Build Variables

Version info (`Version`, `Commit`, `Date`) is injected into `internal/version`
via `-ldflags` at build time. `make build` handles this automatically. Use
`internal/version.Short()` / `version.Info()` — never hardcode a version
string.
