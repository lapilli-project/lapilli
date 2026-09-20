# Changelog

All notable changes to Kairn are listed here. Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Kairn follows SemVer; before 1.0 a minor release may change alpha surfaces (CRDs, chart
values, CLI commands other than `kairn verify`). Compatibility commitments are in
[`docs/COMPATIBILITY.md`](docs/COMPATIBILITY.md). Changes that need action on upgrade are
listed under **Migration**.

## [Unreleased]

### Added
- Object-store export (S3, S3-compatible, GCS): after sealing, each bundle is copied to
  admin-defined destinations (`export.destinations` in the chart; profiles reference them by
  name). Conditional create with a service-verified SHA-256, never overwriting; an existing
  object with other bytes is a `conflict`. Retries with backoff; per-destination status
  (`status.exports`, `EXPORT` column) and Events. Only a verified bundle of the capture is
  ever uploaded. `kairn demo` captures stay local. See `docs/design-export.md`.
- Chart: `serviceAccount.annotations` (IRSA, GKE Workload Identity).
- Webhook authentication: `Authorization: Bearer <token>`, on by default. The chart generates
  the token (kept across upgrades) or uses `webhook.auth.existingSecret`; optional
  `webhook.networkPolicy`. `kairn demo` fires its alert from inside the controller pod.
  Authentication runs before the body is read; bodies are capped at 256 KiB and requests
  at 16 concurrent; rejections are counted and logged at most every 10 s. `/healthz` moved
  to its own port (8081) so a NetworkPolicy on the webhook port never blocks probes.

- `kairn verify s3://… | gs://… | https://…`: verifies an object straight from a bucket or
  a presigned URL, streamed without being stored, with the same verdicts and exit codes as
  a local file. S3 objects also get a version history check (a key written more than once,
  or with a delete marker, is FAILED; `--current-only` skips it), and the key's
  `<cluster>/<incident>.ieb` is checked against the bundle (`--any-key` skips it). New
  `--expect-sha256` (also for local files) and `--version-id`. Read failures are exit 3.
  See `docs/design-remote-verify.md`. `--no-default-features` builds the CLI without any
  network code; the controller image ships that build.
- `status.exports.<name>.sha256` and `.versionId`: the uploaded object's hash and store
  version, for `kairn verify --expect-sha256 / --version-id`.
- Export refuses to run under a cluster id that isn't `[A-Za-z0-9._-]` (at most 100): the
  id is a key segment. The chart fails the render in that case.

- `kairn verify --output json`: one `kairn.dev/verify-result/v1` document on stdout for
  every outcome (exit 0–3), with stable problem codes (`integrity`, `context`, `signature`,
  `custody`, …), the input's size and SHA-256, the bundle's own cluster and incident, and
  the bucket version history. Stable from v0.1.0: `spec/VERIFY-RESULT.md`. The fixtures now
  pin each case's problem codes too.
- `kairn-bundle`: `VerifyReport.problems` is now `Vec<Problem>` (`code` + `message`); new
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
- `kairn verify s3://…` on a deleted key (delete markers, no current object) is FAILED
  (`custody`) instead of "no such object".
- The manifest's `schema_version` is dispatched before the duplicate-member check
  (IEB-SPEC §9).

- **KMS signing (AWS KMS, GCP Cloud KMS)**: `signing.mode=kms` with `signing.kms.key` (an
  AWS key ARN or a GCP key version). The key never enters the cluster; bundles are
  unchanged (`ieb/v1`, verified with `kairn verify --key`).
  - Captures collect once, then wait in `Sealing` while KMS is unavailable, with backoff,
    across restarts, and are never written unsigned. `kairn.dev/retry-seal` re-drives a
    failed seal.
  - `status.seal` records the key id, manifest digest and request id, to match cloud audit
    logs.
  - `kairn key fetch --kms <key>` writes the public key from the KMS itself.
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
  generic `kairn.dev/notification/v1` JSON body. **One message per incident, not per pod**:
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
- `kairn_notifications_total{result}` (`sent`, `repeat`, `failed`, `suppressed`, `dropped`,
  `already-notified`) and `kairn_notify_routes{state}` — a gauge of routes that loaded versus
  routes that are configured but unusable, set at startup and absent when no route is
  configured, so "notification is off" and "notification is broken" never read the same.
  `status.notification = {state, at, reason, route}` (reporting only) with a `NOTIFY` column on
  `kubectl get incidentcapture`; `reason` is one of a fixed set of codes, never transport text.
- `kairn cat-bundle` is now a documented command (it was hidden): it is how an un-exported
  bundle is pulled out of the distroless controller image, and the command a notification
  prints.
