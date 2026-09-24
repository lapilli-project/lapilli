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
| `lapilli_build_info` | gauge | `version`, `image_digest` | Always 1; what is running. |
| `lapilli_captures_total` | counter | `result` = `sealed` \| `refused` \| `failed` | Captures by outcome. `refused` means the controller would not capture at all (another cluster, an unsafe or claimed incident id); `failed` means collected but never sealed. |
| `lapilli_partial_captures_total` | counter | | Sealed captures where an intended collector did not run (the bundle verifies as PARTIAL). |
| `lapilli_collector_failures_total` | counter | | Collectors that failed inside a capture (the capture goes on). |
| `lapilli_capture_seconds` | histogram | | From the start of collection to a sealed bundle. Only sealed captures are observed. |
| `lapilli_bundle_bytes` | histogram | | Sealed bundle sizes, in bytes (buckets up to the 1 GiB producer cap). |
| `lapilli_seal_attempts_total` | counter | `result` = `ok` \| `failed` | Calls to the KMS to sign a manifest, and only those, so the count can be reconciled against the cloud's audit log (`signing.mode=kms`). |
| `lapilli_seal_pack_failures_total` | counter | | Bundles the KMS signed that could not then be packed (retried; the signature was already spent). |
| `lapilli_deferred_captures_total` | counter | | Captures started under a CaptureProfile that declares `deferred` collectors — the operator intends less than a full capture and every such bundle says so (`coverage.deferred`, IEB rule 6). **Not PARTIAL**: nothing failed. Emitted from process start; a rising value alongside a flat `lapilli_partial_captures_total` is a perishable profile working as designed, and a rising value nobody expected is a profile edit worth looking at. |
| `lapilli_reconcile_errors_total` | counter | | Reconcile attempts that ended in an error, e.g. an API-server or RBAC problem, **and** failures of the watch stream itself — a dead watch means no new capture is ever noticed. The capture is retried every 10 s. |
| `lapilli_export_attempts_total` | counter | `result` = `ok` \| `failed` | Object-store export attempts. |
| `lapilli_notifications_total` | counter | `result` = `sent` \| `repeat` \| `failed` \| `suppressed` \| `dropped` \| `already-notified` | Grouped incident notifications, **one per incident, not per pod**. `suppressed` means the route's rate cap was already spent; `repeat` means the same verdict on the same workload inside its cooldown — counted and reported on the next message, not posted; `dropped` means no usable route or a full queue; `already-notified` means another dispatcher (or an earlier run) had the claim. The route name is deliberately not a label: an admin can define any number of routes. |
| `lapilli_notify_routes` | gauge | `state` = `ready` \| `error` | Notification routes that loaded, and routes that are configured but unusable (a bad host, a missing path Secret). Set once at startup, and **absent entirely when no route is configured** — so "notification is off" and "notification is broken" never read the same. |
| `lapilli_webhook_requests_total` | counter | `result` = `accepted` \| `duplicate` \| `rejected` \| `error` | Alert webhook outcomes. **Despite the name these count alerts, not requests**, for every label but `rejected`: one payload of twenty alerts moves `accepted` by twenty. `rejected` is a failed bearer token, which turns the whole request away before it is parsed, so there is no alert count to add. `duplicate` is a resend collapsing onto an existing capture (normal). **`error` is an authenticated alert the API server would not let become a capture** — a missing `create` permission looks like this, and until it was counted the caller got a 500 while every series here stayed flat. A payload is only fully accounted for by summing this with `lapilli_alerts_dropped_total` **and** `lapilli_payloads_dropped_total`; the first two count alerts and the third counts payloads, because a payload refused by the body limit was never parsed. |
| `lapilli_alerts_dropped_total` | counter | `reason` = `no-pod` \| `payload-cap` | Alerts the webhook accepted the request for but did not turn into a capture. Every label here counts **alerts**. `no-pod` is an alert with no `pod` label — a node- or cluster-level rule routed here, which used to produce a bundle with an empty pod target and no evidence in it. `payload-cap` is an alert past `webhook.maxCapturesPerPayload`, which bounds a storm at the size this project has measured (50) rather than at the size the body limit happens to admit (~990). Both are **absent until the first drop**, so a healthy controller does not report zeros that look like a broken filter. |
| `lapilli_payloads_dropped_total` | counter | `reason` = `too-large` | Whole payloads refused **before being parsed**. These count **payloads, not alerts** — the body was never read, so how many alerts were lost is not knowable, and putting that number in the series above would make `captured + dropped` quietly false. Alertmanager retries a refused payload and it is refused again, so any non-zero rate here is an unbounded loss of evidence that no other series can see. |
| `lapilli_signing_key_info` | gauge | `key_id` | Present once a KMS key is pinned; always 1. The `key_id` is what `lapilli verify --key` must match. |

