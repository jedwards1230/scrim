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
BIN2="$REPO_ROOT/.e2e-scrim-v2"
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
  rm -f "$BIN" "$BIN2"
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

# with_status_path rewrites a canvas URL (which may carry a "?t=..." query)
# into that daemon's /api/status URL, preserving the query string so the
# same token authenticates the status call.
with_status_path() {
  local url="$1" root query
  root="$(echo "$url" | sed -E 's#(https?://[^/]+).*#\1#')"
  case "$url" in
    *'?'*) query="${url#*\?}" ;;
    *) query="" ;;
  esac
  if [ -n "$query" ]; then
    echo "${root}/api/status?${query}"
  else
    echo "${root}/api/status"
  fi
}

# sse_client_count curls /api/status at the given (already token-qualified)
# status URL and extracts the "sse_clients" count. Echoes -1 if the request
# fails or the field can't be found, so callers can poll without tripping
# `set -e`-style failures.
sse_client_count() {
  local status_url="$1" body
  body=$(curl -fsS --max-time 2 "$status_url" 2>/dev/null) || {
    echo -1
    return
  }
  echo "$body" | grep -o '"sse_clients":[0-9]*' | grep -o '[0-9]*$' || echo -1
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

# CANVAS_URL already carries "?t=<token>" (auth is on by default). A valid
# query token now redirects (302) to the same URL with the token stripped
# (see internal/server/auth.go) rather than serving the request directly,
# so this follows the redirect (-L) and picks up/resends the cookie it sets
# along the way (-b/-c a shared jar) -- exactly what a real browser does --
# rather than expecting the first hop itself to return content. This is
# also the first half of the "valid token succeeds" auth assertion.
JAR1="$WORKDIR/jar1.txt"
BODY=$(curl -fsS -L -b "$JAR1" -c "$JAR1" "$CANVAS_URL" || true)
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
# Authenticate via the cookie JAR1 already holds (from the request above)
# rather than the query token -- this is what a real browser's own
# EventSource connection does too (the injected reload script's SSE URL
# never carries a token), and it avoids the token-redirect entirely for a
# long-lived streaming connection.
curl -fsS -N --max-time 6 -b "$JAR1" "$(strip_query "$EVENTS_URL")" >"$SSE_OUT" 2>/dev/null &
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
log "Scenario 5: a request with a valid token redirects (302), and following it succeeds (200)"
STATUS=$(curl -s -o /dev/null -w '%{http_code}' "$CANVAS_URL")
if [ "$STATUS" = "302" ]; then
  ok "request with a valid token gets 302 (token-stripping redirect)"
else
  bad "request with a valid token gets 302 (got $STATUS)"
fi
FOLLOWED_STATUS=$(curl -s -o /dev/null -w '%{http_code}' -L -b "$JAR1" -c "$JAR1" "$CANVAS_URL")
if [ "$FOLLOWED_STATUS" = "200" ]; then
  ok "following the redirect (cookie-authenticated) gets 200"
else
  bad "following the redirect (cookie-authenticated) gets 200 (got $FOLLOWED_STATUS)"
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

# --- Scenario 11: open prints the URL by default and never launches a
# browser unless explicitly opted in (--browser or SCRIM_OPEN_BROWSER=1) ---
# A stub "open"/"xdg-open" is put on PATH ahead of the real one so the
# opt-in case can assert a launch was actually attempted without ever
# popping a real browser tab on the machine running this script.
log "Scenario 11: open prints the URL by default (no browser launch); --browser/SCRIM_OPEN_BROWSER opts in"
DIR_OPEN="$WORKDIR/s-open"
"$BIN" add open-test --dir "$DIR_OPEN" --idle-timeout 5m >/dev/null

STUB_BIN_DIR="$WORKDIR/stub-bin"
mkdir -p "$STUB_BIN_DIR"
BROWSER_MARKER="$WORKDIR/browser-launch-marker"
case "$(uname -s)" in
Darwin) STUB_CMD_NAME="open" ;;
Linux) STUB_CMD_NAME="xdg-open" ;;
*) STUB_CMD_NAME="" ;;
esac
if [ -n "$STUB_CMD_NAME" ]; then
  cat >"$STUB_BIN_DIR/$STUB_CMD_NAME" <<EOF
#!/usr/bin/env bash
echo "\$@" > "$BROWSER_MARKER"
EOF
  chmod +x "$STUB_BIN_DIR/$STUB_CMD_NAME"
fi

# (a) default: no --browser, no env -- prints the URL, exits 0, and the
# stub is never invoked (no "could not open a browser" notice either, since
# openBrowser is never even called).
rm -f "$BROWSER_MARKER"
OPEN_OUT=$(PATH="$STUB_BIN_DIR:$PATH" "$BIN" open open-test --dir "$DIR_OPEN" 2>/tmp/e2e-open-stderr.$$)
OPEN_STATUS=$?
OPEN_ERR=$(cat /tmp/e2e-open-stderr.$$)
if [ "$OPEN_STATUS" -eq 0 ]; then
  ok "open (default) exits 0"
else
  bad "open (default) exits 0 (got $OPEN_STATUS)"
fi
if echo "$OPEN_OUT" | grep -q "^http://"; then
  ok "open (default) prints the canvas URL to stdout"
else
  bad "open (default) prints the canvas URL to stdout"
fi
if [ -f "$BROWSER_MARKER" ]; then
  bad "open (default) must NOT launch a browser (stub was invoked)"
else
  ok "open (default) does not launch a browser"
fi
if echo "$OPEN_ERR" | grep -q "could not open a browser"; then
  bad "open (default) must not print the auto-open notice (browser launch was never attempted)"
