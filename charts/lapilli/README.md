# Lapilli Helm chart

A flight recorder for Kubernetes incidents. When an alert fires, Lapilli captures the incident
window — pod and owner-chain objects, an events snapshot, log tails including the previous
container, what the last rollout changed, optionally a PromQL range — and seals it into one
portable, verifiable `.ieb` file.

Full documentation lives in the repository, not here: [`README.md`](../../README.md) for the
product, [`DESIGN.md`](../../DESIGN.md) for what it does and does not claim, and
[`docs/`](../../docs) for each subsystem.

```console
# from a checkout
helm install lapilli charts/lapilli -n lapilli-system --create-namespace --set clusterId=prod-apne2

# once the chart is published
helm install lapilli oci://ghcr.io/lapilli-project/charts/lapilli \
  -n lapilli-system --create-namespace --set clusterId=prod-apne2
```

`helm install` prints NOTES with the Alertmanager receiver config, including the webhook token the
chart generated. Requires Kubernetes ≥ 1.30 (see [Compatibility](#compatibility)).

## What it installs

| Object | Notes |
|---|---|
| `Deployment` | one replica: the controller, plus an optional `mcp` sidecar. The service account token is a projected volume mounted by the controller container only. |
| `ServiceAccount`, `ClusterRole`/`ClusterRoleBinding` (or one `Role` per namespace) | read-only on the workload objects Lapilli collects. `watchNamespaces` decides which of the two. |
| `Role`/`RoleBinding` in the release namespace | its own CRs, `get` on the signing-key Secret when `signing.mode=static`, and events. |
| `Service` | the Alertmanager webhook on `:8080`. A second Service for `mcp` and one for metrics when those are enabled. |
| `PersistentVolumeClaim` | where sealed bundles are written. **Kept on uninstall** and reused on reinstall. |
| `CaptureProfile` (`profile.create`) | what to collect, the window, redaction, where to export, which notify route. |
| `Secret` | the generated webhook bearer token, and the `mcp` token when enabled. Kept across upgrades. |
| `ConfigMap` | the export destinations the admin defined. |
| `ServiceMonitor`, `NetworkPolicy` | opt-in, off by default. |

The two CRDs (`IncidentCapture`, `CaptureProfile`) are in `crds/`. **Helm installs those on
`install` and never on `upgrade`** — upgrading a CRD is a manual delete-and-recreate that cascades
every `IncidentCapture` away. See [`docs/COMPATIBILITY.md`](../../docs/COMPATIBILITY.md) §5.

## Values an operator has to decide

Everything else has a defensible default. These do not:

| Value | Default | The decision |
|---|---|---|
| `clusterId` | `kubernetes` | Recorded in every bundle and checked by `lapilli verify --cluster`. Make it mean something per cluster (`prod-apne2`). |
| `watchNamespaces` | `[]` = cluster-wide | A list renders one namespaced `Role` per entry instead of a `ClusterRole`. Narrower is better; `diffs.configMaps` requires a non-empty list. |
| `persistence.size` | `1Gi` | Size it from **your** bundles (`lapilli_bundle_bytes`), not from the comment: a full volume makes every capture fail, not just the old ones. |
| `retention.*` | all off | Nothing deletes a sealed bundle until you choose a bound. Read [`docs/design-retention.md`](../../docs/design-retention.md) first — with no export destination the local bundle is the only copy. |
| `export.destinations` | `[]` | S3/GCS, admin-defined only; a profile may name one and change nothing about it. Without one, the PVC holds the only copy. [`docs/design-export.md`](../../docs/design-export.md) |
| `signing.mode` | `none` | `static` or `kms`. Unsigned bundles prove nothing against anyone who could write to them (`DESIGN.md` §5). [`docs/kms.md`](../../docs/kms.md) |
| `redaction.mode` | `default` | `strict` redacts every candidate value. **There is no admin floor**: anyone who can patch a `CaptureProfile` can set `off`. See the comment on the value. |
| `notify.routes` + `profile.notifyRoute` | none | One grouped message per incident, to Slack or a JSON endpoint. [`docs/design-notify.md`](../../docs/design-notify.md) |
| `webhook.auth.enabled` | `true` | Leave it on. With `helm template`-based GitOps (Argo CD) you must supply `existingSecret` yourself, because a generated token is not stable across renders. |
| `metrics.prometheusUrl` | `""` | Set it and the `metrics` collector is added to the profile automatically. |
| `mcp.enabled` | `false` | Serves the bundle volume over MCP to an agent. `mcp.allowLogs` sends container logs into that agent's context — a separate, deliberate switch. |
| `terminationGracePeriodSeconds` | `30` | The controller uses it to finish captures in flight and flush notifications. The chart refuses to render below 25. |

`values.yaml` is the reference: every value carries the reasoning and the measurement behind its
default, and `values.schema.json` rejects unknown keys and bad shapes at render time.

## Compatibility

`kubeVersion: ">=1.30.0-0"`, which is what the project claims to support — tested end to end on 1.30
and 1.37, expected to work on 1.31–1.36 ([`docs/COMPATIBILITY.md`](../../docs/COMPATIBILITY.md) §4).
The chart's own manifests need less than that (the projected `serviceAccountToken` volume and the
`kube-root-ca.crt` ConfigMap need ≥ 1.21), but an install on a version nobody has run is not
something to advertise. The chart version equals the app version.

## Before you put this in front of an auditor

- Bundles can hold personal data. Redaction is best-effort and **container logs are not redacted at
  all** (`DESIGN.md` §5). See [`docs/data-handling.md`](../../docs/data-handling.md).
- WORM / Object Lock storage is the recommendation for audit use, and it means nothing can delete a
  bundle until the lock expires — including you. Set that retention period against the legal
  retention period for what the bundles hold. [`docs/design-export.md`](../../docs/design-export.md)
- Lapilli is one link in a chain of custody, not the whole chain. `DESIGN.md` §5 states what it
  never claims.

## Licence

Apache-2.0. The full text is in [`LICENSE`](LICENSE) beside this file, because a packaged chart
travels without the repository around it.

There is deliberately no `NOTICE` here. The chart is Lapilli's own templates and nothing else, so it
carries no third-party work whose notices would have to travel with it. The attribution that does
exist belongs to the binaries: `NOTICE` and `THIRD-PARTY-LICENSES.md` ship inside the container image
at `/usr/local/share/doc/lapilli/` and at the top level of every CLI tarball.
