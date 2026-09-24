# Changelog

All notable changes to Lapilli are listed here. Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Lapilli follows SemVer; before 1.0 a minor release may change alpha surfaces (CRDs, chart
values, CLI commands other than `lapilli verify`). Compatibility commitments are in
[`docs/COMPATIBILITY.md`](docs/COMPATIBILITY.md). Changes that need action on upgrade are
listed under **Migration**.

## [Unreleased]

### Added
- `lapilli postmortem <bundle|dir>` — the draft a human then writes. It **transcribes**: every
  line is a value that exists in the bundle with the file it came from, and Impact, Root cause,
  Contributing factors and Action items are emitted as **empty headings**, because a blank
  heading is an honest prompt and a filled one would be a guess (`DESIGN.md` §2). Markdown on
  stdout; there is no `--format` and no `--template`, because a rendering people can change is a
  rendering whose provenance claims stop being true.

  It verifies first, and the verdict decides **how** it renders, not whether. OK and PARTIAL
  render (PARTIAL names the collectors that did not run, so a thin section reads as an incomplete
  bundle rather than a quiet incident). FAILED **still renders**, behind a banner, and exits 1 —
  a failed bundle is exactly when someone needs to see what it *claims*, and printing nothing
  sends them to `cat` the files with no verdict attached to anything. CANNOT_EVALUATE refuses
  with exit 3: that is the opposite of PARTIAL, not a milder FAILED, and there is no verified
  tree to render from. The exit code never contradicts `lapilli verify` on the same bundle, which
  a test pins across all 39 released fixtures.

  The banner names **which** failure it was, because they are not the same accusation: files that
  do not match the hash tree, a signature that does not check out, an archive that is not
  well-formed, or an identity that could not be re-derived — the last of which means "this may be
  about the wrong place", not "someone tampered with it".

  The header carries the bundle's SHA-256 and the `lapilli verify` line that reproduces the
  verdict, so a reader a week later can re-derive every fact from bytes whose integrity they check
  themselves. The container's last log line is **off by default** behind `--include-log-line`: the
  output is a document pasted into a wiki, which is a broader and more permanent audience than the
  Slack channel the notification defaults were written for. `--key` stays optional, and an
  unpinned signature says so in the header.

  Local bundle or directory only — no `s3://`. See `docs/design-postmortem.md` and
  `docs/design-review-round18.md`.
- Object-store export (S3, S3-compatible, GCS): after sealing, each bundle is copied to
  admin-defined destinations (`export.destinations` in the chart; profiles reference them by
  name). Conditional create with a service-verified SHA-256, never overwriting; an existing
  object with other bytes is a `conflict`. Retries with backoff; per-destination status
  (`status.exports`, `EXPORT` column) and Events. Only a verified bundle of the capture is
  ever uploaded. `lapilli demo` captures stay local. See `docs/design-export.md`.
- Chart: `serviceAccount.annotations` (IRSA, GKE Workload Identity).
- Webhook authentication: `Authorization: Bearer <token>`, on by default. The chart generates
  the token (kept across upgrades) or uses `webhook.auth.existingSecret`; optional
  `webhook.networkPolicy`. `lapilli demo` fires its alert from inside the controller pod.
  Authentication runs before the body is read; bodies are capped at 256 KiB and requests
  at 16 concurrent; rejections are counted and logged at most every 10 s. `/healthz` moved
  to its own port (8081) so a NetworkPolicy on the webhook port never blocks probes.

- `lapilli verify s3://… | gs://… | https://…`: verifies an object straight from a bucket or
  a presigned URL, streamed without being stored, with the same verdicts and exit codes as
  a local file. S3 objects also get a version history check (a key written more than once,
  or with a delete marker, is FAILED; `--current-only` skips it), and the key's
  `<cluster>/<incident>.ieb` is checked against the bundle (`--any-key` skips it). New
  `--expect-sha256` (also for local files) and `--version-id`. Read failures are exit 3.
  See `docs/design-remote-verify.md`. `--no-default-features` builds the CLI without any
  network code; the controller image ships that build.
- `status.exports.<name>.sha256` and `.versionId`: the uploaded object's hash and store
  version, for `lapilli verify --expect-sha256 / --version-id`.
- Export refuses to run under a cluster id that isn't `[A-Za-z0-9._-]` (at most 100): the
  id is a key segment. The chart fails the render in that case.

- `lapilli verify --output json`: one `lapilli.dev/verify-result/v1` document on stdout for
  every outcome (exit 0–3), with stable problem codes (`integrity`, `context`, `signature`,
  `custody`, …), the input's size and SHA-256, the bundle's own cluster and incident, and
  the bucket version history. Stable from v0.1.0: `spec/VERIFY-RESULT.md`. The fixtures now
  pin each case's problem codes too.
