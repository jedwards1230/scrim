# MCP server

`scrim mcp` exposes scrim's verbs as [MCP](https://modelcontextprotocol.io)
tools, so an agent drives scrim natively instead of shelling out.

Tools: `add`, `list`, `link`, `copy_canvas`, `rm`, `snap`, `snaps`, `revert`,
`status`, `list_files`, `read_file`, `write_file`, `edit_file`, `share_canvas`,
`list_grants`, `push` (plus `path` in local mode only — a server-local
directory has no remote meaning).

- `list_files` enumerates a canvas's files (paths + sizes, no content) so an
  agent can discover what to read or edit.
- `edit_file` applies an exact-string replacement server-side, so hub-mode
  edits cost tokens proportional to the change, not the file; it accepts an
  `edits` array to apply many replacements in one transactional
  (all-or-nothing) call.
- `write_file`/`read_file` accept an optional `encoding: "gzip+base64"` to move
  large or binary content compressed (the size cap applies to the decoded
  bytes).
- `copy_canvas` duplicates a canvas server-side.
- `share_canvas`/`list_grants` manage a canvas's view-only sharing grants over
  the machine API — see [identity.md](identity.md#ownership-sharing--tokens).

Each maps to the same code path as the matching verb, so the same safety
invariants hold: `link` returns a URL as data and **never** launches a browser,
no tool logs URLs/canvas content/tokens, and `push` is one-shot. Whichever
mode, `push` packs the canvas from the MCP server process's own disk, so a
remote/in-cluster deployment should author with `write_file`/`edit_file`
instead.

```jsonc
// e.g. an MCP client config — local mode
{ "command": "scrim", "args": ["mcp"] }
```

## Local vs hub mode

- **Local mode** (default): tools operate on the local daemon and the local
  canvas directory on disk. `add`/`path` return server-local filesystem paths,
  and `write_file`/`read_file` act on that directory — the right model when the
  agent and scrim share a machine.
- **Hub mode** (`--hub URL`): the same tool surface operates on a **remote** hub
  over its bearer-authenticated machine API — for a scrim mcp hosted away from
  the agent (e.g. in-cluster). Since there's no shared disk, authoring is done
  entirely through `write_file`/`read_file` (inline content, ~2 MiB cap);
  `path` is absent (a server-local path is meaningless remotely). A bearer token
  authenticates every call — from `SCRIM_PUSH_TOKEN` (the admin credential) or
  `--hub-token-file PATH` — and can be either the admin push token or a [user
  token](identity.md#ownership-sharing--tokens); `scrim mcp --hub` fails closed
  with no token. A user token attributes everything it creates/writes to its
  owner instead of the shared admin credential.

```jsonc
// hub mode — SCRIM_PUSH_TOKEN in the environment
{ "command": "scrim", "args": ["mcp", "--hub", "https://scrim-hub.example"] }
```

The tradeoff is disk vs token: local mode trusts the shared filesystem; hub
mode trusts the bearer token and moves bytes over HTTP.

## Streamable HTTP transport

Transport is stdio by default; pass `--http ADDR` for streamable HTTP. The HTTP
endpoint is unauthenticated by default, so it fails closed: a non-loopback bind
is refused unless you pass `--allow-lan` or configure OAuth resource mode
(below), which authenticates every request instead.
`scrim mcp --http 127.0.0.1:9797` is the safe default.

Two identity layers apply to `--http`. They compose, but for per-user
attribution a validated OAuth token wins (see precedence below):

- **Forwarded-identity (trusted gateway):** when scrim mcp sits behind a trusted
  gateway (e.g. the ContextForge MCP gateway, or any reverse proxy that
  authenticates the end user and forwards a signed principal), it verifies
  HMAC-signed `X-Forwarded-User-*` headers and re-emits the verified principal
  to the hub — see [identity.md](identity.md#the-forwarded-identity-plane).
  This attributes the END user of a call.
- **OAuth 2.0 resource mode** (below): authenticates the *client connection*
  itself (the MCP host presenting a bearer JWT) and, on the same path, derives
  per-user attribution from that validated token's `sub`/`email`/`groups`,
  re-emitting it to the hub as the same `X-Scrim-Actor-*` headers. Because the
  JWT is independently verified (signature/issuer/audience/expiry), the
  JWT-derived actor is **authoritative**: when a request carries a valid bearer
  the HMAC header plane above is not consulted — it is only the fallback when
  OAuth is off.

## OAuth 2.0 resource mode (`--http` only)

Setting `--oauth-issuer` turns `--http`'s `/mcp` endpoint into an
[RFC 9728](https://www.rfc-editor.org/rfc/rfc9728) OAuth 2.0 protected resource:
unauthenticated protected-resource metadata is served at
`/.well-known/oauth-protected-resource`, and every request to `/mcp` must carry
a bearer JWT that's validated (signature/issuer/audience/expiry, via the
issuer's JWKS) before it's served. A `tools/call` additionally needs the tool's
required scope — `scrim:read` for lookups, `scrim:write` for everything else (a
write-scoped token also satisfies a read requirement). A missing/invalid token
is `401`; an insufficient scope is `403`; both carry a `WWW-Authenticate`
challenge pointing at the metadata document. stdio is unaffected — it carries no
inbound HTTP request for this layer to check.

```bash
scrim mcp --http 0.0.0.0:9797 \
  --oauth-issuer https://auth.example.com/application/o/scrim-mcp/ \
  --oauth-audience scrim-mcp
```

- `--oauth-issuer` (env `SCRIM_MCP_OAUTH_ISSUER`) — the authorization server's
  issuer URL; setting it turns OAuth resource mode on. A bad/unreachable issuer
  fails startup (one-shot OIDC discovery). Once set, a non-loopback `--http`
  bind no longer needs `--allow-lan` — the endpoint is authenticated.
- `--oauth-audience` (env `SCRIM_MCP_OAUTH_AUDIENCE`) — the expected `aud` claim
  (the resource id the AS mints tokens for). Required whenever the issuer is
  set; a resource with no pinned audience would accept any token the AS ever
  issued, for any resource.
- `--oauth-resource` (env `SCRIM_MCP_OAUTH_RESOURCE`) — the canonical resource
  URL advertised in the metadata document. Optional: derived from the inbound
  request (honoring `X-Forwarded-Proto`) when unset; set it explicitly when that
  can't be derived correctly (e.g. behind a TLS-terminating proxy scrim can't
  see through).