The next four are **counted from the API** by a poller (every 30 s), not from reconcile
side effects, so a deleted capture leaves the gauges and a restart rebuilds them. They are
absent until the first successful poll, so a fresh pod never reports a misleading zero.

| Series | Type | Labels | Meaning |
|---|---|---|---|
| `lapilli_captures` | gauge | `phase` = `pending` \| `capturing` \| `sealing` \| `exported` \| `failed` | Captures that exist right now. Deleting a capture removes it from here. |
| `lapilli_captures_watched` | gauge | | Captures the controller **holds in its watch cache** — every capture that is not retired. **This is the number the controller's memory tracks**, at roughly 19.4 KB each (`docs/design-trigger-and-load.md` §3.3). `lapilli_captures` counts every capture that *exists*, retired or not, and that number is unbounded by design: nothing deletes a capture. |
| `lapilli_captures_retired` | gauge | | Captures taken out of the watch because nothing was left to do. They keep their CR, their status and their bundle; the controller simply no longer holds them. `kubectl label incidentcapture <name> lapilli.dev/retired-` puts one back. |
| `lapilli_captures_exported_unretired` | gauge | | `Exported` captures the controller is **still** holding. Retirement is binding only while this stays near zero. A population that grows here is the memory clock coming back — an export stuck `Pending` because its destination vanished from the config, or a notification that never settles. |
| `lapilli_captures_retired_total` | counter | | Captures retired since this process started, including the startup sweep. Beside the gauges so a sweep that **stalls** is distinguishable from one that had nothing to do. |
| `lapilli_captures_awaiting_seal` | gauge | | Captures waiting for a signature (`phase=sealing`), the same number as that label. |
| `lapilli_export_destinations` | gauge | `state` = `pending` \| `uploaded` \| `refused` \| `conflict` \| `failed` | Destinations of existing captures. `pending` is still being retried; **`refused`, `conflict` and `failed` are terminal — that evidence never reached the destination and never will.** |
| `lapilli_exports_unsettled` | gauge | | Destinations still being retried (`state=pending`). |

The same poller's `list` is also how the controller knows the API server is there at all.

| Series | Type | Labels | Meaning |
|---|---|---|---|
| `lapilli_apiserver_poll_ok` | gauge | | `1` if the poller's last `list` of `IncidentCapture` succeeded, `0` if it did not. Absent until the first poll returns — which is the first thing the poller does, measured at **6 ms** after startup, so no scrape will realistically see the gap. A consumer must still treat absent as "not known", never as `0`. |
| `lapilli_apiserver_polls_total` | counter | `result` = `ok` \| `forbidden` \| `unauthorized` \| `not-found` \| `api-error` \| `unreachable` | Polls by outcome, and the answer to the first question an operator asks. The four middle values mean **the API server answered** — so the network is fine and the problem is RBAC (`forbidden`), the ServiceAccount token (`unauthorized`), a missing CRD (`not-found`, which happens because Helm does not upgrade `crds/`), or the server itself (`api-error`: 429, 5xx). `unreachable` means **no answer came at all** — DNS, routing, TLS, a dropped egress packet, or a poll that outlasted its interval. |
| `lapilli_apiserver_last_success_timestamp_seconds` | gauge | | When a poll last succeeded, in Unix seconds. Absent until there has been one, and never cleared by a failure. **Read it on a dashboard, not in an alert:** it is the controller pod's clock, and subtracting it from Prometheus's clock pages on skew alone — and it is absent exactly when it would matter most, for a pod that started blind. |

**Why this is a series of its own.** `/healthz` deliberately does not depend on the API server
(`webhook.rs`, and docs/egress.md explains what that buys), so a controller that cannot use it
stays `1/1 Running` with no restarts while every capture stops. `lapilli_reconcile_errors_total`
does not cover the case either: nothing is reconciled when no alert can arrive, so the counter
sits flat through the outage. These three series are the only thing that reports it. They are
**not** wired into the probes on purpose — restarting the pod fixes neither a network policy nor
an RBAC change, and it throws away a capture in flight.

