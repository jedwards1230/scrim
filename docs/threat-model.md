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
  token. Browser reads remain separately gated by the CIDR allowlist (+ optional
  read token), so the push token isn't the only thing standing between the
  network and canvas content.

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

## Stateless, non-revocable OIDC sessions

Hub OIDC sessions are **stateless** — there is no server-side session store, so
a session can't be revoked before it expires. `/auth/logout` only clears the
cookie in that one browser; a compromised cookie stays valid until its TTL
lapses.

- **Mitigation:** keep `--oidc-session-ttl` modest (default `12h`) so the window
  of a leaked cookie is bounded. To invalidate **all** sessions at once in an
  emergency, rotate `--oidc-session-secret` — every existing cookie's HMAC then
  fails to verify. Setting a stable secret (≥32 bytes) is what lets sessions
  survive restarts/replicas in the first place; rotating it is the deliberate
  kill switch.
