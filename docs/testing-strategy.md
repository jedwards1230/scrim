# Testing strategy

What scrim tests today, what it should test next, and how performance
regressions get caught. This is a plan, not a description of the current state
— everything under "Proposed" is unbuilt.

## Where we are

The unit and HTTP-level coverage is already good. 22 packages have tests; the
two that carry the most behavior are also the two best covered — `internal/server`
at 76% and `internal/mcpserver` at 80% — and both are *integration*-grade
already, not unit tests wearing a costume. `internal/server` drives the real
mux through `httptest`; `internal/mcpserver` stands up a real
`server.NewHub` behind `httptest` and talks to it over the real wire
(`internal/mcpserver/backend_hub_test.go:25`). There is a working fake OIDC
provider (`internal/oidc/oidctest`) that mints real RS256 ID tokens and drives a
full auth-code login, and a working fake authorization server for OAuth
(`internal/mcpserver/oauth_test.go:33`).

So the useful question is not "add integration tests" — a lot of what other
projects would call integration tests already exist here. It is: **which seams
are still untested, and which of those need something the current harnesses
can't give.**

Three things are true and worth stating plainly before any plan:

1. ~~**`scripts/e2e.sh` runs in no workflow.**~~ **DONE.** This was the largest
   risk in the repo when this document was written: 34 scenarios and ~124
   assertions — including the entire hub token/grant/claim surface and the
   privacy log-redaction regression — gated on someone remembering to run a
   script. It now runs as its own parallel `e2e` job in `ci.yml`, present in the
   `ci` aggregate's `needs` *and* its results array, and mutation-tested to fail
   the build on a broken assertion.
2. **Three load-bearing paths sit at 0% coverage.** `server/hub.go:74 broadcast`
   (the SSE fan-out — SSE is tested for *shutdown* and for the connection *cap*,
   never for actually delivering a reload to a live client), `daemon.go:49 Ensure`
   / `:205 spawnAndWait` / `spawn_unix.go:13 detach` (the entire self-start path,
   which the PRD itself calls "the main defect surface"), and
   `mcpserver.go:804 handleShareCanvas` / `:852 handleListGrants`.
3. **There are zero benchmarks, zero fuzz targets, and no `t.Parallel()` anywhere.**

## Integration testing

### The split rule

The repo already made the right call — OIDC-dependent cases went to Go rather
than shell, because an OIDC hub fails closed at startup without a live IdP and
standing one up in bash is impractical (`scripts/e2e.sh:1297`). Generalize that
into a rule:

> **Go is the default. Shell e2e is only for what needs a real process, a real
> built binary, or real CLI ergonomics.**

That means shell keeps: daemon spawn/stale-pid/double-start, version-skew
restart, idle-timeout self-exit, SIGTERM handling, browser-launch opt-in, and
`--flag` surface checks. Everything protocol-level — auth matrices, grant
enforcement, SSE payloads, the machine API, MCP tool behavior — belongs in Go,
where it is faster, debuggable, and race-detected.

Do **not** duplicate what shell already covers well. Scenarios 22-32 (hub
tokens, actor attribution, grants, principals, claim) are thorough and correct;
the answer to them is to *run* them in CI, not to re-implement them in Go.

### The in-process trio

Several planned tests need a hub, an MCP server, and an identity provider alive
at once. Every piece already exists and is already wired up somewhere — they are
just wired up three different ways in three different files. Consolidate into one
test-only helper (`internal/testenv`), assembling:

- `oidctest.New(t)` — the fake IdP, plus `IdP.Login(t, auth, returnTo)` which
  returns a usable session cookie (`internal/oidc/oidctest/login.go:17`).
- `server.NewHub(...)` behind `httptest.NewServer` — the pattern in
  `hubgate_oidc_test.go:24`, which already builds a parallel authenticator
  sharing the hub's session secret so tests can mint valid cookies directly.
- The MCP server pointed at that hub's URL — the pattern in
  `backend_hub_test.go:25`, driven end-to-end over streamable HTTP by
  `oauth_attribution_test.go:177 runListOverHTTP`.

That is a consolidation, not a new harness. Resist building an external
orchestration layer (docker-compose, testcontainers): nothing here needs a real
network, a real database, or a real IdP, and adding one buys flakes and CI
minutes for no fidelity gain.

### What to add, in priority order