- `lapilli-bundle`: `VerifyReport.problems` is now `Vec<Problem>` (`code` + `message`); new
  `cluster_id` and `incident_id` fields; new `verify_reader`.
- Verdict precedence is now explicit: FAILED > CANNOT_EVALUATE > PARTIAL > OK. So a
  `--expect-sha256` mismatch is FAILED (exit 1) even on a bundle that can't be evaluated
  (previously 3), and a bucket's unreadable version history no longer hides a FAILED bundle.
- An unusable `--key` (not a public key) is now "cannot evaluate" (exit 3, `unreadable`)
  instead of a signature failure (exit 1): it is the operator's input, not evidence of
  tampering.
- `--expect-sha256` on a directory is a usage error (exit 64; previously 3).
- A local `.ieb` is read once: the SHA-256 reported and checked is of exactly the bytes
  verified.
- `lapilli verify s3://…` on a deleted key (delete markers, no current object) is FAILED
  (`custody`) instead of "no such object".
- The manifest's `schema_version` is dispatched before the duplicate-member check
  (IEB-SPEC §9).

- **KMS signing (AWS KMS, GCP Cloud KMS)**: `signing.mode=kms` with `signing.kms.key` (an
  AWS key ARN or a GCP key version). The key never enters the cluster; bundles are
  unchanged (`ieb/v1`, verified with `lapilli verify --key`).
  - Captures collect once, then wait in `Sealing` while KMS is unavailable, with backoff,
    across restarts, and are never written unsigned. `lapilli.dev/retry-seal` re-drives a
    failed seal.
  - `status.seal` records the key id, manifest digest and request id, to match cloud audit
    logs.
  - `lapilli key fetch --kms <key>` writes the public key from the KMS itself.
  - No cloud SDKs: SigV4 and token providers from `object_store`.
  - See `docs/kms.md` and `docs/design-kms.md`.
- Chart: `extraEnv`; `telemetry.scrapeAnnotations` (on by default) and
  `telemetry.serviceMonitor` for the controller's own metrics.
- **Controller metrics** on `/metrics` of the health port (8081): captures by outcome and
  by phase, partial captures, collector failures, capture duration and bundle size
  histograms, KMS seal attempts (the KMS calls themselves) and pack failures, reconcile
  errors, export attempts and destinations by state (terminal states included, so lost
  evidence stays visible), webhook outcomes, and the pinned signing key id. Gauges are
  counted from the API every 30 s, so deletions and restarts are reflected. No incident
  ids or pod names as labels. Names and labels are stable; see `docs/metrics.md`.
- Static-key and KMS signatures are stored in canonical low-S form. RustCrypto's p256 does
  not normalize by itself; verifiers accept both forms.
- **Incident notification**: when a capture is sealed and its exports have settled, the
  controller posts a one-screen summary where the team already looks — Slack Block Kit or a
  generic `lapilli.dev/notification/v1` JSON body. **One message per incident, not per pod**:
  captures are grouped by `(route, rule, namespace, owner)` over a ~30 s coalescing window
  (hard cap 2 minutes), so a bad rollout across 50 replicas is one message naming 50 captures.
  The message carries the termination reason and exit code, the memory peak against the limit,
  whether the crashed container's last log survived, what the last rollout changed and when
  and by whom, how complete the evidence is, and the command that retrieves the bundle.
  Admin-defined routes (`notify.routes` in the chart, host in a ConfigMap and only the secret
  path segment in a Secret); a `CaptureProfile` may only *name* one (`spec.notify.route`,
  `profile.notifyRoute` in the chart). Per-route `maxPerWindow` (default 10 per 5 minutes)
  with an `N more notifications suppressed` notice on the next message that gets through.
  Sending never delays or fails a capture: it runs in its own task, and each group is sent
  off that task too, so one stuck route cannot hold up another route's messages.
  **Only captures this process has seen from the start are announced**: configuring a route rolls
  the controller, whose watcher relist re-reconciles every capture the PVC holds, so without this
  the day an admin enables notification every incident ever recorded would land in the channel at
  once. The trade is that a capture created before a restart is not announced even if it seals
  afterwards; a wall-clock window instead would have muted a capture waiting out a KMS outage.
  **Repeats are counted, not posted**: once a message has been delivered, further firings for
  the same rollout stay quiet for 30 minutes and the repeats ride on the next message as
  `×N more since 14:05` — a crashlooping pod is one incident, not one every five minutes. Only a
  **new rollout** is news; a changed termination reason is folded in and reported as
  `also seen: Error, OOMKilled`, because a container that dies `OOMKilled` then `Error` is one
  incident and treating each as news posts every firing. A send that failed, or that the rate cap
  turned away, does not arm the cooldown: it would mute the incident for half an hour having
  never announced it. The first group the
  rate cap turns away posts an immediate notice naming the `kubectl get incidentcapture` that
  lists the rest, so a capped channel never goes quiet without saying so.
  See `docs/design-notify.md`.