**What the signal does and does not prove.** It proves the poller's own `list` worked. That is
one of the chart's three RBAC bindings (`rbac.yaml`), not all of them: lose the *collector*
binding and this gauge still reads `1`. A broken collector shows up as
`lapilli_collector_failures_total` and a bundle that verifies as PARTIAL — but only once a capture
runs, so it is found during an incident rather than before one.

| `lapilli_permission_checks_total` | counter | `result` = `held` \| `denied` \| `unknown` \| `not_needed` | Permission self-checks by outcome, accumulated over every pass. `unknown` is a question that could not be asked at all, which is not a denial. `not_needed` is a collector check no listed CaptureProfile requires (a profile set that never asks for `logs` needs no `pods/log`), recorded rather than skipped so a smaller check set is visible as a choice and not as an omission. Emitted from process start, so `absent()` on it means this controller is not reporting. |
| `lapilli_permissions_denied` | gauge | | Permissions the controller needs and the API server says it does not hold, as of the last pass. `0` normally. |
| `lapilli_permissions_unknown` | gauge | | Checks whose question could not be answered (the API server or an authorization webhook did not reply). `0` normally. A denial is never counted here, including on an install whose Role was stripped so far that the CaptureProfiles cannot be listed: that install asks every check and reads every `no` as denied. |
| `lapilli_permissions_asked` | gauge | | Checks actually **sent** to the API server on the last pass. Needs are re-derived every pass from **every** CaptureProfile in the namespace (their union), so this drops when a profile edit removes the last use of a collector, and rises when one adds it. When the profiles cannot be listed nothing is narrowed and every check is sent, so this reads the full count; the controller log says the profiles were unreadable. |

| `lapilli_bundle_fs_bytes` | gauge | `state` = `used` \| `free` | The filesystem holding the bundle root, from one `statvfs` on the **always-on** poller — so it is published whether or not retention is enabled, which matters because the install that has not enabled it is the one whose disk is filling. Correct when the volume is full, and it counts staging directories and leftovers that summing `*.ieb` cannot. **Absent** if `statvfs` failed; a gauge reading `0 free` because it could not look is worse than no gauge. |
| `lapilli_retention_sweeps_total` | counter | `result` = `ok` \| `failed` | Retention sweeps. Emitted from process start, so `absent()` on it means this controller is not sweeping at all and a volume alert can guard against a stale pass. |
| `lapilli_bundles_reclaimed_total` | counter | `reason` = `max-bytes` \| `age` \| `orphan` \| `abandoned` | Local files retention removed. `abandoned` is a staging directory or pack temp file whose capture is no longer live — uncompressed, and usually the largest thing reclaimable. |
| `lapilli_reclaimed_bytes_total` | counter | | Bytes freed on the bundle volume. |
| `lapilli_reclaim_refused_total` | counter | `reason` = `not-uploaded` \| `unexported` \| `in-flight` \| `undeletable` \| `orphan-storm` | Candidates retention would **not** remove. `not-uploaded` means a destination never received it, so the local file is the only copy; `undeletable` means the volume refused the unlink (a read-only or WORM-backed PVC) and the bundle was **not** recorded as reclaimed; `orphan-storm` means too much of the population lost its CR at once, which reads as a CR wipe rather than a licence to delete. |
| `process_resident_memory_bytes` | gauge | | Resident set size of the controller process, from `/proc/self/statm`. The conventional name, not a `lapilli_`-prefixed one, so existing dashboards and alerts already understand it. **Watch this against the pod's memory limit** — the chart's default is 256 MiB and the thing that stops people using an incident recorder is it being OOM-killed during the incident. |
| `process_virtual_memory_bytes` | gauge | | Virtual memory size, from `/proc/self/statm`. |
| `process_cpu_seconds_total` | counter | | User + system CPU seconds, from `/proc/self/stat`. `rate()` over it is the controller's CPU use; the chart sets **no CPU limit** on purpose, because a limit throttles the burst at exactly the moment a capture is running. |
| `process_open_fds` | gauge | | Open file descriptors, from `/proc/self/fd`. |

