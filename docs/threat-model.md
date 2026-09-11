# Threat model

scrim's local daemon is a loopback-only, single-user tool; its threat surface is
small (a random capability token, owner-only file permissions, no request-path
logging — see the README's "Auth & privacy"). The **hub** is the network-facing
surface, and it makes three deliberate trade-offs worth stating plainly. Each is
a conscious choice with a mitigation, not an oversight.

## Push token is admin (read+write)

The hub's `--push-token` is **read+write, not write-only**, and it's the hub's
**admin/bootstrap** credential. A holder is unrestricted over the whole machine
API (`scrim mcp --hub`): it can read canvas content and file bytes
(`GET /api/canvases/{id}/files/...`), list, snapshot, and copy — not just push —
and it owns every legacy canvas.

- **Mitigation:** size its trust accordingly when you distribute it (e.g. to an
  in-cluster MCP deployment), and rotate it as a read-capable secret. A
  logged-in principal that wants its own scoped credential should mint a [user
  token](identity.md#ownership-sharing--tokens) instead of sharing the admin
  token. Browser reads remain separately gated either way — by OIDC login plus
  per-canvas visibility when `--oidc-issuer` is set (which replaces the CIDR
  allowlist and read token entirely; `scrim hub` warns that both are then
  ignored), and by that CIDR allowlist (+ optional read token) when it isn't —
  so the push token isn't the only thing standing between the network and
  canvas content.

## CIDR checked on RemoteAddr

The read allowlist (`--allow`) is checked against the client's transport
`RemoteAddr` — **never** `X-Forwarded-For`, which is trivially spoofable by any
client. This means the allowlist is only meaningful about the *directly
connected* peer.

- **Consequence:** if you front the hub with a reverse proxy, every request
  arrives from the proxy's address, so the CIDR gate can no longer distinguish
  clients — a trusted-proxy layer that consumes a validated forwarded address is
  a later phase. Until then, run the CIDR gate against direct connections, or
  gate reads with OIDC login instead.
- **Mitigation (actor attribution):** the re-emitted `X-Scrim-Actor-*` identity
  headers are trusted ONLY when they ride a valid admin push token, and a
  network policy pins the hub's machine-API ingress to scrim-mcp — so a peer
  that can't reach the hub over the allowed path can't present forged actor
  headers in the first place. Neither half suffices alone; together they bound
  attribution to "scrim-mcp, holding the admin token, on the allowed path."

## Revocable OIDC sessions, at the cost of a piece of state

> **Reversed 2026-09-08.** This section previously described sessions as
> stateless and non-revocable, with secret rotation as the only kill switch.
> Sessions are now server-side records; what follows is the trade-off that
> replaced it, not the one that was accepted before.

Every login is recorded in a registry under the hub's meta dir
(`internal/session`) and the request gate consults it, so a session can be
ended before its cookie expires — from `/tokens`, or by logging out, which now
drops the record as well as clearing the cookie. A cookie copied elsewhere dies
with the session rather than outliving it to its TTL. Only the User-Agent
string and the timestamps are recorded; **no IP address, ever**.

The cost is a piece of hub state on the authentication path, which brings its
own failure mode: if `sessions.json` exists but cannot be read or parsed, the
hub **fails closed** and refuses every session-authenticated request until an
operator repairs or removes it. A *missing* file is deliberately NOT that case —
it is an empty registry, which is exactly a hub's first boot. Getting those two
backwards would lock every user out permanently, so the distinction carries its
own test.

- **Mitigation:** writes are atomic (temp file + rename), so a reader only ever
  sees a whole file and a torn write can't wedge the store. The **admin push
  token never consults the registry at all** — it resolves before the session
  branch — so it stays usable as the recovery credential precisely when the
  registry is broken. `--oidc-session-ttl` still bounds a session nobody signs
  out — but it is now an **idle** window (default 168h), so it bounds an *unused*
  session, not a used one; the bound on a session in continuous use is
  `--oidc-session-max-lifetime` (default 720h), an absolute cap from login that
  renewal can never cross. An attacker holding a stolen cookie therefore keeps
  it alive by using it, up to that cap — which is why the revocable registry,
  not the TTL, is the real answer to a compromised session. Rotating
  `--oidc-session-secret` remains the all-at-once lever
  (every existing cookie's HMAC then fails to verify). Setting a stable secret
  (≥32 bytes) is still what lets sessions survive a restart.