- `docs/egress.md`: the egress allowlist `DESIGN.md` §7 promises, derived from the code — every
  peer the controller opens, when, and why — plus the three ways to enforce it (a CNI with FQDN
  policy, an egress gateway, or maintained IP ranges). **No NetworkPolicy template**, on purpose:
  NetworkPolicy v1 cannot match a DNS name, and most of Kairn's peers are cloud endpoints whose
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
- `kairn-net`: one crate holding Kairn's outbound-HTTP rules — strict endpoint parsing (no
  userinfo, escapes, brackets, backslashes or control characters), no redirects, refused
  address ranges, and resolved addresses pinned into the client. Shared by remote verify, the
  KMS client, the Prometheus collector and notification, so there is one place to get these
  rules right.

### Security
- A capture could get a sealed (and signed) bundle for **another cluster** by setting
  `IncidentCapture.spec.clusterId`, and an unsafe `incidentId` became a path used by the
  staging cleanup. The controller now refuses:
  - a capture for another cluster (`cluster-mismatch`);
  - an unsafe incident id (`invalid-incident-id`);
  - a webhook-form id claimed by another capture (`reserved-incident-id`);
  - a second capture for an incident id already claimed (`incident-id-in-use`), claimed
    atomically per id. A bundle is never overwritten.
- Bundles are written only under the controller's bundle root (`KAIRN_BUNDLE_ROOT`,
  default `/var/lib/kairn/bundles`). A profile with another `export.path` is refused.
- `metrics.prometheusUrl` is chosen by whoever can edit a `CaptureProfile`, and the response
  body lands in the bundle. It is now parsed strictly (no credentials, query, fragment, escapes
  or backslashes), plain HTTP is accepted only for a cluster-local or loopback host, a 3xx is
  an error instead of being followed, and every resolved address is refused if it is
  link-local (169.254.0.0/16, fe80::/10 — where cloud metadata lives), multicast, broadcast or
  unspecified, then pinned so DNS cannot rebind between the check and the connection. A bad
  value fails the metrics collector (the capture is PARTIAL) and stages nothing. The chart
  refuses the same shapes at install time.
- An endpoint's path is never printed. `kairn-net` shows only the scheme and authority, because
  for a chat webhook the path **is** the credential — it was previously written to the log on
  every successful send and, through error strings, into `status`, where anyone with
  `get incidentcaptures` could read it.
- `<incident>.summary.json` never holds the log line. The sidecar sits outside the hash tree and
  outside the signature, nothing reads it and nothing prunes it, so a `.ieb` deleted for
  retention would have left the container's last words behind it in plaintext.
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
- The chart NOTES still suggested the removed `kairn demo --webhook-service`.
- `kairn demo` failed with kubectl 1.30 (a JSON document stream with `---` separators); it
  now applies a single `List`.

### Migration
- `metrics.prometheusUrl` must now be a bare http(s) URL with no credentials, query or
  fragment, and plain `http://` only to a cluster-local or loopback host. A two-label name
  like `http://prometheus.monitoring:9090` is refused (it is indistinguishable from a public
  name): write `http://prometheus.monitoring.svc:9090`. A bracketed IP literal
  (`http://[::1]:9090`) is no longer accepted — use a name or an IPv4 literal.
- Alertmanager must now send the webhook token: add `http_config.authorization.
  credentials_file` to the Kairn receiver (the chart NOTES show how to copy the token), or
  set `webhook.auth.enabled=false` (not recommended).
- `kairn demo --webhook-service` was removed (the demo no longer uses the Service).
- `clusterId` must be `[A-Za-z0-9._-]`, at most 83 characters (the chart schema and the
  controller refuse others); e.g. an EKS ARN is no longer accepted. Pick a short name.

## [0.1.0] - unreleased

First release. The bundle format is `kairn.dev/ieb/v1` and is frozen from this release.

### Added
- Alertmanager webhook → `IncidentCapture` → collectors (logs incl. the previous container
  instance, resources, events + timeline, change indicators, optional PromQL metrics) →
  sealed, portable `.ieb` bundle.
- `diffs/`: before/after pod-template diffs of every rollout in the capture window, for
  Deployments, StatefulSets and DaemonSets, from the revision history Kubernetes already
  keeps; opt-in key-level diff of ConfigMaps whose referenced name changed.
- Redaction v1 at capture time, recorded in `redaction.json` (`default` / `strict` / `off`).
- `kairn verify` (exit codes 0 OK / 1 FAILED / 2 PARTIAL / 3 cannot evaluate / 64 usage),
  optional static-key ECDSA signing, `kairn keygen`, `kairn demo`, `kairn unpack`.
- Helm chart with a values schema; tested on Kubernetes 1.30 and 1.37.

### Security
- `kairn verify` streams `.ieb` files instead of extracting them, enforces path rules,
  rejects links, duplicate and case-colliding entries, and unlisted files under
  `signature/`, and applies resource limits.
- The signing declaration is part of the signed manifest; authenticity is established only
  with `--key`.

### Migration
- Development builds before v0.1.0 wrote `kairn.dev/ieb/v0` bundles, which no release
  reads (`kairn verify` exits 3).
