#!/usr/bin/env bash
# End-to-end test of the scrim core engine: builds the real binary and
# drives it as a subprocess, asserting real observable behavior rather than
# just "it ran". Each scenario uses its own --dir so repeated runs never
# collide with a stale state file from a previous run.
#
# Auth is on by default (Phase 3), so every scenario that curls the daemon
# directly must either present the token scrim itself printed (the URLs
# `add`/`list`/`open` print already carry "?t=<token>") or run against a
# --no-auth daemon.
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$REPO_ROOT/.e2e-scrim"
WORKDIR="$(mktemp -d)"

PASS=0
FAIL=0
FAILED_SCENARIOS=()

log() { printf '\n=== %s ===\n' "$1"; }

ok() {
  PASS=$((PASS + 1))
  printf '  [PASS] %s\n' "$1"
}

bad() {
  FAIL=$((FAIL + 1))
  FAILED_SCENARIOS+=("$1")
  printf '  [FAIL] %s\n' "$1"
}

cleanup() {
  # Best-effort: stop any daemons left running by a failed scenario.
  for d in "$WORKDIR"/*/; do
    [ -f "${d}daemon.json" ] || continue
    pid=$(grep -o '"pid": *[0-9]*' "${d}daemon.json" 2>/dev/null | grep -o '[0-9]*' || true)
    if [ -n "${pid:-}" ] && kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null || true
    fi
  done
  rm -rf "$WORKDIR"
  rm -f "$BIN"
}
trap cleanup EXIT

log "Building scrim"
if ! (cd "$REPO_ROOT" && go build -o "$BIN" .); then
  echo "build failed" >&2
  exit 1
fi
ok "build"

wait_for_file() {
  local path="$1" timeout="$2" waited=0
  while [ ! -f "$path" ]; do
    sleep 0.2
    waited=$((waited + 1))
    if [ "$waited" -gt "$((timeout * 5))" ]; then
      return 1
    fi
  done
  return 0
}

wait_for_file_gone() {
  local path="$1" timeout="$2" waited=0
  while [ -f "$path" ]; do
    sleep 0.2
    waited=$((waited + 1))
    if [ "$waited" -gt "$((timeout * 5))" ]; then
      return 1
    fi
  done
  return 0
}

pid_of_state() {
  grep -o '"pid": *[0-9]*' "$1" 2>/dev/null | grep -o '[0-9]*'
}

# strip_query removes a "?..." suffix from a URL, if present.
strip_query() {
  echo "${1%%\?*}"
}

# with_events_path rewrites a canvas URL (which may carry a "?t=..." query)
# into its SSE events URL, preserving the query string rather than naively
# appending "/__events" after it.
with_events_path() {
  local url="$1" base query
  base="$(strip_query "$url")"
  case "$url" in
    *'?'*) query="${url#*\?}" ;;
    *) query="" ;;
  esac
  base="${base%/}/__events"
  if [ -n "$query" ]; then
    echo "${base}?${query}"
  else
    echo "$base"
  fi
}

# --- Scenario 1 + 2: self-start on `add`, HTML served with injected script ---
log "Scenario 1+2: add self-starts daemon; canvas HTML is served with injected SSE script"
DIR1="$WORKDIR/s1"
OUT=$("$BIN" add e2e-test --title "E2E" --dir "$DIR1" --idle-timeout 5m 2>&1)
CANVAS_DIR=$(echo "$OUT" | sed -n '1p')
CANVAS_URL=$(echo "$OUT" | sed -n '2p')

if [ -f "$DIR1/daemon.json" ]; then
  ok "state file created after add"
else
  bad "state file created after add"
fi

PID1=$(pid_of_state "$DIR1/daemon.json")
if [ -n "${PID1:-}" ] && kill -0 "$PID1" 2>/dev/null; then
  ok "daemon pid ($PID1) is alive"
else
  bad "daemon pid is alive"
fi

echo '<html><body><h1>Hello E2E</h1></body></html>' >"$CANVAS_DIR/index.html"

# CANVAS_URL already carries "?t=<token>" (auth is on by default), so this
# is also the first half of the "valid token succeeds" auth assertion.
BODY=$(curl -fsS "$CANVAS_URL" || true)
if echo "$BODY" | grep -q "Hello E2E"; then
  ok "served HTML contains original content"
else
  bad "served HTML contains original content"
fi
if echo "$BODY" | grep -q "__events" && echo "$BODY" | grep -q "<script>"; then
  ok "served HTML contains injected SSE <script>"
else
  bad "served HTML contains injected SSE <script>"
fi

# --- Scenario 3: SSE fires on file change ---
log "Scenario 3: touching a canvas file delivers an SSE reload event"
EVENTS_URL="$(with_events_path "$CANVAS_URL")"
SSE_OUT="$WORKDIR/sse-out.txt"
: >"$SSE_OUT"
curl -fsS -N --max-time 6 "$EVENTS_URL" >"$SSE_OUT" 2>/dev/null &
CURL_PID=$!
sleep 0.5 # let the SSE connection register before we touch the file
echo '<html><body><h1>Updated</h1></body></html>' >"$CANVAS_DIR/index.html"

SSE_DEADLINE=$((SECONDS + 6))
GOT_RELOAD=0
while [ $SECONDS -lt $SSE_DEADLINE ]; do
  if grep -q "event: reload" "$SSE_OUT" 2>/dev/null; then
    GOT_RELOAD=1
    break
  fi
  sleep 0.2
done
kill "$CURL_PID" 2>/dev/null || true
wait "$CURL_PID" 2>/dev/null || true

if [ "$GOT_RELOAD" -eq 1 ]; then
  ok "SSE reload event received within 6s of file change"
else
  bad "SSE reload event received within 6s of file change"
fi

# --- Scenario 4: auth rejects a request with no token ---
log "Scenario 4: a request without a token is rejected with 401"
NO_TOKEN_URL="$(strip_query "$CANVAS_URL")"
STATUS=$(curl -s -o /dev/null -w '%{http_code}' "$NO_TOKEN_URL")
if [ "$STATUS" = "401" ]; then
  ok "request with no token gets 401"
else
  bad "request with no token gets 401 (got $STATUS)"
fi
# Same assertion against the SSE endpoint specifically -- it's gated too,
# not just static/index routes.
NO_TOKEN_EVENTS_URL="$(strip_query "$EVENTS_URL")"
SSE_STATUS=$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "$NO_TOKEN_EVENTS_URL")
if [ "$SSE_STATUS" = "401" ]; then
  ok "SSE endpoint with no token gets 401"
else
  bad "SSE endpoint with no token gets 401 (got $SSE_STATUS)"
fi

# --- Scenario 5: auth accepts a request with a valid token ---
log "Scenario 5: a request with a valid token is accepted with 200"
STATUS=$(curl -s -o /dev/null -w '%{http_code}' "$CANVAS_URL")
if [ "$STATUS" = "200" ]; then
  ok "request with a valid token gets 200"
else
  bad "request with a valid token gets 200 (got $STATUS)"
fi

# --- Scenario 6: --no-auth bypasses gating entirely ---
log "Scenario 6: --no-auth disables gating entirely"
# DIR1's daemon (default port 7777) is still running at this point (it's
# stopped in Scenario 7, below) -- use a distinct port so this daemon can
# actually bind.
DIR_NOAUTH="$WORKDIR/s-noauth"
OUT_NOAUTH=$("$BIN" add noauth-test --dir "$DIR_NOAUTH" --port 7778 --no-auth --idle-timeout 5m 2>&1)
NOAUTH_CANVAS_DIR=$(echo "$OUT_NOAUTH" | sed -n '1p')
NOAUTH_CANVAS_URL=$(echo "$OUT_NOAUTH" | sed -n '2p')
if [ -f "$DIR_NOAUTH/daemon.json" ]; then
  ok "--no-auth: daemon started"
else
  bad "--no-auth: daemon started"
fi
if echo "$NOAUTH_CANVAS_URL" | grep -q '?t='; then
  bad "--no-auth: printed canvas URL should carry no token query param (got $NOAUTH_CANVAS_URL)"
else
  ok "--no-auth: printed canvas URL carries no token query param"
fi
# The canvas has no index.html yet -- write one so the request below 404s
# only if auth actually blocked it, not because there's nothing to serve.
echo '<html><body>no-auth e2e</body></html>' >"$NOAUTH_CANVAS_DIR/index.html"
STATUS=$(curl -s -o /dev/null -w '%{http_code}' "$NOAUTH_CANVAS_URL")
if [ "$STATUS" = "200" ]; then
  ok "--no-auth: request with no token at all gets 200"
else
  bad "--no-auth: request with no token at all gets 200 (got $STATUS)"
fi
"$BIN" stop --dir "$DIR_NOAUTH" >/dev/null 2>&1 || true

# --- Scenario 7: stop cleanly stops the daemon, canvas files survive ---
log "Scenario 7: stop cleanly stops the daemon; canvas files survive"
STOP_OUT=$("$BIN" stop --dir "$DIR1" 2>&1)
if wait_for_file_gone "$DIR1/daemon.json" 5; then
  ok "state file removed after stop"
else
  bad "state file removed after stop"
fi
if [ -n "${PID1:-}" ] && ! kill -0 "$PID1" 2>/dev/null; then
  ok "daemon process exited after stop"
else
  bad "daemon process exited after stop"
fi
if [ -f "$CANVAS_DIR/index.html" ]; then
  ok "canvas files still exist on disk after stop"
else
  bad "canvas files still exist on disk after stop"
fi
echo "(stop said: $STOP_OUT)"

# --- Scenario 8: idle-exit ---
log "Scenario 8: daemon exits on its own after --idle-timeout with no SSE clients"
DIR5="$WORKDIR/s5"
"$BIN" add idle-test --dir "$DIR5" --idle-timeout 3s >/dev/null
if [ -f "$DIR5/daemon.json" ]; then
  ok "idle-exit scenario: daemon started"
else
  bad "idle-exit scenario: daemon started"
fi
if wait_for_file_gone "$DIR5/daemon.json" 12; then
  ok "daemon exited on its own within bounded wait after idle timeout"
else
  bad "daemon exited on its own within bounded wait after idle timeout"
fi

# --- Scenario 9: stale-pid recovery ---
log "Scenario 9: stale-pid recovery after a simulated crash"
DIR6="$WORKDIR/s6"
"$BIN" add stale-test --dir "$DIR6" >/dev/null
PID6=$(pid_of_state "$DIR6/daemon.json")
if [ -z "${PID6:-}" ]; then
  bad "stale-pid scenario: got a pid to kill"
else
  kill -9 "$PID6" 2>/dev/null || true
  # Give the OS a moment to reap the process.
  DEADLINE=$((SECONDS + 5))
  while kill -0 "$PID6" 2>/dev/null && [ $SECONDS -lt $DEADLINE ]; do sleep 0.2; done

  RECOVER_OUT=$("$BIN" add stale-test-2 --dir "$DIR6" 2>&1)
  if [ $? -eq 0 ] || echo "$RECOVER_OUT" | grep -q "canvases"; then
    :
  fi
  if [ -f "$DIR6/daemon.json" ]; then
    NEWPID=$(pid_of_state "$DIR6/daemon.json")
    if [ -n "${NEWPID:-}" ] && [ "$NEWPID" != "$PID6" ] && kill -0 "$NEWPID" 2>/dev/null; then
      ok "stale state detected and a fresh daemon was spawned (old pid $PID6, new pid $NEWPID)"
    else
      bad "stale state detected and a fresh daemon was spawned"
    fi
  else
    bad "stale state detected and a fresh daemon was spawned (no state file after recovery)"
  fi
fi
"$BIN" stop --dir "$DIR6" >/dev/null 2>&1 || true

# --- Scenario 10: double-start race converges on one daemon ---
log "Scenario 10: concurrent adds converge on exactly one daemon"
DIR7="$WORKDIR/s7"
"$BIN" add race-a --dir "$DIR7" >/tmp/e2e-race-a.$$ 2>&1 &
RACE_PID_A=$!
"$BIN" add race-b --dir "$DIR7" >/tmp/e2e-race-b.$$ 2>&1 &
RACE_PID_B=$!
wait "$RACE_PID_A"
wait "$RACE_PID_B"

if wait_for_file "$DIR7/daemon.json" 15; then
  ok "double-start race: a daemon came up"
else
  bad "double-start race: a daemon came up"
fi

# Count actually-listening scrim serve processes for this dir's port (default
# 7777 unless overridden) to prove convergence on one process, not just "no
# crash".
sleep 0.3
MATCHING_PIDS=$(pgrep -f "scrim serve --dir $DIR7" 2>/dev/null | wc -l | tr -d ' ')
if [ "$MATCHING_PIDS" = "1" ]; then
  ok "exactly one 'scrim serve' process is running for the racing dir (found $MATCHING_PIDS)"
else
  bad "exactly one 'scrim serve' process is running for the racing dir (found $MATCHING_PIDS)"
fi

LIST_OUT=$("$BIN" list --dir "$DIR7" 2>&1)
if echo "$LIST_OUT" | grep -q "race-a" && echo "$LIST_OUT" | grep -q "race-b"; then
  ok "both racing canvases exist against the single converged daemon"
else
  bad "both racing canvases exist against the single converged daemon"
fi

"$BIN" stop --dir "$DIR7" >/dev/null 2>&1 || true
rm -f /tmp/e2e-race-a.$$ /tmp/e2e-race-b.$$

# --- Summary ---
log "Summary"
echo "passed: $PASS"
echo "failed: $FAIL"
if [ "$FAIL" -gt 0 ]; then
  echo "failed scenarios:"
  for s in "${FAILED_SCENARIOS[@]}"; do
    echo "  - $s"
  done
  exit 1
fi
echo "all scenarios passed"