**1. SSE delivery (Go).** `broadcast` is the mechanism the entire product rests
on and it has no Go coverage. Test: connect N `httptest` SSE clients to one
canvas, write a file, assert every client receives exactly one `event: reload`
after the 200ms debounce, and that a client on a *different* canvas receives
nothing. Also assert coalescing: ten rapid writes produce one reload, not ten
(the per-client channel is `make(chan struct{}, 1)` with a non-blocking send —
that coalescing is a designed behavior and nothing asserts it).

**2. Daemon spawn (Go, process-level).** `Ensure` and `spawnAndWait` are only
exercised by shell today. A Go test that builds the binary once (`go test` can
`exec` a fixture built by `go build` in `TestMain`) and drives `Ensure`
concurrently gives a race-detected version of e2e scenario 10, which is exactly
what issue #8 needs. This is the single highest-value Go addition: it converts
a once-observed shell flake into something `-race -count=100` can characterize
in seconds.

**3. The grant enforcement matrix (Go).** The largest *stated* gap
(`scripts/e2e.sh:1297-1308`): per-grant-kind VIEW enforcement (user / group /
everyone / link), owner-only visibility, and "a second token can't see the
first's canvas". This is one table-driven test over
(grant kind × viewer identity × expected status), built on the trio above. It is
a day of work and closes the largest single hole in the security surface.

**4. MCP grant tool handlers (Go).** `handleShareCanvas` / `handleListGrants` at
0%. The backends beneath them are tested; only the tool handlers aren't. Small,
mechanical, do it while the trio is fresh.

**5. Push/swap atomicity under concurrency (Go).** `assertNoStagingLeak`
(`handlers_push_swap_test.go:66`) and the `renameStagedSwap` fault-injection seam
already exist. What's missing is the concurrent case: two simultaneous pushes to
the same id must serialize through `pushLocks` and leave exactly one coherent
canvas; two to *different* ids must not serialize. Run under `-race`.

**Not worth building:** a browser-driven test of the injected reload script, or
a mocked filesystem layer. The script is four lines and the shell suite already
asserts it is injected and that the event fires; a real browser would be the
flakiest thing in the repo.

## Performance testing

### What actually matters

scrim's workload is a handful of humans watching canvases, not a service under
load. Most of the system has no interesting performance story and should get no
tests. Five things do, ranked by actual risk:

**1. The SSE connection cap is off in the local daemon.** `maxSSEClients` and
`maxSSEClientsPerCanvas` (256 / 32) are set only in hub mode
(`internal/server/hubmode.go:113`); the local daemon leaves both at 0 =
unlimited. Broadcast holds one global mutex while iterating a canvas's clients
(`hub.go:74`). With the cap on, contention is bounded by construction. With it
off, it isn't. This is a design question a benchmark should answer: measure
`broadcast` at 10 / 100 / 1000 clients and decide whether the local daemon needs
a (generous) default cap. Most likely answer: yes, and the benchmark is how you
pick the number.

**2. Push latency includes deleting the previous canvas.** The swap is
aside-rename-delete, and `os.RemoveAll(aside)` runs inline in the request
(`handlers_push.go:175`) *inside* `pushLocks.lock(id)` — so a push pays for
deleting the old canvas, and the next push to the same id waits for it. Benchmark
push of a full-size canvas (1000 entries / 50 MiB, the configured caps) over an
existing canvas of the same size. If the delete is a meaningful share of the
total, move it to a background goroutine; that is a two-line change the benchmark
justifies.

