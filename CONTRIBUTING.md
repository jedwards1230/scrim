# Contributing to scrim

scrim is a self-starting daemon that serves agent-authored HTML canvases with
live reload, viewed by a human in a browser.

## Prerequisites

- [Go](https://go.dev/) (version from `go.mod`)
- [golangci-lint](https://golangci-lint.run/)
- [pre-commit](https://pre-commit.com/) (`pip install pre-commit` or `brew install pre-commit`)

## Build, test & lint

```bash
# Build
make build

# Run
make run

# Install globally
make install

# Test
go test ./... -count=1

# End-to-end suite (builds the real binary and drives it as a subprocess).
# Runs as its own CI job; safe to run while your own daemon is up, since every
# scenario allocates its own port.
./scripts/e2e.sh

# Vet
go vet ./...

# Lint
golangci-lint run ./...

# Format
gofmt -l -w .
```

## Documentation

Keep documentation current as part of the change, not as a follow-up — update
the README and any affected docs in the same PR.

## Before you open a PR

- Make sure all CI checks pass locally first — run the formatter, vet, linter,
  and tests.
- Run `pre-commit run --all-files` (this repo uses pre-commit hooks).

## Branching & commits

- Branch off `main`; never commit directly to `main`.
- Use [Conventional Commits](https://www.conventionalcommits.org/) prefixes (`feat:`, `fix:`, `docs:`, `chore:`, `refactor:`, `test:`, …).
- Sign your commits where possible (`git commit -S`).
- Keep each PR focused; delete dead code rather than commenting it out.

## Pull requests

- Open the PR against `main`.
- Every PR runs CI. Resolve **all** review threads before the PR is merged.
- **Every PR needs exactly one `semver:*` label** — see [Releases](#releases). The
  `Semver label` check enforces it and re-runs when you add the label, so there's
  no need to push another commit.
- A PR can be merged once CI is green and all review threads are resolved.

## Releases

Every PR must declare what it does to the version, using exactly one label:

| Label | Effect on merge |
|---|---|
| `semver:major` | Cut `vX.0.0` |
| `semver:minor` | Cut `v0.Y.0` |
| `semver:patch` | Cut `v0.0.Z` |
| `semver:none` | **No release** — docs, chore, CI-only |

A release publishes a single immutable `vX.Y.Z` tag, cross-built binaries, a
multi-arch image, and the Helm chart at
`oci://ghcr.io/jedwards1230/charts/scrim`.

`semver:none` is a real answer, not a way to opt out of the question. It exists
because the alternative — inferring "no release" from a *missing* label — is
indistinguishable from forgetting, and `release.yml` skips every job while still
reporting **success**. A green run that published nothing looks exactly like one
that published everything; that failure mode silently swallowed three releases
before the check existed.

If a release is missed anyway, recover it with a manual dispatch rather than a
second merge:

```bash
gh workflow run release.yml -f bump_type=minor
```

## Plugin version convention

This repo hosts its own Claude Code plugin marketplace (`.claude-plugin/marketplace.json`,
`plugins/scrim/`) so `scrim` can be installed via
`/plugin marketplace add jedwards1230/scrim`. `plugins/scrim`'s
`.claude-plugin/plugin.json` version is the **plugin's own** semver and
deliberately does not track the scrim tool's release version — the two drift
apart on purpose (the plugin is at `0.4.1` while the tool is at `v0.9.0`). It
doesn't bump on every commit, only when the tool's functionality changes in a
way that changes the plugin-relevant surface (new verbs, changed behavior the
skill documents).

When that happens:

1. Bump `plugins/scrim/.claude-plugin/plugin.json`'s `version`.
2. Bump the matching `scrim` entry's `version` in `.claude-plugin/marketplace.json`
   to the same value.
3. Bump `.claude-plugin/marketplace.json`'s own `metadata.version`, sized by
   what changed: major for a plugin added/removed, minor for core marketplace
   metadata changes, patch for a plugin version change.

`.github/workflows/plugin-version-check.yml` (via `scripts/check-plugin-versions.sh`)
enforces steps 1 and 2 in CI on any PR touching `plugins/**` or
`.claude-plugin/marketplace.json`, and enforces that step 3's `metadata.version`
was bumped *at all* — an unchanged one fails the job. What it does **not**
enforce is step 3's major/minor/patch *sizing*: that convention is printed as
advisory prose in the job summary and is never checked.
