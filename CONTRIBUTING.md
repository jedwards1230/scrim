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
- A PR can be merged once CI is green and all review threads are resolved.

## Releases

Releases are opt-in. Before merging, add one of `semver:patch`, `semver:minor`,
or `semver:major` to the PR to cut a release on merge; with no label, merging
does not release. A release publishes a single immutable `vX.Y.Z` tag.