else
  ok "open (default) prints no auto-open notice"
fi
if echo "$OPEN_ERR" | grep -q "browser launch is opt-in"; then
  ok "open (default) prints the opt-in hint on stderr"
else
  bad "open (default) prints the opt-in hint on stderr"
fi

# (b) --browser opts in: same URL on stdout, plus an actual launch attempt.
if [ -n "$STUB_CMD_NAME" ]; then
  rm -f "$BROWSER_MARKER"
  OPEN_OUT=$(PATH="$STUB_BIN_DIR:$PATH" "$BIN" open open-test --dir "$DIR_OPEN" --browser 2>/tmp/e2e-open-stderr.$$)
  OPEN_STATUS=$?
  if [ "$OPEN_STATUS" -eq 0 ]; then
    ok "open --browser exits 0"
  else
    bad "open --browser exits 0 (got $OPEN_STATUS)"
  fi
  if [ -f "$BROWSER_MARKER" ]; then
    ok "open --browser attempts a browser launch (stub invoked)"
  else
    bad "open --browser attempts a browser launch (stub invoked)"
  fi
else
  printf "  [SKIP] Scenario 11(b): no stub command for uname -s=%s\n" "$(uname -s)"
fi

# (c) SCRIM_OPEN_BROWSER=1 opts in persistently, without the flag.
if [ -n "$STUB_CMD_NAME" ]; then
  rm -f "$BROWSER_MARKER"
  OPEN_OUT=$(PATH="$STUB_BIN_DIR:$PATH" SCRIM_OPEN_BROWSER=1 "$BIN" open open-test --dir "$DIR_OPEN" 2>/tmp/e2e-open-stderr.$$)
  OPEN_STATUS=$?
  if [ "$OPEN_STATUS" -eq 0 ]; then
    ok "SCRIM_OPEN_BROWSER=1 open exits 0"
  else
    bad "SCRIM_OPEN_BROWSER=1 open exits 0 (got $OPEN_STATUS)"
  fi
  if [ -f "$BROWSER_MARKER" ]; then
    ok "SCRIM_OPEN_BROWSER=1 attempts a browser launch without --browser (stub invoked)"
  else
    bad "SCRIM_OPEN_BROWSER=1 attempts a browser launch without --browser (stub invoked)"
  fi
else
  printf "  [SKIP] Scenario 11(c): no stub command for uname -s=%s\n" "$(uname -s)"
fi

rm -f /tmp/e2e-open-stderr.$$ "$BROWSER_MARKER"

# --- Scenario 11b: link is permanently print-only -- no flag, no env var
# reaches it (it doesn't even parse --browser), so the only thing worth
# proving here is that it prints the canonical URL and never attempts a
# launch, reusing the same PATH-stub trick as scenario 11. ---
log "Scenario 11b: link prints the canonical URL and never launches a browser"
if [ -n "$STUB_CMD_NAME" ]; then
  rm -f "$BROWSER_MARKER"
  LINK_OUT=$(PATH="$STUB_BIN_DIR:$PATH" SCRIM_OPEN_BROWSER=1 "$BIN" link open-test --dir "$DIR_OPEN" 2>/tmp/e2e-link-stderr.$$)
  LINK_STATUS=$?
  LINK_ERR=$(cat /tmp/e2e-link-stderr.$$)
  if [ "$LINK_STATUS" -eq 0 ]; then
    ok "link <id> exits 0"
  else
    bad "link <id> exits 0 (got $LINK_STATUS)"
  fi
  if echo "$LINK_OUT" | grep -q "^http://"; then
    ok "link <id> prints the canvas URL to stdout"
  else
    bad "link <id> prints the canvas URL to stdout"
  fi
  if [ -f "$BROWSER_MARKER" ]; then
    bad "link <id> must NOT launch a browser, even with SCRIM_OPEN_BROWSER=1 set (stub was invoked)"
  else
    ok "link <id> does not launch a browser, even with SCRIM_OPEN_BROWSER=1 set"
  fi
  if echo "$LINK_ERR" | grep -q "browser launch is opt-in"; then
    bad "link <id> must not print open's opt-in hint (link has no launch path to hint about)"
  else
    ok "link <id> prints no opt-in hint"
  fi
  rm -f /tmp/e2e-link-stderr.$$ "$BROWSER_MARKER"
else
  printf "  [SKIP] Scenario 11b: no stub command for uname -s=%s\n" "$(uname -s)"
fi

"$BIN" stop --dir "$DIR_OPEN" >/dev/null 2>&1 || true

# --- Scenario 12: version-skew restart ---
# The actual browser-launch exec (open/xdg-open/cmd) is exercised via the
# PATH-stub trick in scenario 11 above -- the platform command-selection
# logic itself (internal/openurl) is covered by unit tests; see
# internal/openurl/openurl_test.go.
log "Scenario 12: a CLI built at a different version transparently restarts a mismatched daemon"
DIR8="$WORKDIR/s8"
"$BIN" add version-test --dir "$DIR8" --idle-timeout 5m >/dev/null
PID8=$(pid_of_state "$DIR8/daemon.json")
if [ -n "${PID8:-}" ] && kill -0 "$PID8" 2>/dev/null; then
  ok "version-skew scenario: initial daemon started (pid $PID8)"
else
  bad "version-skew scenario: initial daemon started"
fi

# $BIN's own version is whatever `go build` picked up automatically (a git
# commit hash, since this is a git checkout) -- an explicit -ldflags override
# here is guaranteed to differ from it, without relying on the "dev" sentinel
# exemption (which deliberately skips this check).
if (cd "$REPO_ROOT" && go build -ldflags "-X github.com/jedwards1230/scrim/internal/version.Version=v9.9.9-e2e" -o "$BIN2" .); then
  ok "version-skew scenario: built a second binary at a distinct explicit version"