- `<incident>.summary.json` next to each bundle: the summary the notification renders from,
  written before the staging directory is removed so a restart can still report.
- `lapilli_notifications_total{result}` (`sent`, `repeat`, `failed`, `suppressed`, `dropped`,
  `already-notified`) and `lapilli_notify_routes{state}` — a gauge of routes that loaded versus
  routes that are configured but unusable, set at startup and absent when no route is
  configured, so "notification is off" and "notification is broken" never read the same.
  `status.notification = {state, at, reason, route}` (reporting only) with a `NOTIFY` column on
  `kubectl get incidentcapture`; `reason` is one of a fixed set of codes, never transport text.
- `lapilli cat-bundle` is now a documented command (it was hidden): it is how an un-exported
  bundle is pulled out of the distroless controller image, and the command a notification
  prints.
- The signing key's public half is archived beside the bundles it signed:
  `<bundle path>/keys/<key_id>.pub` locally, and `<prefix>/<cluster>/keys/<key_id>.pub` at each
  export destination (once per key per destination). A bundle signed with a KMS key that is later
  disabled was otherwise unverifiable — the public half is no longer fetchable, and the rotation
  runbook's "keep a copy first" step depended on somebody remembering.
  **The copy is an archive, not a trust anchor**: `lapilli verify --key` still takes the key the
  auditor chose, nothing reads the archive implicitly, and the docs say plainly that whoever can
  write the bucket could replace a bundle and a key together. Its value is that once you know the
  key id you expect — from the manifest, `status.seal.keyId` or the startup log, none of which live
  in the bucket — the bundle can still be verified. Each file is named by its own key id, so the
  name and the content check each other, and `lapilli verify` refuses a key file whose name and bytes
  disagree.
- `lapilli_apiserver_poll_ok`, `lapilli_apiserver_polls_total{result}` and
  `lapilli_apiserver_last_success_timestamp_seconds`: the one failure that halts the product used to
  be the one nothing reported. `/healthz` is deliberately decoupled from the API server, so a
  controller that cannot use it stays `1/1 Running` with no restarts while every capture stops —
  and `lapilli_reconcile_errors_total` says nothing, because nothing is being reconciled when no
  alert can arrive. The signal comes from the state poller's own `list`, the work the controller
  already has to do, so there is no synthetic probe to disagree with reality; the poll is bounded
  by its own interval, because a poll that **hangs** is what a dropped egress packet looks like
  from inside the pod, and without the bound the only ceiling is the client's 295 s read timeout.
  `result` separates the two mistakes an operator has to tell apart: `forbidden`, `unauthorized`,
  `not-found` and `api-error` all mean the API server answered, so the network is fine, while
  `unreachable` means no answer came. The gauge is **absent** until the first poll returns rather
  than guessing, and a failed poll never clears the last-success timestamp. The probes are
  deliberately unchanged: restarting the pod fixes neither a network policy nor an RBAC change,
  and it would cut short a capture in flight. `docs/metrics.md` has the alerts, including an
  `absent()` sentinel — a rule that selects on a series cannot fire once that series is gone, and
  the chart runs a single replica.
- **Permission self-check.** At startup and every 10 minutes the controller asks the API server, with
  `SelfSubjectAccessReview`, whether it holds the permissions it needs, and publishes
  `lapilli_permission_checks_total{result}` plus `lapilli_permissions_denied` and
  `lapilli_permissions_unknown`. `lapilli_apiserver_poll_ok` proves one of the chart's three RBAC
  bindings; the **collector** binding is generated through conditional branches on `watchNamespaces`
  and `diffs.configMaps`, so it is the one most likely to be wrong, and losing it leaves every
  capture empty while that gauge still reads `1`. It did surface — as
  `lapilli_collector_failures_total` and a PARTIAL bundle — but only once a capture ran, which means
  during an incident with the evidence already damaged.
  - It asks about **every verb the code issues**, not one canary per resource: `get`, `list` and
    `watch` are distinct RBAC verbs, so a Role granting only the canary would have passed. `watch` on
    IncidentCapture matters most — the controller is a ListWatch watcher, and without it no alert is
    ever noticed while every other check reads green.
  - A question that could not be answered is **`unknown`, never `denied`**, and each pass is bounded
    (per question and as a whole) so a silent API server cannot freeze the answer or stall the loop.
  - The detail — which permission, which namespace, and the API server's own `reason`, which names
    the missing Role — goes to the **log**, not to `/metrics`. That endpoint is unauthenticated, and
    a gauge per check is a live capability inventory: "the flight recorder cannot read pod logs right
    now" tells an attacker exactly when their actions will not be recorded, and the presence of a
    conditional check would disclose `signing.mode=static` or that export credentials sit in a
    Secret. The cost is accepted and documented: metrics say how many, the log says which.
  - Nothing here refuses to start. A controller that cannot read pod logs still records everything
    else; one that will not start records nothing and cannot say why.
  - Known limit, stated in `docs/metrics.md`: the namespace a capture reads comes from the alert's
    `namespace` label, not from `watchNamespaces`, so a namespaced install must scope
    `watchNamespaces` to every namespace Alertmanager can name.
  - New chart plumbing: `LAPILLI_WATCH_NAMESPACES`, so the questions match the RBAC the chart
    generated — cluster-wide, or per namespace. A malformed entry is dropped with an error rather
    than reported as a permission the cluster could never grant.
