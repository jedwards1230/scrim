---
name: scrim
description: Use when you need to show the user something visual — a live HTML/CSS/JS preview, a canvas, a diagram, a mockup, or any rendered page — and there's no existing app to preview it in. scrim gives you a projection surface at a local URL that live-reloads as you write files, so the user can watch it update in their browser. Before authoring canvas content, check for and load the artifact-design skill (and dataviz for charts/graphs) if available in the session. Triggers on "show me a preview", "render this as HTML", "live preview", "canvas", "projection surface", "scrim".
---

# scrim

`scrim` is a small Go CLI + self-starting daemon that serves a directory of
plain HTML/CSS/JS (or Markdown) at a local URL and live-reloads the browser
tab whenever you write to it. Use it when you want the user to *watch
something render* — a diagram, a mockup, a data view, a game-of-life toy —
and no other app already provides that preview surface (don't reach for it to
serve a real project's dev server; use that project's own tooling for that).

## Availability

Check before use: `command -v scrim`. If missing, point the user at
installation — a release binary from
`github.com/jedwards1230/scrim/releases`, or `go install
github.com/jedwards1230/scrim@latest` if they have Go tooling.
Don't try to install it yourself; surface the command and let the user decide.

## The critical rule: `link`, never `open` — and always tell the user

**Always use `scrim link <id>` to get a canvas's URL. Never use `scrim
open`.** `link` only ever prints the URL — no flag or environment variable
can make it launch a browser, which is exactly why it's the safe, correct
choice for an agent. `open` prints the same URL but can *also* launch the
user's actual browser (opt-in only, via `--browser` or
`SCRIM_OPEN_BROWSER=1`) — that opt-in exists for a human at their own
keyboard to ask for a one-off launch, or to set as their own default. There is
no legitimate reason for an agent to ever pass `--browser` or set
`SCRIM_OPEN_BROWSER=1`; doing so would pop a browser tab open on the user's
machine unprompted, which is a surprise, not a convenience.

After every `scrim add` (and again after any major update to a canvas),
**surface the printed URL back to the user in your own reply** — don't assume
they remember it from a previous turn, and don't let "I already have it
open in a headless browser" substitute for telling them. Verifying a canvas
with your own tooling (see "Closing the loop" below) is a completely separate
step from surfacing the URL — you're never "looking through the user's
browser" on their behalf, you're independently confirming the canvas works
with your own tools, and the user still needs the URL to look for themselves.

## Workflow

1. **Before authoring content**: check for and load the `artifact-design`
   skill if it's available in the current session (and additionally the
   `dataviz` skill if the canvas involves a chart/graph/data visualization).
   See `references/canvas-design.md` in this skill for a compact checklist.
2. `scrim add <id> [--title T] [--desc D] [--icon I]` — starts the daemon if
   it isn't already running, creates (or reuses) a canvas, and prints the
   canvas directory plus its URL. Pick a distinct, purposeful canvas ID per
   task/session rather than reusing one generic ID across unrelated work —
   reusing an ID overwrites/confuses that canvas's gallery entry and
   snapshot history. **Keep a canvas's `--icon` stable** across later
   `add`/re-registration calls for the same ID — the gallery card and the
   generated favicon key off it, so changing it on every update looks like
   URL/identity churn to a human glancing at the dashboard.
3. Write/Edit plain `.html`/`.css`/`.js` files (or `index.md`) directly into
   that printed directory (`index.html` or `index.md` is the entry point). No
   build step, no framework — the daemon serves the files and injects a
   small live-reload script. See "Fragments, markdown, and full documents"
   below for what needs a wrapper and what doesn't.
4. Every save triggers a full-page reload in any open browser tab via SSE —
   you don't re-run anything to see the next version.
5. `scrim link <id>` to get the URL, then **surface it to the user**. Verify
   with your own headless tooling if you want to confirm it renders (see
   "Closing the loop").

## All verbs

```
scrim add <id> [--title T] [--desc D] [--icon I]
                        Register a canvas (self-starts the daemon)
scrim path <id>         Print the filesystem path for a canvas
scrim list              List registered canvases + URLs + daemon status
scrim link [<id>]       Print a canvas's (or the dashboard's) URL. Always
                        print-only — never launches a browser. This is the
                        verb an agent should always use.
scrim open [<id>] [--browser]
                        Print a canvas's (or the dashboard's) URL. Default:
                        prints only, plus a stderr hint. Pass --browser or
                        set SCRIM_OPEN_BROWSER=1 to also launch it — a
                        human's explicit opt-in only. An agent should never
                        pass --browser or set this env var.