else
  bad "version-skew scenario: built a second binary at a distinct explicit version"
fi

# `list` self-starts (calls daemon.Ensure) without creating a new canvas.
"$BIN2" list --dir "$DIR8" >/dev/null 2>&1

RESTARTED=0
NEWPID8=""
DEADLINE=$((SECONDS + 15))
while [ $SECONDS -lt $DEADLINE ]; do
  NEWPID8=$(pid_of_state "$DIR8/daemon.json" 2>/dev/null)
  if [ -n "${NEWPID8:-}" ] && [ "$NEWPID8" != "$PID8" ] && kill -0 "$NEWPID8" 2>/dev/null; then
    RESTARTED=1
    break
  fi
  sleep 0.2
done

if [ "$RESTARTED" -eq 1 ]; then
  ok "version-mismatched daemon (pid $PID8) was replaced by a fresh one (pid $NEWPID8)"
else
  bad "version-mismatched daemon (pid $PID8) was replaced by a fresh one"
fi
if [ -n "${PID8:-}" ] && ! kill -0 "$PID8" 2>/dev/null; then
  ok "old daemon process (pid $PID8) actually exited"
else
  bad "old daemon process (pid $PID8) actually exited"
fi
if grep -q "v9.9.9-e2e" "$DIR8/daemon.json" 2>/dev/null; then
  ok "new daemon's state file reports the new CLI's version"
else
  bad "new daemon's state file reports the new CLI's version"
fi

"$BIN2" stop --dir "$DIR8" >/dev/null 2>&1 || true
rm -f "$BIN2"

# --- Scenario 13: stop succeeds promptly despite an open SSE connection ---
# Regression test for issue #11: a browser tab left open on a canvas holds
# its SSE connection open indefinitely, which used to block
# http.Server.Shutdown from completing until its own 5s internal deadline --
# often racing past `stop`'s own ~5s wait window and reporting a spurious
# timeout even though the daemon went on to exit anyway.
log "Scenario 13: stop succeeds within a few seconds despite an open SSE connection (issue #11)"
DIR9="$WORKDIR/s9"
OUT9=$("$BIN" add sse-stop-test --dir "$DIR9" --idle-timeout 5m 2>&1)
CANVAS_DIR9=$(echo "$OUT9" | sed -n '1p')
CANVAS_URL9=$(echo "$OUT9" | sed -n '2p')
echo '<html><body>sse-stop e2e</body></html>' >"$CANVAS_DIR9/index.html"

EVENTS_URL9="$(with_events_path "$CANVAS_URL9")"
STATUS_URL9="$(with_status_path "$CANVAS_URL9")"
SSE_OUT9="$WORKDIR/sse-stop-out.txt"
: >"$SSE_OUT9"
# A query token now redirects (302) rather than serving the request
# directly (see internal/server/auth.go), which would prevent this
# long-lived streaming connection from ever registering. Prime a cookie
# with a quick, redirect-followed throwaway request first, then open the
# actual SSE connection cookie-authenticated (no token in its URL at all,
# exactly like a real browser's own EventSource) so it's server directly
# rather than redirected.
JAR9="$WORKDIR/jar9.txt"
curl -fsS -L -o /dev/null -b "$JAR9" -c "$JAR9" "$CANVAS_URL9"
# --max-time is a generous safety net against a leaked process if an
# assertion below fails partway through -- it's well beyond how long this
# scenario should ever actually take, and the connection is explicitly
# killed right after use regardless of pass/fail.
curl -fsS -N --max-time 60 -b "$JAR9" "$(strip_query "$EVENTS_URL9")" >"$SSE_OUT9" 2>/dev/null &
SSE_CURL_PID=$!

# `kill -0` on the backgrounded curl PID only proves the curl *process* is
# alive -- it doesn't prove the SSE request actually reached the server and
# got registered in the hub (a curl still stuck in DNS/connect would pass
# the same check). Poll the daemon's own /api/status sse_clients count
# instead, which only goes to 1 once hub.register has actually run for
# this connection.
REG_DEADLINE=$((SECONDS + 5))
REGISTERED=0
while [ $SECONDS -lt $REG_DEADLINE ]; do
  if [ "$(sse_client_count "$STATUS_URL9")" = "1" ]; then
    REGISTERED=1
    break
  fi
  sleep 0.1
done
if [ "$REGISTERED" -eq 1 ] && kill -0 "$SSE_CURL_PID" 2>/dev/null; then
  ok "sse-stop scenario: SSE connection is open and registered with the hub before stop"
else
  bad "sse-stop scenario: SSE connection is open and registered with the hub before stop (hub sse_clients never reached 1)"
fi

STOP_START=$SECONDS
STOP_OUT9=$("$BIN" stop --dir "$DIR9" 2>&1)
STOP_STATUS=$?
STOP_ELAPSED=$((SECONDS - STOP_START))

if [ "$STOP_STATUS" -eq 0 ]; then
  ok "sse-stop scenario: stop exits 0 despite an open SSE connection (was: timed out waiting for daemon to stop)"
else
  bad "sse-stop scenario: stop exits 0 despite an open SSE connection (exit $STOP_STATUS: $STOP_OUT9)"
fi
if [ "$STOP_ELAPSED" -le 4 ]; then
  ok "sse-stop scenario: stop completed in ${STOP_ELAPSED}s (bounded well under the old ~5s timeout)"
else
  bad "sse-stop scenario: stop completed in ${STOP_ELAPSED}s, want <= 4s"
fi
if wait_for_file_gone "$DIR9/daemon.json" 5; then
  ok "sse-stop scenario: state file removed after stop"
