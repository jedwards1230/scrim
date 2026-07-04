# scrim

A projection surface for coding agents: write plain HTML/CSS/JS files into a
canvas directory and [`scrim`](https://github.com/jedwards1230/scrim) serves
them at a local URL with instant live-reload, so a human can watch from any
browser on the machine (or LAN, with `--host`).

This plugin is a thin skill wrapper around the standalone `scrim` binary — it
doesn't bundle the tool itself.

## Install

```
/plugin marketplace add jedwards1230/scrim
/plugin install scrim@jedwards1230-scrim
```

Then install the `scrim` CLI separately:

```bash
go install github.com/jedwards1230/scrim@latest
```

or grab a release binary from
[github.com/jedwards1230/scrim/releases](https://github.com/jedwards1230/scrim/releases).

## What it does

A `scrim` skill teaches Claude the current CLI surface (`add`, `path`, `list`,
`link`, `open`, `rm`, `snap`, `snaps`, `revert`, `status`, `stop`, `serve`) and
the core loop: `scrim add <id>` → Write/Edit files in the printed directory →
the browser reloads itself → always surface the canvas URL back to the user
via `link` (never `open`, which is reserved for the human's own explicit
browser-launch opt-in).

The `hub` and `push` verbs (central-store server + canvas upload) also exist
but are out of scope for agent use — hub operation is a human/CI concern.

It also covers:

- The card-gallery dashboard and per-canvas metadata (`--title`/`--desc`/`--icon`).
- Snapshots (`snap`/`snaps`/`revert`) as pure filesystem operations.
- The capability-token auth model and the token-stripping redirect.
- `--no-mdns` / LAN-binding behavior.
- How scrim wraps bare HTML fragments and markdown in its own skeleton, while
  a complete HTML document passes through unwrapped.
- How to verify a canvas actually rendered, using the agent's own headless
  tooling (Playwright MCP preferred, `curl` as a fallback) rather than ever
  touching the user's own browser.

**Dependencies:** the `scrim` binary (not bundled — see Install above).
