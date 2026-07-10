# Stability & versioning (pre-1.0)

scrim is pre-1.0. Releases are opt-in and each publishes a single immutable
`vX.Y.Z` tag (see [CONTRIBUTING.md](../CONTRIBUTING.md#releases)). Until 1.0,
treat the surface as still-settling: verbs, flags, and on-disk layout can change
between minor versions. This page says what "breaking" means and how upgrades
are handled.

## What "breaking" covers

Two pieces of persistent state matter across an upgrade:

- **Local on-disk state** (`~/.scrim`): canvas directories, per-canvas metadata
  (`meta/<id>.json`), snapshots (`versions/<id>/...`), and the daemon state file
  (`daemon.json`). Canvas *content* is just files you authored — always yours,
  never rewritten by an upgrade. The metadata/snapshot/state **layout** is what a
  release may change.
- **Hub `/data` state**: the same canvas + metadata + snapshot layout, plus
  ownership/grant records, served from the hub's own directory.

A change to either layout is the kind of "breaking" this policy is about — not a
change to a flag name (annoying, but re-runnable).

## Metadata migrations happen on startup

Layout changes are handled by forward migrations that run when the daemon or hub
starts, so an upgrade is a restart, not a manual data step. For example, a hub
stamps `owner: admin` on any canvas whose metadata predates ownership on every
startup (see [identity.md](identity.md#ownership-sharing--tokens)), and the v0.1
sidecar-metadata pattern was migrated out to external `meta/<id>.json` files.
Migrations are additive and idempotent — a canvas's authored content is never
touched, so a migration can't lose your work.

Because migrations run **forward** on startup, downgrading to an older binary
after a newer one has migrated the on-disk state is not supported — the older
binary may not understand the newer layout. Snapshot a hub's `/data` before a
major upgrade if you need a rollback path.

## Version-skew auto-restart is a stability feature

The self-starting daemon guards against a stale process serving under a
just-upgraded CLI. Every self-start check compares the CLI binary's own version
against the version a running daemon reports on `/api/status`; a mismatch is
treated the same as a stale/dead daemon — the old one is stopped gracefully and
a fresh one is started transparently. Canvases are untouched (they live on disk,
independent of the daemon process).

This is deliberate: after `go install`-ing a new scrim, the very next verb
transparently retires the old daemon, so you never silently keep talking to a
process built from last week's code. The check is skipped for an unversioned
"dev" build (no `-ldflags` version and no VCS revision), so a `go run`/`go test`
build doesn't restart a real daemon on every invocation.