else
  bad "sse-stop scenario: state file removed after stop"
fi

# Clean up the SSE connection regardless of how the assertions above went --
# by now the daemon should already have closed it on its way out, but kill
# it explicitly rather than relying on that.
kill "$SSE_CURL_PID" 2>/dev/null || true
wait "$SSE_CURL_PID" 2>/dev/null || true

# --- Scenario 14: SIGTERM to the daemon exits promptly despite an open SSE
# connection (issue #11, the ctx.Done() path specifically) ---
# Scenario 13 covers `scrim stop`, which goes through initiateShutdown /
# s.stopCh. A signal delivered straight to the daemon process instead goes
# through Run's ctx.Done() case (signal.NotifyContext in cli/serve.go) --
# a separate path that shipped without hub.closeAll wired in, even after
# PR #12's fix for the stop path. This sends SIGTERM directly to the daemon
# process (not via `scrim stop`) to exercise that path specifically.
log "Scenario 14: SIGTERM to the daemon process exits promptly despite an open SSE connection (issue #11, ctx.Done path)"
DIR10="$WORKDIR/s10"
OUT10=$("$BIN" add sigterm-test --dir "$DIR10" --idle-timeout 5m 2>&1)
CANVAS_DIR10=$(echo "$OUT10" | sed -n '1p')
CANVAS_URL10=$(echo "$OUT10" | sed -n '2p')
echo '<html><body>sigterm e2e</body></html>' >"$CANVAS_DIR10/index.html"

EVENTS_URL10="$(with_events_path "$CANVAS_URL10")"
STATUS_URL10="$(with_status_path "$CANVAS_URL10")"
SSE_OUT10="$WORKDIR/sse-sigterm-out.txt"
: >"$SSE_OUT10"
# See the identical comment in Scenario 13: prime a cookie via a
# redirect-followed throwaway request, then open the actual SSE connection
# cookie-authenticated so it's served directly instead of redirected.
JAR10="$WORKDIR/jar10.txt"
curl -fsS -L -o /dev/null -b "$JAR10" -c "$JAR10" "$CANVAS_URL10"
curl -fsS -N --max-time 60 -b "$JAR10" "$(strip_query "$EVENTS_URL10")" >"$SSE_OUT10" 2>/dev/null &
SSE_CURL_PID10=$!

REG_DEADLINE=$((SECONDS + 5))
REGISTERED10=0
while [ $SECONDS -lt $REG_DEADLINE ]; do
  if [ "$(sse_client_count "$STATUS_URL10")" = "1" ]; then
    REGISTERED10=1
    break
  fi
  sleep 0.1
done
if [ "$REGISTERED10" -eq 1 ]; then
  ok "sigterm scenario: SSE connection is open and registered with the hub before signal"
else
  bad "sigterm scenario: SSE connection is open and registered with the hub before signal (hub sse_clients never reached 1)"
fi

PID10=$(pid_of_state "$DIR10/daemon.json")
SIGTERM_START=$SECONDS
kill -TERM "$PID10" 2>/dev/null || true

SIGTERM_DEADLINE=$((SECONDS + 5))
EXITED10=0
while [ $SECONDS -lt $SIGTERM_DEADLINE ]; do
  if ! kill -0 "$PID10" 2>/dev/null; then
    EXITED10=1
    break
  fi
  sleep 0.1
done
SIGTERM_ELAPSED=$((SECONDS - SIGTERM_START))

if [ "$EXITED10" -eq 1 ]; then
  ok "sigterm scenario: daemon process exited after SIGTERM despite an open SSE connection (${SIGTERM_ELAPSED}s)"
else
  bad "sigterm scenario: daemon process exited after SIGTERM despite an open SSE connection"
fi
if [ "$SIGTERM_ELAPSED" -le 4 ]; then
  ok "sigterm scenario: exited in ${SIGTERM_ELAPSED}s (bounded well under the old ~5s timeout)"
else
  bad "sigterm scenario: exited in ${SIGTERM_ELAPSED}s, want <= 4s"
fi
if wait_for_file_gone "$DIR10/daemon.json" 5; then
  ok "sigterm scenario: state file removed after SIGTERM"
else
  bad "sigterm scenario: state file removed after SIGTERM"
fi

kill "$SSE_CURL_PID10" 2>/dev/null || true
wait "$SSE_CURL_PID10" 2>/dev/null || true

# --- Scenario 15: index.md renders via the skeleton, and SSE live-reload
# still fires when the markdown file itself is touched ---
log "Scenario 15: a canvas with only index.md renders via goldmark + the skeleton, and SSE live-reload works when the .md file is touched"
DIR11="$WORKDIR/s11"
OUT11=$("$BIN" add md-test --dir "$DIR11" --idle-timeout 5m 2>&1)
CANVAS_DIR11=$(echo "$OUT11" | sed -n '1p')
CANVAS_URL11=$(echo "$OUT11" | sed -n '2p')
printf '# Hello Markdown\n\nSome *body* text.\n' >"$CANVAS_DIR11/index.md"

# A valid query token now redirects (302) to a token-stripped URL rather
# than serving the request directly (see internal/server/auth.go) -- follow
# it (-L), picking up and resending the cookie it sets along the way
# (-b/-c a jar), just like a real browser would.
JAR11="$WORKDIR/jar11.txt"
BODY11=$(curl -fsS -L -b "$JAR11" -c "$JAR11" "$CANVAS_URL11" || true)
if echo "$BODY11" | grep -q "<h1>Hello Markdown</h1>"; then
  ok "index.md scenario: response contains goldmark-rendered heading"
else
  bad "index.md scenario: response contains goldmark-rendered heading"
