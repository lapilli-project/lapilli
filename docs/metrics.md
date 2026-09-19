# Controller metrics

The controller serves Prometheus metrics on **`/metrics` of the health port (8081)**, in the
text exposition format. No authentication: the series carry counts and the signing key id,
never incident content.

Scraping, with the chart:

- **Annotations** (default): `prometheus.io/scrape`, `port: 8081`, `path: /metrics` on the
  pod. Turn off with `telemetry.scrapeAnnotations=false`.
- **Prometheus Operator:** `telemetry.serviceMonitor.enabled=true` adds a Service and a
  `ServiceMonitor` (that CRD must exist), with `telemetry.serviceMonitor.interval` and
  `.labels`.

## Series

Names, labels and types are stable (docs/COMPATIBILITY.md §2). Series may be added; none is
renamed, retyped or given a new label within a major.

| Series | Type | Labels | Meaning |
|---|---|---|---|
| `kairn_build_info` | gauge | `version`, `image_digest` | Always 1; what is running. |
| `kairn_captures_total` | counter | `result` = `sealed` \| `refused` \| `failed` | Captures by outcome. `refused` means the controller would not capture at all (another cluster, an unsafe or claimed incident id); `failed` means collected but never sealed. |
| `kairn_partial_captures_total` | counter | | Sealed captures where an intended collector did not run (the bundle verifies as PARTIAL). |
| `kairn_collector_failures_total` | counter | | Collectors that failed inside a capture (the capture goes on). |
| `kairn_capture_seconds` | histogram | | From the start of collection to a sealed bundle. Only sealed captures are observed. |
| `kairn_bundle_bytes` | histogram | | Sealed bundle sizes, in bytes (buckets up to the 1 GiB producer cap). |
| `kairn_seal_attempts_total` | counter | `result` = `ok` \| `failed` | Calls to the KMS to sign a manifest, and only those, so the count can be reconciled against the cloud's audit log (`signing.mode=kms`). |
| `kairn_seal_pack_failures_total` | counter | | Bundles the KMS signed that could not then be packed (retried; the signature was already spent). |
| `kairn_reconcile_errors_total` | counter | | Reconcile attempts that ended in an error, e.g. an API-server or RBAC problem. The capture is retried every 10 s. |
| `kairn_export_attempts_total` | counter | `result` = `ok` \| `failed` | Object-store export attempts. |
| `kairn_webhook_requests_total` | counter | `result` = `accepted` \| `duplicate` \| `rejected` | Alert webhook outcomes. `duplicate` is a resend collapsing onto an existing capture (normal); `rejected` is a failed bearer token. |
| `kairn_signing_key_info` | gauge | `key_id` | Present once a KMS key is pinned; always 1. The `key_id` is what `kairn verify --key` must match. |

The next three are **counted from the API** by a poller (every 30 s), not from reconcile
side effects, so a deleted capture leaves the gauges and a restart rebuilds them. They are
absent until the first successful poll, so a fresh pod never reports a misleading zero.

| Series | Type | Labels | Meaning |
|---|---|---|---|
| `kairn_captures` | gauge | `phase` = `pending` \| `capturing` \| `sealing` \| `exported` \| `failed` | Captures that exist right now. Deleting a capture removes it from here. |
| `kairn_captures_awaiting_seal` | gauge | | Captures waiting for a signature (`phase=sealing`), the same number as that label. |
| `kairn_export_destinations` | gauge | `state` = `pending` \| `uploaded` \| `refused` \| `conflict` \| `failed` | Destinations of existing captures. `pending` is still being retried; **`refused`, `conflict` and `failed` are terminal — that evidence never reached the destination and never will.** |
| `kairn_exports_unsettled` | gauge | | Destinations still being retried (`state=pending`). |

Deliberately absent: incident ids, pod and namespace names, destination names and cluster
ids as labels. Cardinality stays flat however many captures there are, and the metrics
endpoint says nothing about what was captured.

Counters reset when the pod restarts, as process counters do (`increase()` handles that).
The state-derived gauges are rebuilt by the poller within 30 s of a restart, and are not
emitted at all until its first success.

## Alerts worth having

```yaml
# Evidence is being refused or lost.
- alert: KairnCapturesFailing
  expr: increase(kairn_captures_total{result=~"failed|refused"}[15m]) > 0
  annotations: { summary: "Kairn refused or failed a capture: check the capture's status.message" }

# Signing is stuck: captures are collected but can't be sealed.
- alert: KairnCapturesAwaitingSeal
  expr: kairn_captures_awaiting_seal > 0
  for: 30m
  annotations: { summary: "Kairn captures have waited 30m for a KMS signature (see docs/kms.md)" }

# Evidence that will never reach its destination. This one does NOT resolve itself when the
# retries run out, which a pending-only alert would.
- alert: KairnExportsLost
  expr: kairn_export_destinations{state=~"refused|conflict|failed"} > 0
  annotations: { summary: "A bundle never reached an export destination ({{ $labels.state }})" }

# A conflict means the bucket already holds different bytes under this key: an
# evidence-integrity event, not a transient failure.
- alert: KairnExportConflict
  expr: increase(kairn_export_attempts_total{result="failed"}[1h]) > 0
    and on() kairn_export_destinations{state="conflict"} > 0

# Still retrying long after the incident.
- alert: KairnExportsUnsettled
  expr: kairn_exports_unsettled > 0
  for: 1h

# The controller can't talk to the API server (RBAC, outage): captures stop silently.
- alert: KairnReconcileErrors
  expr: increase(kairn_reconcile_errors_total[15m]) > 5

# Captures that never got anywhere.
- alert: KairnCapturesStuck
  expr: kairn_captures{phase=~"pending|capturing"} > 0
  for: 15m

# Alertmanager's token copy is stale (or someone is probing).
- alert: KairnWebhookRejecting
  expr: increase(kairn_webhook_requests_total{result="rejected"}[10m]) > 0

# The signing key is not the one the auditors pinned. `changes()` cannot see this: rotation
# ends one series and starts another, each constant at 1. Compare the label instead.
- alert: KairnSigningKeyUnexpected
  expr: kairn_signing_key_info{key_id!="<the key_id auditors hold>"} == 1
```

`kairn_captures_total{result="sealed"}` staying flat is **not** an alert: no incidents may
simply mean nothing fired. Alert on the failure paths, and on Alertmanager's own delivery
to the webhook.