- **The shutdown budgets are now enforced rather than asserted.** Two numbers in the notification
  flush were arithmetic nobody checked.
  - One flush attempt's worst case is name resolution *plus* the request, not the request alone —
    `lapilli_net` resolves and vets the address under its own `RESOLVE_TIMEOUT` first. At 5 s + 5 s
    that was exactly the 10 s drain window, so a slow resolver and a slow endpoint raced the
    timeout; a group is claimed before it is posted, so losing that race marked captures notified
    that were never announced. The flush now uses a 3 s request budget and `main.rs` carries a
    **compile-time assertion** that the drain window covers `RESOLVE_TIMEOUT + POST_BUDGET_DRAINING`
    with slack. The old value no longer fails a test; it fails the build.
  - The chart did not set `terminationGracePeriodSeconds`, relying on Kubernetes' 30 s default, so
    lowering it truncated both the captures in flight and the notification flush — with the
    kubelet's SIGKILL and nothing logged. New `terminationGracePeriodSeconds` value (default 30),
    a schema `minimum` of 25, a render-time guard as a backstop, and a unit test that reads the
    schema back so `RECONCILE_GRACE + NOTIFY_DRAIN` cannot outgrow the floor unnoticed.
  - At shutdown the dispatcher logs any rate-cap tally it is carrying. That tally never reaches the
    channel, because it rides on the next message and there is not going to be one; the storm was
    already announced by the standalone notice, and the count is in
    `lapilli_notifications_total{result="suppressed"}`.