**Why the volume is measured even with retention off.** Nothing deletes a sealed bundle unless
`retention` is enabled, and the chart's default PVC is 1 GiB. One alert over a 20-pod Deployment at
Alertmanager's default 4h repeat is ~120 captures a day, and how long that takes to fill 1 GiB is a
property of the bundles rather than of Lapilli — day 8 at a megabyte each, day 34 at 250 KB, years
at the 6.2 KB measured for a thin workload. **A full volume makes every capture fail, not just the
old ones**, which is why the gauge exists rather than a rule of thumb. The gauge exists so that is visible before it happens; the sweep is
what acts on it. `kubelet_volume_stats_available_bytes` says much the same thing and every cluster
already scrapes it; this one is scoped to the bundle root and needs no kubelet-metrics access.

Note what it is **not**: `retention.maxBytes` bounds the bytes Lapilli's own files occupy, which is a
different quantity from this gauge. A PVC is usually backed by a filesystem much larger than the
request, so `statvfs` used-bytes would trip a ceiling derived from `persistence.size` immediately.

**Why a permission check as well.** `lapilli_apiserver_poll_ok` proves one binding of the three the
chart creates. The **collector** binding is generated through conditional branches on
`watchNamespaces` and `diffs.configMaps`, so it is the one most likely to be wrong, and losing it
leaves every capture empty while that gauge still reads `1`. It does surface as
`lapilli_collector_failures_total` and a PARTIAL bundle — but only once a capture runs, which means
during an incident, with the evidence already damaged. The self-check asks at startup and every
10 minutes, with `SelfSubjectAccessReview`, about **every verb the code actually issues** — `get`,
`list` and `watch` are distinct RBAC verbs, so one canary verb per resource would pass on a Role
that grants only that one.

`SelfSubjectAccessReview` needs no permission of its own: `system:basic-user` is bound to
`system:authenticated`, so a ServiceAccount with no RoleBinding at all can still ask and be told
no. A missing permission is reported, never fatal — a controller that cannot read pod logs still
records everything else, while one that refuses to start records nothing and cannot say why.

**Counts here, detail in the log.** These are deliberately *not* a gauge per check. This endpoint
has no authentication, and a per-check map is a live capability inventory of the controller's
ServiceAccount: "the flight recorder cannot read pod logs right now" tells an attacker exactly when
their actions will not be recorded, and the mere *presence* of a conditional check would disclose
`signing.mode=static` or that export credentials sit in a Secret. Which permission is missing, in
which namespace, with the API server's own `reason` — it names the missing Role — goes to the
controller log. The cost is real and accepted: an operator reading only metrics learns that
something is missing, not what.

**What it does not cover.** The namespace a capture reads comes from the alert's `namespace` label,
not from `watchNamespaces`. On a namespaced install an alert naming a namespace outside that list
still creates a capture, and that capture comes out empty with every check reading green — so
`watchNamespaces` must cover every namespace Alertmanager can name. The check also proves nothing
about the object store, the KMS endpoint or a notification host.

**How fast it notices.** A poll that *errors* flips the gauge on the next poll, so within 30 s: a
revoked RoleBinding was measured at 30 s on a live cluster. A poll that **hangs** — which is what a
dropped egress packet produces, as opposed to a refusal — is bounded by the poll interval, so worst
case is one interval of sleeping plus one of hanging, about 60 s. Without that bound the only
ceiling is the client's 295 s read timeout; the first version of this had no bound and a real
blackhole measured 70 s to flip, with failures then accruing every ~60 s instead of every 30 s.

Deliberately absent: incident ids, pod and namespace names, destination names and cluster
ids as labels. Cardinality stays flat however many captures there are, and the metrics
endpoint says nothing about what was captured.

Counters reset when the pod restarts, as process counters do (`increase()` handles that).
The state-derived gauges are rebuilt by the poller within 30 s of a restart, and are not
emitted at all until its first success.

## Alerts worth having

Examples to copy, not a shipped `PrometheusRule` — the chart deliberately generates none, for the
same reason it generates no NetworkPolicy (docs/egress.md): routing labels, `for:` durations and
namespace selectors belong to whoever owns the alerting stack. Only the API-server rules carry
`severity`, because there the two-stage escalation is the point; add your own to the rest.

