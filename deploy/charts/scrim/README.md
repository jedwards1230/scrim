# scrim Helm chart

Deploys scrim as **two workloads from one chart**:

| Workload | State | Role | Auth role |
|---|---|---|---|
| `hub` | **Stateful** — owns the canvases on a PVC | Serves the browser UI and the machine API | OIDC **relying party**: initiates login (client secret, redirect URL, session cookie) |
| `mcp` | **Stateless** — no PVC, read-only root | Runs the same image with the `mcp` verb; drives the hub's machine API with the push token | OAuth **resource server**: validates a bearer JWT someone else minted (audience + JWKS); never logs anyone in |

Both run the same image. The hub uses the image's default entrypoint; `mcp`
overrides the command.

## Why two workloads and not one

`mcp` is the internet-reachable component. Keeping it separate means the process
that terminates untrusted traffic holds **no user data**, has **no PVC**, and runs
with a **read-only root filesystem** — it can only reach canvases by calling the
hub's API with a token. Merging them would mount the canvas volume into that
process. `mcp.enabled` without `hub.enabled` is rejected at template time, because
the MCP server is a client of the hub, not a standalone service.

## Two auth surfaces, not one setting

`hub.oidc.*` and `mcp.oauth.*` are **opposite ends of OAuth** and share almost no
fields. A relying party has a client secret and a callback; a resource server has
an audience and a JWKS. This is why they need two IdP clients, not one.

`mcp.oauth.audience` is a **stable contract value**. It stays constant across an
IdP migration — only `issuer` moves. Don't change it to match a new provider's
naming.

## Install

```bash
helm install scrim oci://ghcr.io/jedwards1230/charts/scrim -n scrim --create-namespace
```

Use the release name `scrim` to get object names `scrim-hub` / `scrim-mcp`.

## Secrets

Portable by default: the chart references Secrets **by name** and never creates
them. Provide `secrets.tokensSecret` (keys `SCRIM_PUSH_TOKEN`,
`SCRIM_OIDC_SESSION_SECRET`) and, when OIDC is on, `secrets.oidcSecret` (key
`SCRIM_OIDC_CLIENT_SECRET`).

Set `onePassword.enabled=true` to additionally render `OnePasswordItem` CRDs that
materialize those Secrets. Off by default so the chart doesn't depend on the
1Password operator.

## Safety rails

The chart refuses to render rather than ship an open door:

- `mcp.ingress.enabled` with an empty `mcp.oauth.issuer` **fails** — that
  combination would publish an unauthenticated MCP endpoint.
- `hub`/`mcp` ingress enabled with an empty `host` fails.
- `mcp.enabled` without `hub.enabled` fails.

`NOTES.txt` warns when either component is running unauthenticated, since both
are legitimate configurations for a private network but neither should be a
surprise.

## ⚠️ Migrating from raw manifests

A Deployment's `spec.selector` is **immutable**. If you are replacing hand-written
manifests whose selector differs from this chart's, the first apply fails with a
field-immutable error.

The homelab manifests this chart replaces used:

```yaml
app.kubernetes.io/name: scrim-hub
app.kubernetes.io/instance: scrim-hub-prod
```

The chart uses `name` + `instance` + `component`. So the cutover needs a one-time
delete of the two Deployments before the first Helm apply:

```bash
kubectl delete deployment scrim-hub scrim-mcp -n scrim
```

**This does not touch the PVC** — canvases live on `scrim-hub-data`, a separate
object, and are unaffected. Downtime is the pod restart.

Verify the claim name matches before deleting anything:

```bash
kubectl get pvc -n scrim
```

If it differs from `<release>-hub-data`, set `hub.persistence.existingClaim` so
the chart binds the existing volume instead of provisioning a new one.

## Values

See [`values.yaml`](values.yaml) — every key is commented. The configuration this
chart replaced is kept as a rendering test at
[`ci/homelab-values.yaml`](ci/homelab-values.yaml).