- **Bundle retention** (`retention.*` in the chart, off by default). Nothing deleted a sealed bundle
  before this, and the chart's volume is 1 GiB — one alert over a 20-pod Deployment at Alertmanager's
  hourly repeat fills it in about nine days, and **a full volume makes every capture fail, not just
  the old ones.**
  - **Bytes are the primary bound**, not age: `retention.maxBytes`. The shipped default is `0`,
    which is off — nothing is reclaimed until an operator chooses a bound. Set it to `""` and the
    chart derives `persistence.size × 0.8`, so the ceiling follows the volume instead of being a
    number nobody updates. `retention.days` is a secondary trim. An age window alone never engages before the disk
    does, which is why the first design of this was rejected.
  - `retention.minFreeBytes` (64 MiB by default) is a **preflight**: a capture that cannot possibly be
    sealed now fails with a `pvc-full:` reason code before it collects, instead of dying part-way
    through on a raw `No space left on device`.
  - **Abandoned staging directories and pack temp files are reclaimed regardless of either bound**, and
    keyed on the capture UID rather than on age, because a capture may legitimately sit in `Sealing`
    for days through a KMS outage. They hold *uncompressed* evidence on the same volume, so on the
    install this feature exists for they are the largest thing reclaimable.
  - A sealed bundle is reclaimed only when **every destination is observed as `Uploaded`**.
    `Refused`, `Conflict` and `Failed` are refusals: those are the states where the local copy is the
    only copy. On an install with no destination at all the local bundle is likewise the only copy, so
    that needs `retention.allowUnexported=true` on purpose, and `NOTES.txt` says what it destroys.
  - The **claim files and the archived signing keys are never touched**: `<incident>.notified`
    (deleting it re-announces month-old incidents and never converges), `<incident>.ieb.owner`
    (deleting it lets a resent alert rebuild a bundle carrying the old incident's identity), and
    `keys/<key_id>.pub` (on a local-only install a rotated key exists nowhere else, so every bundle it
    signed would become unverifiable).
  - **Orphans are off by default** (`retention.reclaimOrphans`). Nothing in Lapilli deletes an
    `IncidentCapture`, so "no live CR" describes human behaviour — and one `kubectl delete
    incidentcapture --all`, or the documented CRD delete-and-recreate upgrade, would otherwise
    authorise a mass delete. The sweep also refuses above 100 files or 5% of the population.
  - Every reclaim is appended to **`reclaimed.jsonl`** on the volume before the file is removed, and a
    reclaim that cannot be recorded does not happen. `status.local` and the metrics are views of it:
    an Event expires within the hour, `status` dies with the CR, and counters reset on restart.
  - A failed unlink — a read-only or WORM-backed volume — is counted as `undeletable` and the bundle is
    **not** recorded as reclaimed.
  - Bounded: a budget around the whole pass, a per-sweep cap, the filesystem walk on a blocking
    thread, and a paged list rather than a second unpaginated copy of the population in a 256 MiB pod.
  - New series: `lapilli_bundle_fs_bytes{state}` from one `statvfs` on the **always-on** poller, so the
    volume is visible on the install that has *not* enabled retention — which is the one whose disk is
    filling; `lapilli_retention_sweeps_total{result}` from process start as the `absent()` sentinel;
    `lapilli_bundles_reclaimed_total{reason}`, `lapilli_reclaimed_bytes_total` and
    `lapilli_reclaim_refused_total{reason}`. Three alerts, with promtool unit tests in
    `scripts/alert-rules-check.sh`.
- **Bundle retention** designed and reviewed before implementation (`docs/design-retention.md`,
  `docs/design-review-round17.md`). Two lenses returned four blockers against the first draft, and the
  document was rewritten rather than patched.
  - "The `.ieb` and **its sidecars**" was the phrase that produced the worst bug: `<incident>.notified`
    and `<incident>.ieb.owner` are not sidecars but the two permanent `O_EXCL` claims behind once-only
    notification and the promise that an incident id's bundle is never overwritten. Deleting the first
    re-announces a month-old incident to Slack and never converges; deleting the second lets a resent
    webhook build a new bundle carrying the old incident's identity.
  - The safety predicate was inverted. `ExportState::settled()` includes `Refused`, `Conflict` and
    `Failed` — the three states the metrics doc defines as "that evidence never reached the destination
    and never will" — so the refusal permitted deleting the only copy exactly when there is no second
    copy. Reclaim now requires every destination observed as `Uploaded`, re-derived rather than read
    from `status`, which a compromised collector could patch into a targeted delete.
  - Age never engages before the disk fills. On the chart's 1 GiB default, one alert over a 20-pod
    Deployment at Alertmanager's hourly repeat fills the volume on **day 9** with a 30-day window having
    deleted nothing. Bytes are now the primary bound, with a preflight free-space check so a capture
    that cannot be sealed fails with a `pvc-full:` reason instead of a raw ENOSPC.
  - The orphan pass was a mass delete waiting for a housekeeping command: nothing in the controller
    ever deletes an `IncidentCapture`, so "no live CR" describes human behaviour, and the *documented*
    CRD upgrade path cascades every CR away. It is now off by default, needs a complete paged list, and
    refuses above a threshold share. It would also have deleted `keys/<key_id>.pub` — the archived
    signing keys, which on a local-only install exist nowhere else — making every bundle they signed
    unverifiable, including bundles safely in an Object Lock bucket.
- A capture trigger that needs no alert rule was designed, reviewed and **returned to its premise**
  before any code was written (`docs/design-event-trigger.md`,
  `docs/design-review-round16.md`). Two lenses found the same first blocker independently: the CR
  labels the design invented to index captures by target are **illegal label values** — the dedup
  bucket is `YYYY-MM-DDTHH:MM` and a colon cannot appear in one — so adding them would have made every
  `IncidentCapture` create fail, including on the shipped Alertmanager path the design claimed it left
  untouched.
  What survives is measured, on real 1.30.0 and 1.37.0 clusters, and is kept in the design doc: the
  `BackOff` event's shape is byte-identical across both versions and the kubelet does aggregate it;
  core `v1.Event` is the right API, because the same object read through `events.k8s.io/v1` has
  `series: null`; **an OOMKilled container emits no OOM event at all**, so a container that OOMs every
  20–30 minutes never enters restart backoff and produces no Warning event ever; and `BackOff` lands in
  ~10 s while a `for: 5m` alert lands in five minutes, which is the opposite of what the design
  assumed. The redesign starts from Pod status rather than the event stream, because a `restartCount`
  increment with `lastState.terminated.reason` is complete where events are not.
- `DESIGN.md` §11: the **postmortem draft** is now in the v0.2 roadmap. Round 11 concluded it was the
  stronger feature and recorded it as "the roadmap's next item"; it never reached the table, so the
  next thing built was chosen from a roadmap that did not reflect the review. A verdict that does not
  land where the next decision is made has no force.
- `DESIGN.md` §11: **bundle lifecycle (retention and deletion)** added to the v0.2 roadmap, with the
  constraints that keep it from being a `retentionDays` flag. Until that shipped (later in this same
  release) nothing deleted a sealed bundle, so they accumulated on the PVC and at every destination
  for the life of the install — a capacity
  problem for a busy cluster and a liability problem for whoever has to answer for what they still
  hold. The design has to start from three facts: a delete feature in an evidence tool is a
  destroy-evidence feature, so deleting must be at least as recorded as capturing; a destination
  under Object Lock will refuse, and reporting success while the object remains is worse than
  refusing; and redaction is best-effort, which argues *for* bounded retention rather than for
  keeping everything forever.
- The controller logs **without ANSI colour**. A pod log is never a terminal, and the colour codes
  wrapped every field name, so `kubectl logs lapilli | grep check=` matched nothing — which defeated
  the decision above to keep the permission detail in the log rather than on an unauthenticated
  endpoint.
- `lapilli_reconcile_errors_total` now also counts failures of the **watch stream**. A dead watch
  means no new capture is ever noticed, and it previously produced a log line and nothing else.
- `docs/egress.md`: the egress allowlist `DESIGN.md` §7 promises, derived from the code — every
  peer the controller opens, when, and why — plus the three ways to enforce it (a CNI with FQDN
  policy, an egress gateway, or maintained IP ranges). **No NetworkPolicy template**, on purpose:
  NetworkPolicy v1 cannot match a DNS name, and most of Lapilli's peers are cloud endpoints whose
  addresses change. Two traps are called out because they bite: `ipBlock` matches the **post-DNAT**
  address, so the API server's ClusterIP is the wrong value; and blanket-excepting the link-local
  range breaks EKS Pod Identity and GKE Workload Identity, which is where the controller's own
  credentials come from.
- The kind E2E validates every chart render against the API server with
  `kubectl apply --dry-run=server --validate=strict`, once per tested Kubernetes minor. A
  chart-generated egress policy was written and withdrawn during review because it placed the
  admin's peers at rule level: the API server silently **pruned** the unknown fields and stored
  *allow-all egress* while `helm lint`, `helm install` and a values review all looked correct. Only
  a server-side dry run reports a pruned field, which is why the check lives in the E2E rather than
  in `helm-renders.sh` — strict decoding needs the API server's openapi and cannot run offline.
- `lapilli-net`: one crate holding Lapilli's outbound-HTTP rules — strict endpoint parsing (no
  userinfo, escapes, brackets, backslashes or control characters), no redirects, refused
  address ranges, and resolved addresses pinned into the client. Shared by remote verify, the
  KMS client, the Prometheus collector and notification, so there is one place to get these
  rules right.