```yaml
# Evidence is being refused or lost.
- alert: LapilliCapturesFailing
  expr: increase(lapilli_captures_total{result=~"failed|refused"}[15m]) > 0
  annotations: { summary: "Lapilli refused or failed a capture: check the capture's status.message" }

# Signing is stuck: captures are collected but can't be sealed.
- alert: LapilliCapturesAwaitingSeal
  expr: lapilli_captures_awaiting_seal > 0
  for: 30m
  annotations: { summary: "Lapilli captures have waited 30m for a KMS signature (see docs/kms.md)" }

# Evidence that will never reach its destination. This one does NOT resolve itself when the
# retries run out, which a pending-only alert would.
- alert: LapilliExportsLost
  expr: lapilli_export_destinations{state=~"refused|conflict|failed"} > 0
  annotations: { summary: "A bundle never reached an export destination ({{ $labels.state }})" }

# A conflict means the bucket already holds different bytes under this key: an
# evidence-integrity event, not a transient failure.
- alert: LapilliExportConflict
  expr: increase(lapilli_export_attempts_total{result="failed"}[1h]) > 0
    and on() lapilli_export_destinations{state="conflict"} > 0

# Still retrying long after the incident. Same frozen-gauge guard as above.
- alert: LapilliExportsUnsettled
  expr: lapilli_exports_unsettled > 0
    unless on(instance) lapilli_apiserver_poll_ok == 0
  for: 1h

# A route was configured but could not be loaded at all: nothing will ever be posted to it.
# This fires without waiting for an incident, which the failure counter below cannot.
- alert: LapilliNotifyRouteDisabled
  expr: lapilli_notify_routes{state="error"} > 0
  annotations: { summary: "A Lapilli notification route is disabled; see the controller log for the reason" }

# A route is misconfigured or its endpoint is down: evidence is being captured and nobody is
# being told. `dropped` almost always means the route name on a profile does not exist.
- alert: LapilliNotificationsFailing
  expr: increase(lapilli_notifications_total{result=~"failed|dropped"}[30m]) > 0
  annotations: { summary: "Incident summaries are not reaching their channel ({{ $labels.result }})" }

# The rate cap is doing its job, which also means a human is not seeing every incident.
- alert: LapilliNotificationsSuppressed
  expr: increase(lapilli_notifications_total{result="suppressed"}[1h]) > 0
  annotations: { summary: "Incident summaries hit the per-route rate cap; raise maxPerWindow or split the route" }

# No Lapilli controller is reporting at all. This has to be its own rule: the two below select on
# a series, and a rule on a series that has DISAPPEARED never fires — it does not even go
# pending. The chart runs one replica with `strategy: Recreate`, so a rollout that never
# completes leaves no pod reporting, and the same is true of a Pending pod, a CrashLoop, a scale
# to 0, or a policy that blocks port 8081. `lapilli_apiserver_polls_total` is the right sentinel
# because it is emitted from process start, unlike the two gauges. With more than one install,
# add a selector (e.g. `{namespace="lapilli-system"}`) — `absent()` is false while any one reports.
- alert: LapilliNotReporting
  # `absent(sum(…))` rather than `absent(…)`: the bare form inherits the selector, so the page
  # would arrive labelled `result="ok"` while saying the controller is gone.
  expr: absent(sum(lapilli_apiserver_polls_total{result="ok"}))
  for: 10m
  labels: { severity: critical }
  annotations: { summary: "No Lapilli controller is reporting: the cluster has no evidence recorder" }

# The controller cannot use the API server. Nothing else catches this: the pod stays Ready
# because /healthz is decoupled on purpose, and lapilli_reconcile_errors_total stays flat because
# nothing is being reconciled. `for: 3m` rides out one lost poll and the ~60 s a hung poll can take
# to be noticed. It does NOT need to ride out a rollout: the first poll happens at startup, within
# milliseconds, so the gauge is never absent for a whole scrape.
- alert: LapilliApiServerUnusable
  expr: lapilli_apiserver_poll_ok == 0
  for: 3m
  labels: { severity: warning }
  annotations:
    summary: "Lapilli cannot use the API server: no capture will run (the pod stays Ready on purpose)"
    description: >-
      Break it down with lapilli_apiserver_polls_total by result: forbidden or unauthorized is RBAC
      or the ServiceAccount token, not-found is a missing CRD, unreachable is the network
      (docs/egress.md), api-error is the server itself.

# The second stage, and not a duplicate: this one uses ONLY Prometheus's clock, so it also fires
# for a pod that was blind from the moment it started — which never emits a last-success
# timestamp for `time() - …` to subtract — and for a poller that stopped without failing.
- alert: LapilliApiServerBlindTooLong
  # `sum without(result)` drops the same misleading label while keeping the instance ones, so one
  # blind pod among healthy ones is still visible.
  expr: sum without(result) (increase(lapilli_apiserver_polls_total{result="ok"}[5m])) == 0
  for: 15m
  labels: { severity: critical }
  annotations: { summary: "Lapilli has not reached the API server for 15m; captures have been stopping that long" }

# Reconcile attempts that error out. Narrower than it looks: it needs a capture to exist, so it
# says nothing about an outage during a quiet period — that is what the two alerts above are for.
- alert: LapilliReconcileErrors
  expr: increase(lapilli_reconcile_errors_total[15m]) > 5

# Captures that never got anywhere. The `unless` is load-bearing: the state gauges deliberately
# hold their last value when the API server cannot be polled, so without it an API-server outage
# also pages "captures are stuck" — which is not true, the *picture* is stale.
- alert: LapilliCapturesStuck
  expr: lapilli_captures{phase=~"pending|capturing"} > 0
    unless on(instance) lapilli_apiserver_poll_ok == 0
  for: 15m

# Alertmanager's token copy is stale (or someone is probing).
- alert: LapilliWebhookRejecting
  expr: increase(lapilli_webhook_requests_total{result="rejected"}[10m]) > 0

# The bundle volume is filling. When it is full EVERY capture fails, so this is not about losing old
# evidence. The `unless` guards against a stale sweep the way the capture gauges are guarded: a sweep
# that has stopped leaves the byte gauges looking healthy for a while.
- alert: LapilliBundleVolumeFilling
  expr: lapilli_bundle_fs_bytes{state="free"} / ignoring(state) sum without(state) (lapilli_bundle_fs_bytes) < 0.15
  for: 30m
  labels: { severity: warning }
  annotations:
    summary: "Lapilli's bundle volume is over 85% full; captures fail when it is full"
    description: "Enable retention (docs/design-retention.md) or raise persistence.size. Abandoned staging directories are reclaimed regardless of the byte and age bounds."

# Retention is not sweeping at all, though something asked it to. Emitted from process start, so this
# fires on a controller that never got a sweep away rather than staying silent on an absent series.
- alert: LapilliRetentionNotSweeping
  # `sum` on both halves: the bare forms inherit `result="ok"`, so the page would arrive labelled
  # "ok" while saying the sweep is not running. Round 14 found the same thing twice.
  expr: absent(sum(lapilli_retention_sweeps_total{result="ok"})) or sum without(result) (increase(lapilli_retention_sweeps_total{result="ok"}[2h])) == 0
  for: 1h
  labels: { severity: warning }
  annotations: { summary: "Lapilli's retention sweep has not completed in 2h" }

# The sweep is refusing to reclaim. `not-uploaded` is the important one: those bundles reached no
# destination, so the local copy is the only copy and retention will never remove it — which means
# the volume keeps filling and the fix is the export path, not the retention window.
- alert: LapilliRetentionCannotKeepUp
  expr: increase(lapilli_reclaim_refused_total{reason=~"not-uploaded|undeletable|orphan-storm"}[1h]) > 0
  labels: { severity: warning }
  annotations:
    summary: "Lapilli retention refused to reclaim ({{ $labels.reason }})"
    description: "not-uploaded: the only copy is local, so fix export. undeletable: the volume refused the unlink. orphan-storm: many bundles lost their IncidentCapture at once, which looks like a CR wipe."

# A permission this install needs is not held. Fires before an incident rather than during one,
# which is the whole point: the alternative is finding out from a bundle that came out empty.
# $labels.check names it; the controller's log says what stops working without it.
- alert: LapilliMissingPermission
  expr: lapilli_permissions_denied > 0
  for: 5m
  labels: { severity: critical }
  annotations:
    summary: "Lapilli is missing {{ $value }} permission(s) it needs"
    description: >-
      Captures will be incomplete. The controller log names each one, the namespace, and the API
      server's reason — it is not a label here because this endpoint is unauthenticated.

# The checks could not be answered at all — an authorization webhook that is down, or an API server
# that stopped replying. Distinct from a denial: nothing is known, so nothing should be concluded,
# and a page about RBAC would send the operator to the wrong place. Both gauges are always emitted
# once a pass has run, so neither rule can go silent by the series vanishing.
- alert: LapilliPermissionChecksUnanswerable
  expr: lapilli_permissions_unknown > 0
  for: 30m
  labels: { severity: warning }
  annotations: { summary: "Lapilli cannot find out whether it holds the permissions it needs" }

# An alert arrived, authenticated, and could not be turned into a capture — almost always a missing
# `create` permission on incidentcaptures. Every other series stays flat while this happens, so
# without this rule the evidence recorder is silently accepting nothing.
- alert: LapilliWebhookErroring
  expr: increase(lapilli_webhook_requests_total{result="error"}[10m]) > 0
  labels: { severity: critical }
  annotations: { summary: "Lapilli could not record an alert it received: check RBAC on incidentcaptures" }

# A WHOLE payload was refused before it was parsed, so an unknown number of alerts were lost at
# once and no other series moved. Alertmanager retries the same oversized payload and it is
# refused again, so this does not drain — it is a standing hole in the evidence for as long as the
# storm lasts. This is the rule that would have made the 256 KiB body limit visible; without it
# the recorder was blind at exactly the scale that matters and said nothing.
- alert: LapilliPayloadRefused
  expr: increase(lapilli_payloads_dropped_total{reason="too-large"}[10m]) > 0
  labels: { severity: critical }
  annotations:
    summary: "Lapilli refused a whole alert payload as too large: an unknown number of alerts were not recorded"

# The per-payload cap turned alerts away. This is working as configured — the cap exists so a storm
# cannot take the controller with it — but it means evidence you might want was not collected, and
# that is a decision an operator should see rather than discover afterwards. Raise
# `webhook.maxCapturesPerPayload` only after measuring; the default is the largest storm this
# project has measured, not a computed ceiling.
- alert: LapilliPayloadCapped
  expr: increase(lapilli_alerts_dropped_total{reason="payload-cap"}[30m]) > 0
  labels: { severity: warning }
  annotations:
    summary: "A storm exceeded Lapilli's per-payload capture cap; the excess alerts were not captured"

# A rule is routed to Lapilli that carries no `pod` label, so it can never produce a capture. The
# usual cause is a node- or cluster-level alert (KubeNodeNotReady and friends) matched by a route
# that was meant for pod-level rules. Lapilli records a POD's incident window and has nothing to
# record for these; before they were dropped they produced signed bundles with nothing in them.
- alert: LapilliAlertsWithoutPod
  expr: increase(lapilli_alerts_dropped_total{reason="no-pod"}[30m]) > 0
  labels: { severity: warning }
  annotations:
    summary: "Alerts with no pod label are being routed to Lapilli and cannot be captured: check the Alertmanager route's matchers"

# The controller's watch cache is filling. Captures are retired from the watch once nothing is left
# to do (docs/design-capture-retirement.md), so this should sit near the number of incidents actually
# in flight. It grows only when captures stop retiring, and at ~19.4 KB each the 256Mi default is
# exhausted around 13,000 — or around 7,400 before the controller can still absorb the ~110 MiB a
# 20-alert storm needs. 5,000 leaves roughly a month of headroom at one rule's capture rate.
#
# Note this is NOT `sum(lapilli_captures)`: that counts every capture that exists, which grows
# forever by design because nothing deletes a capture. An alert on that number fires on a healthy
# old install and gets silenced.
- alert: LapilliWatchCacheFilling
  expr: lapilli_captures_watched > 5000
  for: 1h
  labels: { severity: warning }
  annotations:
    summary: "Lapilli is holding {{ $value }} captures in its watch cache; check lapilli_captures_exported_unretired for captures that are not retiring"

# Exported captures that are not retiring. This is the failure of the mechanism above rather than of
# the controller: the two known causes are an export left `Pending` because its destination was
# removed from the configuration, and a notification that never settles. Neither loses evidence, and
# both put the memory clock back.
- alert: LapilliCapturesNotRetiring
  expr: lapilli_captures_exported_unretired > 500
  for: 2h
  labels: { severity: warning }
  annotations:
    summary: "{{ $value }} exported captures are still held by Lapilli; retirement is not keeping up and the controller's memory will grow"

# The signing key is not the one the auditors pinned. `changes()` cannot see this: rotation
# ends one series and starts another, each constant at 1. Compare the label instead.
- alert: LapilliSigningKeyUnexpected
  expr: lapilli_signing_key_info{key_id!="<the key_id auditors hold>"} == 1
```

`lapilli_captures_total{result="sealed"}` staying flat is **not** an alert: no incidents may
simply mean nothing fired. Alert on the failure paths, and on Alertmanager's own delivery
to the webhook.