**3. Snapshots are full recursive copies with no retention.** `snapshot.Create`
is a `WalkDir` + `io.Copy` per file (`internal/snapshot/snapshot.go:382`) — no
tar, no dedup, no hardlinking — and nothing ever prunes (issue #45). Cost is
O(canvas size) in time *and* disk, every snapshot, forever. The useful artifact
here is not a benchmark, it's a **growth assertion**: a test that takes 50
snapshots of a fixed canvas and asserts total disk stays under a bound. It fails
today; it passes once #45 lands. Write it with #45, not before.

**4. Token and principal stores are whole-file rewrites under one global mutex.**
`tokens.json` and `principals.json` marshal the entire list on every write
(`usertoken.go:253`, `principal.go:136`), each behind a single package-level
`sync.Mutex` — unlike canvas metadata, which is sharded per-canvas. Minting one
token rewrites every token. Benchmark create/revoke at n = 10 / 100 / 1000 to
find where it bends. This is the sleeper: it is fine now and quadratic later.

**5. The hub index at many canvases.** The gallery reads `meta/<id>.json` per
canvas. Benchmark the index handler at 10 / 100 / 1000 canvases.

**Explicitly not worth measuring:** end-to-end HTTP RPS (net/http is not the
bottleneck here and the number would only measure the runner), markdown
rendering, gzip, or anything in `internal/gzipx` / `internal/dircopy`.

### Benchmarks vs. load generation

Go benchmarks for all five. No load-generation tool. A `vegeta`/`k6` harness
would measure GitHub's shared runners more than it measures scrim, and nothing
in this system is expected to face concurrent load that `b.RunParallel` can't
represent. The one genuine concurrency question — many SSE clients on one canvas
— is better expressed as a benchmark of `broadcast` plus a `-race` stress test
of the watcher, because the risk there is a *correctness* failure under
contention, not a throughput ceiling.

That watcher stress test deserves its own note. `scheduleReload`
(`internal/server/watcher.go:144`) carries a 30-line comment documenting a real
prior crash (a `WaitGroup` driven negative by `Reset` on an already-firing
`AfterFunc`), and `Close` has a documented ordering requirement. That is the
most concurrency-delicate code in the repo and it deserves a dedicated
`-race` test hammering register/write/deregister/close concurrently. It is not a
performance test — it just lives next to one.

### Regression detection

A benchmark nobody reads is worthless, and a benchmark that gates CI on
wall-clock is worse — shared runners are noisy enough that it would train
everyone to re-run until green. So split the mechanism in two:

**Per-PR, blocking — assert what is deterministic.** Two kinds of assertion,
both stable on any runner:

- **Allocation counts.** `b.ReportAllocs()` plus a `testing.Benchmark(...)`
  wrapper in a normal `Test` function asserting `AllocsPerOp() < N`. Allocations
  don't vary with runner load. An accidental `[]byte` copy per SSE client, or a
  full re-marshal added to a hot path, moves this immediately.
- **Complexity ceilings.** A test asserting push of a 1000-entry canvas, or
  minting the 1000th token, completes under a *generous* bound (10× measured,
  not 1.2×). This catches an O(n) path going O(n²) — the failure mode that
  actually matters — and never fires on a slow runner.

**Nightly, advisory.** `go test -bench=. -benchmem -count=6`, results uploaded as
an artifact and `benchstat`-compared against the previous night's artifact, with
the delta table written to the job summary. Non-blocking, human-read. Do *not*
build an auto-issue-filing bot: for a repo this size the machinery would outlive
the signal.

Be honest that the nightly is the weaker half. The blocking allocation and
complexity assertions are what will actually catch a regression.

## Flake resistance

The repo has one known flake (issue #8) and it is instructive, because the bug
is most likely in the *assertion*, not the lock. Scenario 10 does
`wait_for_file daemon.json`, then a bare `sleep 0.3`, then
`pgrep -f "scrim serve --dir $DIR7" | wc -l` and requires exactly `1` — a fixed
300 ms settle window that also counts a losing racer that hasn't exited yet.
Meanwhile, ten lines away, the same script does this correctly, polling
server-observable truth with a deadline:

```sh
REG_DEADLINE=$((SECONDS + 5))
while [ $SECONDS -lt $REG_DEADLINE ]; do
  if [ "$(sse_client_count "$STATUS_URL9")" = "1" ]; then REGISTERED=1; break; fi
  sleep 0.1
done
```

Five rules, all derived from what's already in the tree:

1. **Never assert after a fixed sleep.** Poll an observable with a deadline. If
   there is nothing observable to poll, that is a gap in the product's
   introspection surface, not a reason to sleep.
2. **Assert the invariant, not a transient.** For #8, the thing that must be
   true is "exactly one daemon is *serving*" — one pid in `daemon.json`, both
   canvases reachable — not "the process table has one match right now".
3. **Every test that starts a daemon gets its own `--dir` *and* its own port.**
   ~~`scripts/e2e.sh` does not follow this.~~ **DONE.** When this was written only
   6 scenarios passed `--port` and the rest used the default 7777. Every scenario
   now allocates through `use_port`/`alloc_port` (23 call sites), and scenario 1
   asserts against the daemon's own state file that it bound the allocated port
   and not 7777 — so the property is tested, not merely intended. The rule stands
   for anything new.
4. **No second-granularity timing assertions.** `$SECONDS`-based bounds like
   `-le 4` are one scheduling hiccup from failing. Either widen the bound
   drastically or assert ordering instead of duration.
5. **No `t.Parallel()` yet.** There is none in the repo today, and adding it
   before rule 3 is universally true would manufacture exactly the class of flake
   this section is about.

One more trap worth closing: ~~running as root silently skips four
failure-injection tests~~ **DONE.** The count was wrong — it is **three** tests,
not four (`internal/snapshot/snapshot_test.go` ×2, `internal/server/handlers_push_swap_test.go`
×1); the fourth push-swap failure-injection test is uid-independent by construction.
Root bypasses the permission checks they inject, so in a container-based CI the
rollback and staging-leak paths would quietly stop being verified with no red
signal. `requireUnprivileged` now `t.Fatal`s when `CI` is set and euid is 0, and
prints to stderr otherwise. `scripts/e2e.sh` got the same treatment: it records
skips and fails when `CI` is set and anything was skipped.

## CI budget and cadence

Today's per-PR wall clock is ~3m45s, and **tests are not the critical path** —
`spec-lint` alone is 3m35s, almost all of it `go install vacuum` pulling a
toolchain. `test` is 59-71s. So there is roughly 2.5 minutes of headroom in
parallel with `spec-lint` before anything gets slower.

That headroom makes the most important item on this list free:

| Cadence | What | Cost |
|---|---|---|
| **Per-PR** | Existing `go test -race` + the new Go integration tests | ~90s (grows to ~2m) |
| **Per-PR** | `scripts/e2e.sh` as a parallel job | 60-120s, hidden under `spec-lint` |
| **Per-PR** | Allocation + complexity assertions (ordinary `Test` funcs) | negligible |
| **Nightly** | `-bench=. -benchmem -count=6` + `benchstat` vs. previous | ~5m, advisory |
| **Nightly** | `scripts/e2e.sh` ×20, reporting a flake rate | ~30m, characterizes #8 |
| **Nightly** | `macos-latest` + `windows-latest` test matrix | ~3m |
| **Manual** | Nothing. If a case is worth writing, it is worth scheduling. |  |

Two adjacent notes. First, caching the `vacuum` binary (or pinning
`GOTOOLCHAIN`) would cut ~3 minutes off every PR — the single largest CI win
available, and it costs one workflow edit. Second, **CI is Linux-only** despite
`pid_windows.go`, `spawn_windows.go`, and a `//go:build windows` test file
existing. The nightly matrix is how that gets honest without taxing every PR.

## Phasing

**Phase 0 — wire up what exists. MOSTLY DONE.** The e2e job is in `ci.yml` and
every scenario is port-isolated; that took ~124 existing assertions from
unenforced to enforced, which was the best value-per-hour in this document.

**Still outstanding: scenario 10's assertion.** It remains a fixed `sleep 0.3`
followed by `pgrep … | wc -l` requiring exactly 1. Diagnostics were added around
it, but the assertion itself was not converted to deadline polling per rule 2.

Note the original rationale for this item no longer holds: it was expected to
resolve #8 without touching `internal/daemon/lock.go`. It will not. The recovered
failure output shows this assertion **passed** (`found 1`) — the lock worked, and
#8 was never the double-start race its title claimed. The real bug is a negative
timeout margin (worst-case spawn-lock hold ~15.4s against a 15s
`spawnLockTimeout`, with `cmdAdd` calling `daemon.Ensure` before creating the
canvas). Converting the assertion is still correct hygiene — a fixed sleep plus a
process count is the wrong shape regardless — but it is not a fix for #8.

**Phase 1 — close the 0% seams (1-2 days).** Build `internal/testenv` by
consolidating the three existing wire-ups, then use it for SSE delivery, the
daemon spawn path, and the MCP grant tool handlers. The `-race -count=100`
daemon test is the durable answer to #8 that Phase 0 only papers over.

**Phase 2 — the grant enforcement matrix (1-2 days).** The largest stated gap and
the one with security consequences. Table-driven, on top of Phase 1's helper.

**Phase 3 — performance (1 day).** The five benchmarks, the allocation and
complexity assertions, the nightly workflow, and the watcher `-race` stress test.
Deliberately last: it is the least valuable per hour, and doing it before Phase 1
means benchmarking paths that aren't yet correctness-tested.

**Deferred.** The macOS/Windows nightly matrix (do it when a platform bug is
actually reported, or when #19 lands). The snapshot growth assertion (write it
with #45, not before). Load generation (probably never).

The honest summary: Phase 0 is hours and removes the largest risk in the repo.
Phases 1-2 are the real work. Phase 3 is worth doing but is not urgent, and
saying so is more useful than pretending otherwise.