- **Permission self-checks follow the profiles.** Needs are re-derived on every pass from the
  **union** of every `CaptureProfile` in the controller's namespace (profiles are chosen per
  capture, so whatever any of them could ask for must be held), instead of one profile read once
  at startup. A collector check no profile needs is recorded as `not_needed` rather than asked
  — so tightening `pods/log` on an install whose profiles never intend `logs` no longer raises a
  false alarm every 600 s — and an unreadable profile list asks every collector check rather than
  narrowing (the first version recorded them `unknown`, and the E2E's "a denial must not
  read as unanswerable" invariant caught it). New gauge
  `lapilli_permissions_asked`; new `result="not_needed"` on `lapilli_permission_checks_total`.
  When a collector does not run and the latest self-check found a denial it depends on, the
  capture gets a `CollectorDenied` Event naming the check and the check's time, so the cause is
  at the capture and not three hops away in a log. Nothing is refused on the strength of a
  self-check: the bundle is PARTIAL, as before, and it says why.
  (`docs/design-permissions-by-profile.md`)
- **`coverage.deferred`** in `ieb/v1`: a producer may declare collectors it chose not to intend
  because the data is kept elsewhere. Additive — absent and `[]` seal to byte-identical
  manifests. Enforced: no duplicates, disjoint from `collectors_intended`. Never changes the
  verdict; never silent: a `notice`, `(deferred: …)` on the verdict line, and
  `bundle.collectors_run` / `bundle.collectors_intended` / `bundle.deferred` in
  `verify-result/v1`. `lapilli postmortem` and notifications name the set too.
- **`CaptureProfile.spec.deferred`** and the chart's `profile.deferred`: the perishable
  profile. Intend `resources` and `changes`, declare `logs`/`events`/`metrics` as kept elsewhere,
  and every bundle carries `coverage.deferred`. A name in both lists is refused by the API
  server (a CEL rule on the CRD) and, for an API server that does not enforce CEL, by the
  reconciler before anything is collected. New counter `lapilli_deferred_captures_total`. The
  E2E (`test/e2e/deferred.sh`) exercises the arc on a kind cluster: CEL refusal, the perishable
  bundle, a denied `pods/log` named at a full-profile capture, and the same tightened Role
  reading as *not needed* — `lapilli_permissions_denied` back to 0 — under the perishable one.

### Changed
- `CaptureProfile.spec.collectors` defaults to `[logs, resources, events, changes]` — the same
  default the chart writes — instead of `[logs]`. A hand-written profile that omitted the field
  got the thinnest capture there is and one no chart install ever produced. Alpha CRD; a profile
  that wants logs only now says so.

### Security
- A capture could get a sealed (and signed) bundle for **another cluster** by setting
  `IncidentCapture.spec.clusterId`, and an unsafe `incidentId` became a path used by the
  staging cleanup. The controller now refuses:
  - a capture for another cluster (`cluster-mismatch`);
  - an unsafe incident id (`invalid-incident-id`);
  - a webhook-form id claimed by another capture (`reserved-incident-id`);
  - a second capture for an incident id already claimed (`incident-id-in-use`), claimed
    atomically per id. A bundle is never overwritten.
- Bundles are written only under the controller's bundle root (`LAPILLI_BUNDLE_ROOT`,
  default `/var/lib/lapilli/bundles`). A profile with another `export.path` is refused.