fi
if echo "$BODY11" | grep -q "scrim:skeleton"; then
  ok "index.md scenario: response is wrapped in scrim's skeleton"
else
  bad "index.md scenario: response is wrapped in scrim's skeleton"
fi
if echo "$BODY11" | grep -q "__events" && echo "$BODY11" | grep -q "<script>"; then
  ok "index.md scenario: response contains injected SSE <script>"
else
  bad "index.md scenario: response contains injected SSE <script>"
fi

# Authenticate the long-lived SSE connection via the cookie JAR11 already
# holds (from the request above), not the query token -- exactly like a
# real browser's own EventSource connection (see the identical pattern and
# comment in Scenario 3) -- so it's served directly instead of redirected.
EVENTS_URL11="$(with_events_path "$CANVAS_URL11")"
SSE_OUT11="$WORKDIR/sse-md-out.txt"
: >"$SSE_OUT11"
curl -fsS -N --max-time 6 -b "$JAR11" "$(strip_query "$EVENTS_URL11")" >"$SSE_OUT11" 2>/dev/null &
CURL_PID11=$!
sleep 0.5 # let the SSE connection register before we touch the file
printf '# Hello Markdown\n\nUpdated *body* text.\n' >"$CANVAS_DIR11/index.md"

SSE_DEADLINE11=$((SECONDS + 6))
GOT_RELOAD11=0
while [ $SECONDS -lt $SSE_DEADLINE11 ]; do
  if grep -q "event: reload" "$SSE_OUT11" 2>/dev/null; then
    GOT_RELOAD11=1
    break
  fi
  sleep 0.2
done
kill "$CURL_PID11" 2>/dev/null || true
wait "$CURL_PID11" 2>/dev/null || true

if [ "$GOT_RELOAD11" -eq 1 ]; then
  ok "index.md scenario: SSE reload event received within 6s of touching the .md file"
else
  bad "index.md scenario: SSE reload event received within 6s of touching the .md file"
fi
"$BIN" stop --dir "$DIR11" >/dev/null 2>&1 || true

# --- Scenario 16: a bare HTML fragment (no doctype/html tag) is wrapped in
# the skeleton ---
log "Scenario 16: an HTML fragment with no doctype/html wrapper renders wrapped in the skeleton"
DIR12="$WORKDIR/s12"
OUT12=$("$BIN" add fragment-test --dir "$DIR12" --idle-timeout 5m 2>&1)
CANVAS_DIR12=$(echo "$OUT12" | sed -n '1p')
CANVAS_URL12=$(echo "$OUT12" | sed -n '2p')
printf '<h1>Just a fragment</h1>\n<p>no doctype or html tag here</p>\n' >"$CANVAS_DIR12/index.html"

# Follow the token-stripping redirect (-L) with a jar, same as Scenario 15.
BODY12=$(curl -fsS -L -b "$WORKDIR/jar12.txt" -c "$WORKDIR/jar12.txt" "$CANVAS_URL12" || true)
if echo "$BODY12" | grep -q "Just a fragment"; then
  ok "fragment scenario: response contains the fragment's original content"
else
  bad "fragment scenario: response contains the fragment's original content"
fi
if echo "$BODY12" | grep -q 'name="viewport"' && echo "$BODY12" | grep -q "scrim:skeleton"; then
  ok "fragment scenario: response contains the skeleton's viewport meta tag and marker"
else
  bad "fragment scenario: response contains the skeleton's viewport meta tag and marker"
fi
"$BIN" stop --dir "$DIR12" >/dev/null 2>&1 || true

# --- Scenario 17: a complete HTML document passes through unwrapped ---
log "Scenario 17: a complete HTML document (with <!doctype html>) is served byte-equivalent to the original modulo reload-script injection only"
DIR13="$WORKDIR/s13"
OUT13=$("$BIN" add complete-test --dir "$DIR13" --idle-timeout 5m 2>&1)
CANVAS_DIR13=$(echo "$OUT13" | sed -n '1p')
CANVAS_URL13=$(echo "$OUT13" | sed -n '2p')
printf '<!doctype html>\n<html><head><title>e2e complete</title></head><body><h1>Complete Doc</h1></body></html>\n' >"$CANVAS_DIR13/index.html"

# Follow the token-stripping redirect (-L) with a jar, same as Scenario 15.
BODY13=$(curl -fsS -L -b "$WORKDIR/jar13.txt" -c "$WORKDIR/jar13.txt" "$CANVAS_URL13" || true)
if echo "$BODY13" | grep -q "<title>e2e complete</title>" && echo "$BODY13" | grep -q "<h1>Complete Doc</h1>"; then
  ok "complete-document scenario: original document content is present verbatim"
else
  bad "complete-document scenario: original document content is present verbatim"
fi
if echo "$BODY13" | grep -q "scrim:skeleton"; then
  bad "complete-document scenario: skeleton must NOT be applied to a complete document (found scrim:skeleton marker)"
else
  ok "complete-document scenario: skeleton is not applied (no scrim:skeleton marker)"
fi
DOCTYPE_COUNT13=$(echo "$BODY13" | grep -oi "<!doctype" | wc -l | tr -d ' ')
HTML_TAG_COUNT13=$(echo "$BODY13" | grep -oi "<html" | wc -l | tr -d ' ')
if [ "$DOCTYPE_COUNT13" = "1" ] && [ "$HTML_TAG_COUNT13" = "1" ]; then
  ok "complete-document scenario: no double-wrapping (exactly one <!doctype> and one <html> tag)"
else
  bad "complete-document scenario: no double-wrapping (found $DOCTYPE_COUNT13 <!doctype>, $HTML_TAG_COUNT13 <html> tags)"