scrim rm <id>           Remove a canvas
scrim snap <id> [--label L]
                        Snapshot a canvas's current contents (does not
                        self-start the daemon)
scrim snaps <id>        List a canvas's snapshots (does not self-start)
scrim revert <id> [<snap>]
                        Restore a canvas from a snapshot, latest by default
                        (does not self-start; takes its own "prerevert"
                        safety snapshot first, so a revert is itself undoable)
scrim status            Show daemon status (does not self-start)
scrim stop              Stop the daemon (canvas files persist on disk)
scrim serve             Run the daemon in the foreground (containers/systemd;
                        not for normal use)
```

Every verb that needs the daemon running (`add`, `list`, `link`, `open`,
etc.) self-starts it as needed and idles it down after `--idle-timeout` of
inactivity. `status` and `stop` never self-start it — they only report on or
act against an already-running daemon, printing "no daemon running" instead
if there isn't one. `snap`/`snaps`/`revert` (like `path`/`rm`'s fallback
path) are pure filesystem operations and never self-start the daemon either.

## Fragments, markdown, and full documents

A canvas's `index.html` can be **either**:

- A complete HTML document (`<!doctype html>` + `<head>` + `<body>`) — served
  byte-for-byte, unwrapped (modulo the injected live-reload script), **or**
- A bare HTML fragment with no document wrapper at all — scrim auto-wraps it
  in its own embedded skeleton (CSS reset, `prefers-color-scheme` light/dark
  theming, viewport meta) before serving.

An `index.md` is rendered to HTML via `goldmark` at serve time and gets the
same auto-wrap treatment; raw HTML embedded in it passes through
unsanitized, the same trust model as a `.html` canvas.

This means simple content doesn't need any boilerplate — write a fragment or
a markdown file and scrim supplies the shell. **This is the opposite of the
claude.ai Artifact tool**, whose skeleton-wrap only ever wraps (an Artifact
must always be a complete document): scrim wraps only when you *don't*
provide your own document shell, and gets out of the way completely when you
do.

## Gallery & canvas metadata

The dashboard at `/` is a card gallery: each canvas gets an icon (from
`--icon`, or a deterministic default derived from its ID), an accent color
(derived from the ID), its title/description, last-modified time, and a live
viewer count. A canvas's served pages also get a matching favicon generated
from that icon, unless the canvas ships its own `favicon.ico`. Use
`--title`/`--desc`/`--icon` on `add` to make a canvas identifiable at a
glance in the gallery — and keep the icon stable across updates (see
Workflow step 2 above).

## Snapshots

`scrim snap <id> [--label L]` copies a canvas's current contents into a
timestamped snapshot. `scrim snaps <id>` lists them, newest first. `scrim
revert <id> [<snapshot>]` replaces the canvas's current contents with a
snapshot's entirely (not merged), defaulting to the latest snapshot when none
is named. All three are pure filesystem operations — they never self-start
the daemon — and `revert` takes its own `prerevert` safety snapshot of
whatever was there first, so a revert is itself undoable via another revert.

## Auth & token model

By default every printed/opened URL carries a capability token (`?t=...`).
The first request with a valid token sets a cookie, then the daemon redirects
to the same URL with the token stripped — so the token never lingers in the
browser's URL bar, history, or a copied/shared link. Subsequent requests
(including the live-reload SSE connection) authenticate via that cookie
instead. Requests with neither a valid token nor a valid cookie get 401. Pass
`--no-auth` (or set `SCRIM_NO_AUTH=1`) to disable this entirely — only suggest
that on a trusted network, and say so explicitly.

The daemon binds to `127.0.0.1` by default — only reachable from the local
machine. `--host` opts into binding beyond loopback for LAN viewing (e.g. a
second device); when it does, the daemon also advertises itself as
`scrim.local` over mDNS unless `--no-mdns` (or `SCRIM_NO_MDNS=1`) is set.
There's no cross-network relay — Tailscale or similar handles that if the
user wants to view from off-network.

## Closing the loop

Don't just write files and declare done — verify what actually rendered,
using **your own tooling**, never the user's browser:

- **Preferred**: use Playwright MCP (if available) to navigate to the canvas
  URL (from `scrim link`) and take a screenshot. This catches rendering/JS
  errors a file read can't. This is independent verification with your own
  headless tools — it is never a substitute for surfacing the URL to the
  user, since they still need it to look for themselves.
- **Fallback** (no browser tooling available): `curl` the canvas URL to
  sanity check the markup served, but note to the user that you haven't
  visually confirmed it.

If the screenshot shows something broken, fix the files and reload the
screenshot rather than asking the user to check for you.