- `metrics.prometheusUrl` is chosen by whoever can edit a `CaptureProfile`, and the response
  body lands in the bundle. It is now parsed strictly (no credentials, query, fragment, escapes
  or backslashes), plain HTTP is accepted only for a cluster-local or loopback host, a 3xx is
  an error instead of being followed, and every resolved address is refused if it is
  link-local (169.254.0.0/16, fe80::/10 — where cloud metadata lives), multicast, broadcast or
  unspecified, then pinned so DNS cannot rebind between the check and the connection. A bad
  value fails the metrics collector (the capture is PARTIAL) and stages nothing. The chart
  refuses the same shapes at install time.
- An endpoint's path is never printed. `lapilli-net` shows only the scheme and authority, because
  for a chat webhook the path **is** the credential — it was previously written to the log on
  every successful send and, through error strings, into `status`, where anyone with
  `get incidentcaptures` could read it.
- `<incident>.summary.json` never holds the log line. The sidecar sits outside the hash tree and
  outside the signature. When this was written nothing read it and nothing pruned it, so a `.ieb`
  deleted for retention would have left the container's last words behind it in plaintext. Both
  halves have since changed — `reconcile.rs` reads the sidecar back so a restart can still send a
  notification, and retention lists `.summary.json` as reclaimable beside the `.ieb` — but the
  reason the log line is kept out of it stands.
- A grouped incident is claimed **member by member**. Claiming only the capture the message
  names left the other pods of a group unclaimed, so each re-announced the incident on its next
  reconcile — and a watcher relist was enough to trigger that.
- Strings an alert author chose are escaped exactly once, at the leaf, and every Slack block cap
  counts rendered characters: escaping again at block level turned a real `&` into `&amp;amp;`,
  and capping the input could not hold a block under Slack's limit because `&` expands to five
  characters. A block over the limit was a 400 the code treated as final, with the claim already
  spent — so the incident was never announced at all.
- A notification route name is validated in code, not only by the chart's schema, because it is
  used as a path segment when reading the route's Secret; and an unknown field in a route is
  refused rather than silently meaning "the default".