fi
if echo "$BODY13" | grep -q 'name="viewport"'; then
  bad "complete-document scenario: no duplicate viewport tag introduced by the skeleton (found one, original had none)"
else
  ok "complete-document scenario: no viewport tag introduced by the skeleton (original had none)"
fi
if echo "$BODY13" | grep -q "__events" && echo "$BODY13" | grep -q "<script>"; then
  ok "complete-document scenario: reload-script injection still applies"
else
  bad "complete-document scenario: reload-script injection still applies"
fi
"$BIN" stop --dir "$DIR13" >/dev/null 2>&1 || true

# --- Scenario 18: add --title/--desc/--icon renders in the dashboard gallery ---
log "Scenario 18: add --title/--desc/--icon renders in the dashboard gallery"
DIR14="$WORKDIR/s14"
"$BIN" add gallery-test --title "Gallery Title" --desc "Gallery Desc" --icon "🎨" --dir "$DIR14" --idle-timeout 5m >/dev/null
if [ -f "$DIR14/daemon.json" ]; then
  ok "gallery scenario: daemon started"
else
  bad "gallery scenario: daemon started"
fi

# `open` with no id prints the dashboard's own (token-qualified) URL on its
# first stdout line and always exits 0, whether or not a real browser is
# available (see Scenario 11) -- the same mechanism used there to get a
# fully-qualified URL without hardcoding the auth token here.
#
# The dashboard URL carries a valid query token, which now redirects (302) to
# a token-stripped URL rather than serving the request directly (see
# internal/server/auth.go) -- follow it (-L), picking up and resending the
# cookie it sets along the way (-b/-c a jar), just like a real browser would
# (same pattern as Scenario 5/21).
DASHBOARD_URL14=$("$BIN" open --dir "$DIR14" 2>/dev/null | head -1)
JAR14="$WORKDIR/jar14.txt"
BODY14=$(curl -fsS -L -b "$JAR14" -c "$JAR14" "$DASHBOARD_URL14" || true)
if echo "$BODY14" | grep -q "Gallery Title"; then
  ok "gallery scenario: dashboard HTML contains the canvas title"
else
  bad "gallery scenario: dashboard HTML contains the canvas title"
fi
if echo "$BODY14" | grep -q "Gallery Desc"; then
  ok "gallery scenario: dashboard HTML contains the canvas description"
else
  bad "gallery scenario: dashboard HTML contains the canvas description"
fi
if echo "$BODY14" | grep -q "🎨"; then
  ok "gallery scenario: dashboard HTML contains the canvas icon"
else
  bad "gallery scenario: dashboard HTML contains the canvas icon"
fi
"$BIN" stop --dir "$DIR14" >/dev/null 2>&1 || true

# --- Scenario 19: snap + snaps are pure filesystem operations (no daemon) ---
log "Scenario 19: snap + snaps are pure filesystem operations, no daemon required"
DIR15="$WORKDIR/s15"
CANVAS_DIR15=$("$BIN" path snaptest --dir "$DIR15")
mkdir -p "$CANVAS_DIR15"
echo '<html><body>v1</body></html>' >"$CANVAS_DIR15/index.html"

SNAP_OUT=$("$BIN" snap snaptest --label mysnap --dir "$DIR15" 2>&1)
if [ -f "$DIR15/daemon.json" ]; then
  bad "snap scenario: snap did not self-start the daemon (found daemon.json)"
else
  ok "snap scenario: snap did not self-start the daemon"
fi
if echo "$SNAP_OUT" | grep -q "mysnap"; then
  ok "snap scenario: snap reports the label"
else
  bad "snap scenario: snap reports the label"
fi

SNAPS_OUT=$("$BIN" snaps snaptest --dir "$DIR15" 2>&1)
if echo "$SNAPS_OUT" | grep -q "mysnap"; then
  ok "snaps scenario: snaps lists the snapshot"
else
  bad "snaps scenario: snaps lists the snapshot"
fi

# --- Scenario 20: modify after a snapshot, then revert restores it ---
log "Scenario 20: modify after a snapshot, then revert (default: latest) restores the pre-modification contents"
DIR16="$WORKDIR/s16"
CANVAS_DIR16=$("$BIN" path reverttest --dir "$DIR16")
mkdir -p "$CANVAS_DIR16"
echo '<html><body>pre-modification</body></html>' >"$CANVAS_DIR16/index.html"

"$BIN" snap reverttest --dir "$DIR16" >/dev/null 2>&1

echo '<html><body>MODIFIED</body></html>' >"$CANVAS_DIR16/index.html"
echo "extra" >"$CANVAS_DIR16/extra.txt"

"$BIN" revert reverttest --dir "$DIR16" >/dev/null 2>&1
if [ -f "$DIR16/daemon.json" ]; then
  bad "revert scenario: revert did not self-start the daemon (found daemon.json)"
else
  ok "revert scenario: revert did not self-start the daemon"
fi
if grep -q "pre-modification" "$CANVAS_DIR16/index.html" 2>/dev/null; then
  ok "revert scenario: canvas contents match the pre-modification snapshot"
else
  bad "revert scenario: canvas contents match the pre-modification snapshot"
fi
if [ ! -f "$CANVAS_DIR16/extra.txt" ]; then
  ok "revert scenario: a file added after the snapshot is gone post-revert (replace, not merge)"
else
  bad "revert scenario: a file added after the snapshot is gone post-revert (replace, not merge)"
fi

# revert takes its own "prerevert" safety snapshot of the pre-revert state
# before restoring -- confirm it actually did.
PREREVERT_SNAPS=$("$BIN" snaps reverttest --dir "$DIR16" 2>&1)
if echo "$PREREVERT_SNAPS" | grep -q "prerevert"; then
  ok "revert scenario: a prerevert safety snapshot was taken automatically"
