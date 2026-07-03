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
| `server` | The daemon itself: HTTP server, static canvas serving + SSE injection, per-canvas SSE, the card-gallery index page, `/api/*`, per-canvas favicon (agent-authored or generated from the canvas's icon), idle reaper, capability-token auth middleware (redirects a valid query token to a token-stripped URL), mDNS advertisement (opt-out via --no-mdns). Serve-time only (files on disk are never modified): an `index.md` directory-index is rendered via `goldmark`, and a bare HTML fragment (no `<!doctype`/`<html>`) is wrapped -- both in an embedded skeleton (`assets/skeleton.html`: CSS reset, `prefers-color-scheme` theming, viewport meta) before reload-script injection. A complete HTML document passes through unwrapped. |
| `snapshot` | Canvas versioning: copy a canvas directory's current contents into a timestamped snapshot, list them, and revert a canvas back to one -- a pure filesystem operation against `config.Config.VersionsDir()`, independent of the daemon |
| `mdns` | Loopback-vs-LAN bind detection, and starting/stopping the `scrim.local` mDNS advertisement (`github.com/hashicorp/mdns`) |
| `logging` | Sole sanctioned logging surface for `server`/`daemon`: category+error only (no request paths/canvas IDs/tokens ever logged), wraps `http.Server.ErrorLog` |
| `openurl` | Cross-platform "launch the default browser" (`open`/`xdg-open`/`rundll32 url.dll,FileProtocolHandler`) |
| `cli` | Verb parsing/dispatch for `add`, `path`, `list`, `open`, `rm`, `snap`, `snaps`, `revert`, `status`, `stop`, `serve`; prints `?t=<token>`-qualified URLs (and, when mDNS is active, both the `scrim.local` and plain `ip:port` forms) |

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

Planned data flow: `main.go` dispatches a verb → `cli` either talks to a
running daemon over its local HTTP API or starts one (`daemon`) → the daemon
serves canvases and pushes SSE reloads on file changes (`server`, via
`fsnotify`) → a browser (human) or an agent (`add`/`path`) is the other end.

### Architecture decisions

- **No CGO**: the binary must be cross-compilable without a C toolchain.
- **Dependencies stay minimal**: Go stdlib + `fsnotify` + one mDNS library +
  `goldmark` (serve-time markdown rendering, see below) only. Don't add a
  dependency without a real need.
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