- Notification never carries a log line, in any mode: bundles deliberately do not redact logs,
  and free-text redaction is heuristic. `detail: content` adds only the changed field and its
  values, which the redactor does cover. Strings an alert author or a workload chose (`rule`,
  `pod`, the diff's actor) are escaped and rendered as `plain_text`, so `<!channel>` and
  `<url|label>` cannot be injected; `spec.trigger.rule` is pattern-constrained in the CRD.
  Once-only is an `O_EXCL` claim file, and the bundle path and object URL are recomputed from
  configuration, so anyone who can patch `status` can neither replay nor suppress a message
  nor forge the evidence link.
- `redact_text` ignored its `strict` flag, so `redaction.mode: strict` was byte-identical to
  `default` on free text. Fixed, and a secret-looking `name: value` (not only `name=value`) is
  now redacted, including a header whose value is the next token.

### Fixed
- The kind E2E acted on **whatever kubectl context happened to be current** rather than on the
  cluster it had just created. One run's commands went to a GKE cluster when the context changed
  underneath it mid-run; they were refused there for lack of permission, which is luck rather than
  a safeguard. `test/e2e/run.sh` now creates the cluster into a kubeconfig of its own, exports
  `KUBECONFIG` for every sub-script, and refuses to continue unless the current context is the
  cluster it made. The developer's own kubeconfig is no longer touched at all.
- The controller ignored **SIGTERM**, which is what Kubernetes sends: every rollout skipped the
  shutdown path and the pod was killed at the end of its grace period. It now shuts down on
  SIGTERM as well as SIGINT and, because a notification group is claimed before it is posted,
  flushes the groups still coalescing (bounded at 10 s) rather than leaving those captures marked
  notified and never announced.
- `Summary` reported a **running** container as terminated: the `lastState.terminated` read was
  conditioned on the container status existing rather than on the terminated block. A live pod
  rendered as `terminated, restart 0` while the same summary said `no terminated instance`, and
  `Summary::is_empty()` could never be true for any pod with a status. `restarts` now lives on
  `Summary` (it is a property of the container, not of a death), so a flapping live pod is
  reported as such.
- The chart NOTES still suggested the removed `lapilli demo --webhook-service`.
- `lapilli demo` failed with kubectl 1.30 (a JSON document stream with `---` separators); it
  now applies a single `List`.

### Migration
- `metrics.prometheusUrl` must now be a bare http(s) URL with no credentials, query or
  fragment, and plain `http://` only to a cluster-local or loopback host. A two-label name
  like `http://prometheus.monitoring:9090` is refused (it is indistinguishable from a public
  name): write `http://prometheus.monitoring.svc:9090`. A bracketed IP literal
  (`http://[::1]:9090`) is no longer accepted — use a name or an IPv4 literal.
- `profile.name` must be an RFC 1123 subdomain (lowercase alphanumerics, `-` and `.`), and
  `spec.profile` on an `IncidentCapture` carries the same pattern. The chart used to accept any
  non-empty name while the API server would have taken it too — but a name the chart allowed and
  the CRD now refuses would install cleanly and then produce no captures at all.
- `status.message` is capped at 1024 bytes by the CRD, and the controller truncates before
  writing, so it never has its own patch rejected. Upgrading the CRDs needs
  `kubectl apply --server-side --force-conflicts` (see `docs/COMPATIBILITY.md` §3).
- Alertmanager must now send the webhook token: add `http_config.authorization.
  credentials_file` to the Lapilli receiver (the chart NOTES show how to copy the token), or
  set `webhook.auth.enabled=false` (not recommended).
- `lapilli demo --webhook-service` was removed (the demo no longer uses the Service).
- `clusterId` must be `[A-Za-z0-9._-]`, at most 83 characters; e.g. an EKS ARN is no longer
  accepted. Pick a short name. Three places enforce it now: the chart's `values.schema.json`,
  the controller's own `--cluster-id` check at startup, and — new — the CRD's
  `spec.clusterId`. (An earlier wording here said "the controller refuses others", which
  overstated it: a capture whose `clusterId` was merely path-unsafe had object-store export
  disabled with a logged error and was still captured.)

## [0.1.0] - unreleased

First release. The bundle format is `lapilli.dev/ieb/v1` and is frozen from this release.

### Added
- Alertmanager webhook → `IncidentCapture` → collectors (logs incl. the previous container
  instance, resources, events + timeline, change indicators, optional PromQL metrics) →
  sealed, portable `.ieb` bundle.
- `diffs/`: before/after pod-template diffs of every rollout in the capture window, for
  Deployments, StatefulSets and DaemonSets, from the revision history Kubernetes already
  keeps; opt-in key-level diff of ConfigMaps whose referenced name changed.
- Redaction v1 at capture time, recorded in `redaction.json` (`default` / `strict` / `off`).
- `lapilli verify` (exit codes 0 OK / 1 FAILED / 2 PARTIAL / 3 cannot evaluate / 64 usage),
  optional static-key ECDSA signing, `lapilli keygen`, `lapilli demo`, `lapilli unpack`.
- Helm chart with a values schema; tested on Kubernetes 1.30 and 1.37.

### Security
- `lapilli verify` streams `.ieb` files instead of extracting them, enforces path rules,
  rejects links, duplicate and case-colliding entries, and unlisted files under
  `signature/`, and applies resource limits.
- The signing declaration is part of the signed manifest; authenticity is established only
  with `--key`.

### Migration
- Development builds before v0.1.0 wrote `lapilli.dev/ieb/v0` bundles, which no release
  reads (`lapilli verify` exits 3).