else
  bad "revert scenario: a prerevert safety snapshot was taken automatically"
fi

# --- Scenario 21: privacy -- the daemon log never contains tokens, canvas
# paths, or canvas IDs ---
# This is the load-bearing regression test for the whole privacy-hardening
# epic: it drives a mix of real traffic (a successful, cookie-authenticated
# canvas request following the token-stripping redirect; a genuine 404; a
# 401 for a missing token; a 401 for a wrong token) through a daemon, then
# greps its ENTIRE log output -- both the log file (item 2) and, since the
# daemon's stdout/stderr are fully redirected to that same file (cmd.Stdout
# = cmd.Stderr = logFile in spawnAndWait), there is nothing else to check
# separately -- for the capability token, any "/c/" path fragment, and the
# canvas ID. It's run twice for stability, since a flaky pass here would be
# far worse than a flaky pass anywhere else in this suite.
log "Scenario 21: privacy -- daemon log never contains tokens, paths, or canvas IDs"

run_privacy_scenario() {
  local n="$1"
  local dir="$WORKDIR/privacy-$n"
  local id="privacy-test-$n"
  local jar="$WORKDIR/privacy-cookies-$n.txt"
  local out canvas_dir canvas_url token base body status

  out=$("$BIN" add "$id" --dir "$dir" --idle-timeout 5m 2>&1)
  canvas_dir=$(echo "$out" | sed -n '1p')
  canvas_url=$(echo "$out" | sed -n '2p')
  echo '<html><body>privacy e2e content</body></html>' >"$canvas_dir/index.html"

  token=$(echo "$canvas_url" | sed -E 's/.*[?&]t=([^&]*).*/\1/')
  base="$(strip_query "$canvas_url")"

  # 1. A successful request: follow the token-stripping redirect (-L),
  # picking up and resending the cookie it sets along the way (-b/-c), just
  # like a real browser would.
  body=$(curl -fsS -L -b "$jar" -c "$jar" "$canvas_url" || true)
  if echo "$body" | grep -q "privacy e2e content"; then
    ok "privacy run $n: successful canvas request serves real content"
  else
    bad "privacy run $n: successful canvas request serves real content"
  fi

  # 2. A genuine 404: cookie-authenticated (from step 1's jar), no query
  # token, for a path that doesn't exist.
  status=$(curl -s -o /dev/null -w '%{http_code}' -b "$jar" "${base}does-not-exist.html")
  if [ "$status" = "404" ]; then
    ok "privacy run $n: nonexistent path gets 404"
  else
    bad "privacy run $n: nonexistent path gets 404 (got $status)"
  fi

  # 3. 401 for no token and no cookie at all.
  status=$(curl -s -o /dev/null -w '%{http_code}' "$base")
  if [ "$status" = "401" ]; then
    ok "privacy run $n: no token/cookie gets 401"
  else
    bad "privacy run $n: no token/cookie gets 401 (got $status)"
  fi

  # 4. 401 for a wrong token.
  status=$(curl -s -o /dev/null -w '%{http_code}' "${base}?t=totally-wrong-token")
  if [ "$status" = "401" ]; then
    ok "privacy run $n: wrong token gets 401"
  else
    bad "privacy run $n: wrong token gets 401 (got $status)"
  fi

  "$BIN" stop --dir "$dir" >/dev/null 2>&1 || true
  wait_for_file_gone "$dir/daemon.json" 5 || true

  local logfile="$dir/daemon.log"
  if [ -f "$logfile" ]; then
    ok "privacy run $n: daemon log file exists"
  else
    bad "privacy run $n: daemon log file exists"
    return
  fi

  if grep -qF "$token" "$logfile"; then
    bad "privacy run $n: daemon log does not contain the capability token"
  else
    ok "privacy run $n: daemon log does not contain the capability token"
  fi
  if grep -qF "/c/" "$logfile"; then
    bad "privacy run $n: daemon log does not contain a /c/ path fragment"
  else
    ok "privacy run $n: daemon log does not contain a /c/ path fragment"
  fi
  if grep -qF "$id" "$logfile"; then
    bad "privacy run $n: daemon log does not contain the canvas ID"
  else
    ok "privacy run $n: daemon log does not contain the canvas ID"
  fi
}

run_privacy_scenario 1
run_privacy_scenario 2

# --- Scenario 22: hub central store -- push, curl-served canvas, wrong/
# missing push token rejected on writes, CIDR-denied hub returns 403 on
# reads ---
# Every hub instance here uses its own isolated --data dir under $WORKDIR
# (so the existing cleanup() trap's daemon.json glob finds and kills it if
# anything below fails) and a dedicated high port -- never the default
# daemon's ~/.scrim or port 7777.
log "Scenario 22: hub central store (push, curl-served canvas, push-token gate, CIDR gate)"
HUB1_DATA="$WORKDIR/hub1-data"
HUB1_PORT=19291
HUB_PUSH_TOKEN="e2e-hub-push-token"
DIR_PUSH_SRC="$WORKDIR/push-src"

# A local canvas that will be pushed to the hub. It's never served from
# here in this scenario -- `scrim push` reads it straight off disk -- so the
# local daemon that `add` self-started is stopped again immediately.
OUT_PUSH=$("$BIN" add hub-push-test --dir "$DIR_PUSH_SRC" --port 19292 --idle-timeout 5m --title "Hub Push Test" 2>&1)
PUSH_SRC_CANVAS_DIR=$(echo "$OUT_PUSH" | sed -n '1p')
echo '<html><body><h1>hub e2e content</h1></body></html>' >"$PUSH_SRC_CANVAS_DIR/index.html"
"$BIN" stop --dir "$DIR_PUSH_SRC" >/dev/null 2>&1 || true

"$BIN" hub --data "$HUB1_DATA" --port "$HUB1_PORT" --push-token "$HUB_PUSH_TOKEN" --allow 127.0.0.0/8 >"$WORKDIR/hub1.log" 2>&1 &
HUB1_PID=$!
if wait_for_file "$HUB1_DATA/daemon.json" 10; then
  ok "hub scenario: hub 1 started"
else
  bad "hub scenario: hub 1 started"
fi

PUSH_OUT=$("$BIN" push hub-push-test --to "http://127.0.0.1:$HUB1_PORT" --token "$HUB_PUSH_TOKEN" --dir "$DIR_PUSH_SRC" 2>&1)
if echo "$PUSH_OUT" | grep -q "^http://127.0.0.1:$HUB1_PORT/c/hub-push-test/"; then
  ok "hub scenario: push reports the hub canvas URL"
else
  bad "hub scenario: push reports the hub canvas URL (got: $PUSH_OUT)"
fi

HUB_BODY=$(curl -fsS "http://127.0.0.1:$HUB1_PORT/c/hub-push-test/" || true)
if echo "$HUB_BODY" | grep -q "hub e2e content"; then
  ok "hub scenario: hub serves the pushed canvas HTML"
else
  bad "hub scenario: hub serves the pushed canvas HTML"
fi
if echo "$HUB_BODY" | grep -q "__events" && echo "$HUB_BODY" | grep -q "<script>"; then
  ok "hub scenario: hub-served canvas has the injected SSE reload script"
else
  bad "hub scenario: hub-served canvas has the injected SSE reload script"
fi

# A write (push) with no bearer token at all -> 401.
STATUS=$(curl -s -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:$HUB1_PORT/api/push/hub-push-test" --data-binary "")
if [ "$STATUS" = "401" ]; then
  ok "hub scenario: push with no bearer token gets 401"
else
  bad "hub scenario: push with no bearer token gets 401 (got $STATUS)"
fi
# A write (push) with the wrong bearer token -> 401.
STATUS=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer wrong-token" "http://127.0.0.1:$HUB1_PORT/api/push/hub-push-test" --data-binary "")
if [ "$STATUS" = "401" ]; then
  ok "hub scenario: push with wrong bearer token gets 401"
else
  bad "hub scenario: push with wrong bearer token gets 401 (got $STATUS)"
fi

# The liveness/readiness probe is gate-exempt: an unauthenticated,
# non-browser request (no bearer, no session -- exactly a kubelet probe)
# gets 200 with no body, even though this hub CIDR-restricts ordinary reads
# to loopback. It needs no OIDC (this hub has none) and reveals nothing (#47).
STATUS=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$HUB1_PORT/healthz")
if [ "$STATUS" = "200" ]; then
  ok "hub scenario: unauthenticated GET /healthz gets 200 (gate-exempt probe)"
else
  bad "hub scenario: unauthenticated GET /healthz gets 200 (got $STATUS)"
fi

# NOTE: the #49 identity scenarios -- private-by-default (unauth API/SSE 401,
# browser 302 login), owner-only visibility, and each share-grant kind
# (user/group/everyone/link via ?k=) -- require a live OIDC IdP, which an
# OIDC-configured hub fails closed without at startup. Standing up a real IdP
# in shell is impractical, so those scenarios live as Go httptest+oidctest
# integration tests in internal/server/hubgate_identity_test.go instead (per
# the PR contract). healthz above is the identity-adjacent scenario doable
# here (it needs no OIDC).

# A second hub, with a CIDR allowlist that deliberately excludes loopback --
# a 127.0.0.1 read against it must be refused (403), not merely
# unauthenticated (401).
HUB2_DATA="$WORKDIR/hub2-data"
HUB2_PORT=19391
"$BIN" hub --data "$HUB2_DATA" --port "$HUB2_PORT" --push-token "e2e-hub2-push-token" --allow 10.0.0.0/8 >"$WORKDIR/hub2.log" 2>&1 &
HUB2_PID=$!
if wait_for_file "$HUB2_DATA/daemon.json" 10; then
  ok "hub scenario: hub 2 (10.0.0.0/8-only allowlist) started"
else
  bad "hub scenario: hub 2 (10.0.0.0/8-only allowlist) started"
fi

STATUS=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$HUB2_PORT/")
if [ "$STATUS" = "403" ]; then
  ok "hub scenario: a 127.0.0.1 read against a 10.0.0.0/8-only allowlist gets 403"
else
  bad "hub scenario: a 127.0.0.1 read against a 10.0.0.0/8-only allowlist gets 403 (got $STATUS)"
fi

# Stop both hubs. A hub's own /api/stop is gated behind the push token as a
# write (not exposed via `scrim stop`'s query-token apiclient call), so it's
# stopped the way a container runtime/systemd actually would: a signal to
# the process, same as Scenario 14's SIGTERM-to-the-daemon path.
kill -TERM "$HUB1_PID" 2>/dev/null || true
kill -TERM "$HUB2_PID" 2>/dev/null || true
if wait_for_file_gone "$HUB1_DATA/daemon.json" 5; then
  ok "hub scenario: hub 1 stopped cleanly on SIGTERM"
else
  bad "hub scenario: hub 1 stopped cleanly on SIGTERM"
fi
if wait_for_file_gone "$HUB2_DATA/daemon.json" 5; then
  ok "hub scenario: hub 2 stopped cleanly on SIGTERM"
else
  bad "hub scenario: hub 2 stopped cleanly on SIGTERM"
fi

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
