# Changelog

All notable changes to Lapilli are listed here. Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Lapilli follows SemVer; before 1.0 a minor release may change alpha surfaces (CRDs, chart
values, CLI commands other than `lapilli verify`). Compatibility commitments are in
[`docs/COMPATIBILITY.md`](docs/COMPATIBILITY.md). Changes that need action on upgrade are
listed under **Migration**.

## [Unreleased]

### Changed

- `docs/security-scanning.md` carries a **date per row** and the published `0.2.0` scan. The point
  of the new rows is the comparison: `0.2.0` shows six High where `0.1.0` showed three, and
  re-scanning `0.1.0` on the same day gives **exactly the same six, down to the CVE ids**. The
  image did not get worse — the vulnerability database learned three `libssl3t64` advisories in the
  three days between. Reading the release as the cause is the obvious mistake and the table used to
  invite it.
- Those three are in a package the page did not account for, which is the condition `RELEASE.md`
  step 9 names as needing action rather than a note. `libssl3t64` is in the base layer and no
  Lapilli process opens it — TLS is `rustls` with `ring` compiled in, and `deny.toml` bans
  `openssl-sys` and `native-tls` with the `supply-chain` job making the ban a gate. Counted anyway,
  because a scanner counts them and an adopter's policy may refuse on the count alone.
- The chart is on Artifact Hub (`artifacthub.io/packages/helm/lapilli/lapilli`), with
  `artifacthub-repo.yml` pushed beside it in the registry as the ownership claim.

## [0.2.0] - 2026-10-01

### Security

- **`redaction.mode: strict` with a non-empty `redaction.plaintext` redacted *less* than
  `default`.** An exempted name skipped redaction entirely instead of skipping only strict's
  widening, so it never reached the default name rules: with `plaintext: [DB_PASSWORD]`, a
  GitHub token and a `Authorization: Bearer …` header came through in the clear where
  `mode: default` removes them. Choosing the stricter mode made the bundle less redacted, which
  is the opposite of what the field's own documentation said in `Policy::plaintext`
  (*"exempt from `Strict`"*) and in the CRD. Present in 0.1.0.

  An exempt name is now judged by the default rules, which is the same shape
  `redact_config_value` already used for a multi-line key. The invariant — **`strict` never
  redacts less than `default` on the same input** — is now a property test over every named
  surface (env values, probe headers, annotations, ConfigMap values), and it is what makes
  `off < default < strict` a real order rather than a declaration order; the measurement that
  found this was run because an admin floor needs that order to exist.

  **If you set `mode: strict` with a non-empty `plaintext` list**, bundles sealed before this
  release may carry values under those names in the clear. Every bundle records both halves, so
  the question is answerable from the bundle: `lapilli unpack <bundle> <dir>` and read
  `<dir>/redaction.json` — `mode: "strict"` with a non-empty `plaintext_names` is the affected
  combination. `lapilli verify` reports the mode but not the exemption list; surfacing it is a
  roadmap item, not a change made in the same commit as the fix, because `lapilli verify` is the
  one command whose output carries a compatibility commitment.

- `redact_text`'s documentation claimed `strict` also widens the `name: value` form. It does not,
  and the condition that appeared to do it could never fire, because a name that classifies as
  nothing has no disallowed values. Keeping it that way is deliberate — in an event message the
  colon form is prose, and widening it would take `reason: CrashLoopBackOff` and the timestamps
  with it, which the existing readability test pins in both modes. The dead condition is gone and
  the asymmetry is stated where the code is.

### Fixed

- **On a default install the retention sweep never ran, so two kinds of garbage accumulated
  forever — and one of them carries workload content.** `spawn` returned early when no bound was
  set, which is the chart's default (`maxBytes: 0`, `days: 0`). But `decide` returns
  `Reason::Abandoned` and `Reason::Handoff` *before it reads the policy at all*, because neither is
  a retention decision: one is a staging directory or pack temp file abandoned by a capture that is
  no longer live, the other a `.unsent` notification hand-off past its cooldown that no dispatcher
  can still send.

  The second is the reason this matters beyond disk. Round 30 took `.unsent` out of the
  never-reclaim list precisely because it is the only file under the bundle root holding workload
  content — a serialized group with the namespace, the owner, every member's pod name and every
  member's summary including a diff's before/after values — so "survive forever" was the wrong
  default for it. The age bound meant to collect it lived inside a sweep the default install did not
  run. That privacy fix was inert where it mattered most.

  It also made `LapilliBundleVolumeFilling`'s own advice self-contradictory: it tells an operator
  whose volume is filling to enable retention *and* that abandoned staging directories are reclaimed
  regardless of the bounds — true of the bounds, false of a sweep that was not running.

  The sweep now always runs. Only which reasons are reachable changes: with no bound set,
  `MaxBytes`, `Age` and `Orphan` cannot be returned, so no bundle, summary or claim is touched. A
  test pins both halves and then pins that the same month-old bundle *is* taken once a bound exists,
  so the refusals are the policy's doing rather than an accident of the candidate.
  `Policy::sweeps()` is now `Policy::has_bounds()`: the old name described what it was used for, and
  that use was the bug.

  **Making it run exposed a classification that was wrong from the start.** `sealing::seal_file` is
  the staging path with `.seal.json` appended, which makes it a *sibling* of the directory whose
  name also begins with `.staging-`. `abandoned_tail` stripped that prefix and returned a tail
  ending in `.seal.json` rather than in the capture uid, so `holds_uid` could never match a live
  capture: a capture waiting through a KMS outage had the state it resumes from deleted under it
  and came back `staging-lost`. The file that exists to survive a KMS outage was removed during
  one. It was unreachable only because the sweep did not run on a default install. The tail now
  strips `.seal.json` before the uid is looked for, so a seal file whose capture really is gone is
  still collected and a live one is refused. The regression test takes the filename from
  `sealing::seal_file` itself rather than spelling it out — every existing test used directory
  names, which is exactly why this was missed.

  This shipped once and was reverted the same day: the kind E2E caught it on `main`, the revert
  said what was not yet understood, and the journal answered it. The release gate did its job.

- `test/e2e/run.sh` asserted that retention had not swept on a default install by checking
  `lapilli_retention_sweeps_total{result="ok"} == 0`. Its own comment claimed two things — the
  counter is zero *and* nothing was reclaimed — while checking only the first, which held as a
  proxy because the sweep did not run at all. It now asserts the property: no bundle was taken for
  the byte ceiling, for age, or for a missing `IncidentCapture`. Waste that no policy governs may be
  collected, which is the point. The same shape as the `captureprofiles` RBAC assertion that once
  froze "get only" in place of "no write".

- The kms E2E's failure handler dumps `reclaimed.jsonl` and the bundle root, read from the node
  rather than through `kubectl exec` — the controller image is distroless and has no shell. That
  failure reported `retention sweep reclaimed=2` and nothing said *what*, so "a live capture's file
  was deleted" and "two harmless leftovers were collected" read identically.

- **A bundle small enough to email could render a document too large to open.** `lapilli postmortem`
  and the MCP `summary` tool had no size bound: a 1,615-byte `.ieb` that verifies OK rendered a
  1,051,015-byte document, and the same channel at 256 MiB rendered 268 MB in five seconds. The
  Markdown escaping amplifies 2x to 3x rather than bounding, and `ieb/v1` fixes no per-member size
  for `resources/`, `logs/`, `timeline.json` or `diffs/**` — only the 2 GiB whole-bundle total.

  Every name-shaped metadata value is now bounded at the leaf that fills it: object kind and name,
  actor, revisions, container, termination reason and a changed field's path in `summary.rs`
  (`MAX_NAME = 512`, above every Kubernetes limit any of them can carry), and the cluster id, rule,
  timestamps, window, producer version and collector names in the renderer. The caps sit in
  `summary.rs` rather than in the renderer because the same `Summary` is what the MCP `summary` tool
  sends, and a renderer-side bound would have left that tool open.

  The one evidential channel keeps its evidence. An event message is bounded at 1024 characters and
  **the table says how many it cut**; the whole message stays in `timeline.json`, in the bundle,
  under the signature — the choice `ieb/v1` already makes for a truncated log tail. The verifier's
  problem list is bounded the same way and names `lapilli verify --output json`, which carries all
  of it.

  **No real bundle's document changes**: all 43 frozen fixtures render byte-identically before and
  after. `docs/design-postmortem.md` carries the measurement and what it settled.

- **Two of the four `lapilli_alerts_dropped_total` reasons were counted and never exposed.** The
  render loop carried its own list of two while the callers named four, so an operator losing
  alerts to `bad-firing-ts` — shipped in 0.1.0 — or to `bad-rule-name` saw nothing move, on a
  series whose whole job is to make that loss visible. The reason is now an enum (`AlertDrop`)
  that both the increment and the render go through, so one cannot exist without the other, and
  the test reads the rendered body rather than a second list. `docs/metrics.md` gains
  `LapilliAlertsRefusedByTheCRD` over both reasons, with a promtool timeline that proves a flat
  zero does not fire it and that its regex does not swallow the other two.

- `docs/metrics.md` said the drop reasons were "absent until the first drop"; they are emitted
  from process start at zero, which is what the code has always done and what absence means for
  every other series in that document. A rule written on `absent()` would have been wrong.

### Changed

- **`cargo deny check bans advisories` runs in the local gate too.** Both were CI-only, which is the
  asymmetry this project has now been bitten by four times: a gate that runs only on the runner
  makes "the local gate is green" mean less than it reads. `check bans` is what keeps a C crypto
  stack out of the shipped controller and `check advisories` is the advisory gate. Skipped with a
  message, never silently, when cargo-deny is not installed — the shape `release-check.sh` already
  uses for the MCP client. One caveat is recorded where someone would hit it: cargo-deny reads
  *cached* index metadata for yanked crates, so a stale local index reports nothing where a fresh
  runner reports the yank.

- `yoke-derive` moves to `0.8.4`. `0.8.3` was yanked at crates.io, which is how the advisory gate
  earns its place: the finding came from its first real run. The lock bump made
  `THIRD-PARTY-LICENSES.md` stale and the attribution gate caught that in the same pass — a
  generated file that goes out of date whenever the lock moves, which is why that check exists.

- **On crates.io**: `lapilli`, `lapilli-bundle`, `lapilli-net`, `lapilli-kms` and
  `lapilli-controller`, all `0.1.0`. `cargo install lapilli` builds the CLI from the registry.
- **The CLI crate is `lapilli`** (it was `lapilli-cli`; its binary always was `lapilli`). Its
  directory is still `crates/lapilli-cli/`. The binary, the image and the chart are unchanged.

- CI's actions move to `actions/checkout@v7`, `azure/setup-helm@v5.0.1` and
  `helm/kind-action@v1.15.0`. Dependabot proposed all nine action bumps as one group; the six that
  only `release.yml` uses were **not** taken, because that workflow runs on a tag alone, so a green
  `ci` is silent about them — and two of them (`actions/upload-artifact`,
  `actions/download-artifact`) are what carry the per-platform image digests and the CLI tarballs
  between jobs. They are listed in `release.yml`'s header with the reason, and belong in a change
  of their own verified by a pre-release tag. `CONTRIBUTING.md` now says to split these PRs.

- **`main` is protected, and what it enforces is written down.** The repository had no branch
  protection at all: every commit, including all of today's, went straight to `main`, while
  `CONTRIBUTING.md` described a fork → topic branch → pull request → review flow. A ruleset now
  requires a pull request, all 13 `ci` checks (the kind E2E among them — four separate times a
  change passed the local gate and only a cluster found the defect), an up-to-date branch and
  resolved review threads, and blocks force-push and deletion.

  Two choices are recorded rather than left to be inferred. **Required approvals are 0**, because
  there is one maintainer and GitHub does not let anyone approve their own pull request — one
  approval would stop the project rather than review it; it becomes 1 in the same change that adds
  the second maintainer. And the **admin role bypasses**, so the maintainer still pushes directly;
  a bypass nobody mentions reads as a rule nobody has, so `CONTRIBUTING.md` names it in a table.

- **`CONTRIBUTING.md` said unsigned commits could not be merged, and nothing checked.** There was no
  DCO gate anywhere in the repository, so an unsigned commit could be merged and nothing would
  notice — the same shape as `deny.toml` naming an advisory gate that did not exist, this time in
  the document an outside contributor reads first. A required `Every commit carries a DCO sign-off`
  job now checks every commit in a pull request against its own author, skips merge commits, and
  prints the exact `Signed-off-by` trailer it expected plus the command that adds it. Verified both
  ways against real commits before it was required.

- `.github/CODEOWNERS` and a pull-request template. The template asks how a change was *verified*
  rather than whether it was tested, asks for a mutation proof at the site a new check governs, and
  asks outright whether only a cluster can judge the change.

- **What Lapilli reaches is stated as a measured boundary** instead of "the instant an alert
  fires": *evidence that outlives the alert, and dies before the postmortem.* A CronJob's failed
  pod is deleted when its next run is created, so anything scheduled more often than its alert's
  `for:` destroys its own evidence first — measured on kind 1.37 in
  `docs/design-trigger-reachability.md`, which also records what it leaves open.

### Migration

Nothing in this release requires a CRD re-apply or a chart value change. Three behaviours change
on upgrade, and one of them can change what a bundle contains.

**If you set `redaction.mode: strict` with a non-empty `redaction.plaintext`, you will now see more
redaction, and bundles sealed by 0.1.0 may carry cleartext.** An exempted name used to skip
redaction entirely; it now skips only strict's widening and is still judged by the `default` rules.
So a name on that list that looks like a credential — `DB_PASSWORD`, `api_key`, `Authorization` —
now comes out `<redacted>` where 0.1.0 left the value in. If you listed such a name deliberately to
keep the value readable, that value was also readable to every destination the bundle reached, which
is the defect. Names with no credential shape (`CACHE_WARMUP`, `LOG_LEVEL`) are unaffected.

To find out whether an existing bundle is affected, without opening `resources/`:

```sh
lapilli unpack <bundle> /tmp/b && cat /tmp/b/redaction.json
```

`"mode": "strict"` with a non-empty `plaintext_names` is the affected combination. A `mode` that is
not `strict` settles it on its own, and `plaintext_names` may be absent rather than `[]` in a
minimal bundle, which answers the same way. There is no 0.1.x
backport: `SECURITY.md` supports the latest minor only before 1.0, so 0.2.0 is the remedy.

**A default install now reclaims two kinds of leftover.** With `retention.maxBytes: 0` and
`retention.days: 0` — the defaults — the sweep did not run at all, so an abandoned staging directory
and a notification hand-off past its cooldown stayed on the volume forever. Both are now collected.
No bundle, summary or claim is touched: with no bound set, the age, byte-ceiling and orphan reasons
cannot be reached. If you were relying on `.unsent` files persisting for inspection, copy them
before upgrading — they carry workload content, which is why they are not kept indefinitely.

**`lapilli postmortem` output is bounded.** Metadata values are cut at 512 characters and an event
message at 1024, each marked with an ellipsis, and the timeline says how many messages it cut. Every
cap is above the longest value Kubernetes can legitimately produce, so a real bundle's document is
unchanged — verified byte-for-byte against all 43 frozen fixtures. The whole value always remains in
the bundle.

**For anyone depending on `lapilli-controller` as a library** (not the container image or the chart):
`Policy::sweeps()` is now `Policy::has_bounds()`, and `Metrics::alert_dropped` takes an `AlertDrop`
instead of a `&'static str`. This is the pre-1.0 minor slot for breaking changes.

## [0.1.0] - 2026-09-28

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

- The release workflow builds the controller image per platform on native runners
  (`ubuntu-latest`, `ubuntu-24.04-arm`) and merges the manifests; `v0.1.0-rc.1`'s arm64 leg
  took 2 h 55 min under QEMU. The release-notes digest is the manifest list's.
- `scripts/release-check.sh` drives `lapilli mcp` over stdio through the reference MCP client
  (`test/mcp/check.sh`) when `npx` is present, and says so when it is not.
- `lapilli postmortem <bundle|dir>` — the draft a human then writes. It **transcribes**: every
  line is a value that exists in the bundle with the file it came from, and Impact, Root cause,
  Contributing factors and Action items are emitted as **empty headings**, because a blank
  heading is an honest prompt and a filled one would be a guess (`DESIGN.md` §2). Markdown on
  stdout; there is no `--format` and no `--template`, because a rendering people can change is a
  rendering whose provenance claims stop being true.

  It verifies first, and the verdict decides **how** it renders, not whether. OK and PARTIAL
  render (PARTIAL names the collectors that did not run, so a thin section reads as an incomplete
  bundle rather than a quiet incident; the first version filtered the problems on a code that does
  not exist and printed "PARTIAL: none did not run" on every partial bundle — fixed before
  release, in the same change as `coverage.deferred`). FAILED **still renders**, behind a banner, and exits 1 —
  a failed bundle is exactly when someone needs to see what it *claims*, and printing nothing
  sends them to `cat` the files with no verdict attached to anything. CANNOT_EVALUATE refuses
  with exit 3: that is the opposite of PARTIAL, not a milder FAILED, and there is no verified
  tree to render from. The exit code never contradicts `lapilli verify` on the same bundle, which
  a test pins across all 43 released fixtures (47 verify cases).

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
- Chart: `imagePullSecrets` (`[{name: regcred}]`), rendered on the pod spec only when set, so a
  team that mirrors the image into a private registry can install. There was no value for it;
  found smoke-testing `v0.1.0-rc.1` while the GHCR package was still private.
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
  network code; the controller image ships `--no-default-features --features mcp`
  (`Dockerfile`): no outbound network code, plus the inbound MCP server.
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
  E2E (`test/e2e/deferred.sh`) proves the arc on kind (release gate, 2026-09-24, Kubernetes
  1.30.0 and 1.37.0): CEL refusal, the perishable
  bundle, a denied `pods/log` named at a full-profile capture, and the same tightened Role
  reading as *not needed* — `lapilli_permissions_denied` back to 0 — under the perishable one.
- **`lapilli mcp`** — the bundle as a tool an agent can call, over the Model Context Protocol.
  Five tools: `find_bundles` by what an alert carries (namespace, pod or `checkout-*`, rule, a
  time window), `verify` (the `verify-result/v1` document), `read_file` (any file the verified
  hash tree names — `resources/*.json`, the point-in-time object bodies; `diffs/**`, the rollout
  diff; redacted at capture), `summary`, `postmortem`. Stdio for a laptop; `--http` with a
  bearer token for the controller pod, where the bundles are: chart `mcp.enabled` adds a second
  container with the bundle volume mounted read-only and a Service `<release>-mcp`. Only `.ieb`
  files; only bundles that verify OK or PARTIAL serve files; `logs/**` only with
  `--allow-logs` / `mcp.allowLogs`, flagged untrusted. `incident.target {namespace, pod}` joins
  the manifest (additive; omitted when absent, so existing bundles are byte-identical) so a
  lookup never unpacks a bundle; `lapilli-bundle::read_manifest` streams only the manifest.
  `integrations/holmesgpt/` carries the `mcp_servers` snippet and a bash toolset.
  (`docs/design-distribution-path.md`, `docs/design-review-round26.md`)
- An alert with no `pod` label is refused and counted:
  `lapilli_alerts_dropped_total{reason="no-pod"}`, with the `LapilliAlertsWithoutPod` rule in
  `docs/metrics.md`. Such an alert (a `KubeNodeNotReady`, or any node- or cluster-level rule
  routed here) used to fall through with an empty pod and the controller's own namespace, and
  produced a **signed bundle with no evidence** — `events.json` and `timeline.json` both `[]`,
  every PromQL result empty — reported as `sealed`. Lapilli records a pod's incident window and
  has nothing to record for these; the reconciler refuses the same shape, because a capture can
  also be created by hand.
- `webhook.maxCapturesPerPayload` (default 50): alerts past the cap in one Alertmanager payload
  are counted in `lapilli_alerts_dropped_total{reason="payload-cap"}`, logged with the knob that
  raises them, and dropped. The body limit alone admits about 990 alerts per payload, and the
  measured cost grows with the size of the storm (50 alerts peak at 146.8 MiB against the
  256 MiB limit) with nothing else bounding it; 50 is the largest storm measured end to end, not
  a computed ceiling. `LapilliPayloadCapped` fires when the cap is hit.
  (`docs/design-trigger-and-load.md`, `docs/design-review-round21.md`)
- `lapilli verify` prints a notice when a bundle holds PromQL result files and every one of them
  is an empty series set. Coverage still counts `metrics` as run — the collector did run, and an
  empty result is a true record of what Prometheus answered — so the verdict is unchanged; it
  was the silence about it that was wrong, the same shape as the pod-less capture above. A
  result file too large to peek at counts as holding data, and an unparseable one suppresses the
  notice rather than guessing.

- **A way to verify what you downloaded.** Each CLI tarball and `SHA256SUMS` carries a Sigstore-signed
  build-provenance attestation from the job that produced it, and `README.md` gains a *Verifying what
  you downloaded* section with the commands. A checksum file shipped in the same release as the files
  it describes proves nothing on its own — and a project whose pitch is offline-verifiable evidence
  shipping no way to verify its own downloads was the asymmetry a reviewer would find first. The
  image's BuildKit attestations are described precisely now: unsigned in-toto, not Sigstore-signed
  SLSA.
- **`NOTICE` and `THIRD-PARTY-LICENSES.md`, inside the artifacts.** 277 crates are linked statically
  into every published binary, so their licence text travels with the binary and not only with this
  source tree: the image carries all three files under `/usr/local/share/doc/lapilli/` and each
  tarball carries them at its top level. `object_store` is an Apache Arrow crate shipping a `NOTICE`
  that Apache-2.0 §4(d) requires passing on, and it is the only one of the 338 third-party crates
  that has one — a narrow obligation that `v0.1.0-rc.1` met none of. The listing is generated by
  `cargo about` and carries full licence text, not SPDX ids; `LICENSE`'s appendix names a copyright
  holder, which no file in the repository did before.
- **Every published binary carries its own dependency list.** Built with `cargo auditable`, so the
  resolved graph is in the binary — `cargo audit bin lapilli` or `syft scan file:lapilli` reads it.
  This also fixes the image's SBOM attestation in place: `v0.1.0-rc.1`'s held twelve Debian packages
  and none of the crates, because two static binaries on distroless leave no package database; the
  same scan now reports 377 crates. It is a dependency list, not a licence record.
- **OCI labels on the image** — `title`, `description`, `licenses`, `source`, `url`, `documentation`,
  `vendor` in the `Dockerfile` so an adopter's own build gets them, `revision` and `version` from the
  tag. `docker inspect` on `v0.1.0-rc.1` answered `null`: nothing tied the image to this repository
  or its licence.
- **[`docs/data-handling.md`](docs/data-handling.md)** — the page an operator reads before installing.
  Every category a bundle carries, split three ways: redacted best-effort, never redacted by design,
  and the files beside the bundle on the volume. The design records already conceded that a bundle
  "may hold personal data the redactor missed"; that is about what redaction *missed*, and said
  nothing about what it never visits — labels, `managedFields`, pod and host IPs, `nodeName`,
  `serviceAccountName`, image references, everything in `events.json` but `message`, and every log
  line. Then where copies go, how long they live, and what erasing one incident requires: which files
  to remove and which to leave, because deleting `<incident>.notified` makes a month-old incident get
  announced to Slack as news.
- **`docs/security-scanning.md`** — what a Trivy or grype scan reports against the image, what the
  binaries actually link (`DT_NEEDED`, no `dlopen`, no OpenSSL in the lock), why the bases are
  digest-pinned, and an OpenVEX statement for a finding in a package nothing loads.
- `.github/ISSUE_TEMPLATE/bug_report.yml`, whose first line is "do not attach a real cluster's `.ieb`
  or `postmortem` output", with the reason and the alternative. `.github/dependabot.yml` for
  `github-actions`, `docker` and `cargo` — pins nobody refreshes are worse than no pins.
- **The attribution is gated, and on every pull request.** `scripts/attribution-check.sh` asserts
  the files exist, that `LICENSE` names a holder, that every dependency shipping a `NOTICE` has it
  reproduced in ours, and that the listing covers exactly the crates linked into a published binary —
  and a CI job runs it, because what breaks it is not an edit but the `cargo` Dependabot group moving
  `Cargo.lock`, which at release-time-only checking would have left a licence we stopped
  distributing. It unpacks the registry sources itself and fails, rather than noting, when one is
  missing: on a fresh runner `cargo fetch` leaves only archives, so the skip path would have read
  nothing and gone green. `scripts/release-check.sh` adds the checks that need a built image — the
  copies inside it are byte-identical, the OCI labels are set, both binaries carry their dependency
  list — and drives `lapilli mcp` through the reference MCP client when `npx` is present. Every cargo
  invocation in it now passes `--locked`.
- `charts/lapilli` gains `image.digest` (pin the image instead of trusting a mutable tag) and
  `imagePullSecrets` (a team mirroring the image into a private registry could not install at all).

### Changed

- `CaptureProfile.spec.collectors` defaults to `[logs, resources, events, changes]` — the same
  default the chart writes — instead of `[logs]`. A hand-written profile that omitted the field
  got the thinnest capture there is and one no chart install ever produced. Alpha CRD; a profile
  that wants logs only now says so.

- **Public claims that were not ours to make are gone.** `ROADMAP.md` and the round-29 log recorded
  that an employer's cluster was a candidate adopter *"with the employer's consent"*, and
  `docs/design-kms.md` recorded that the author held that employer's cloud credentials and had
  weighed using them for this project. A public design record is not where either belongs and the
  consent was not ours to publish; the round-29 log carries a correction rather than a silent edit.
  The adoption criterion that stands is the one in `ROADMAP.md`: an incident on a cluster the author
  does not operate.
- **The conduct reporting chain ended nowhere.** `CODE_OF_CONDUCT.md` and `SECURITY.md` both routed
  reports to the maintainers listed in `MAINTAINERS.md`, which had no contact column — and with one
  maintainer, "report to the maintainers" and "report to the person involved" are the same address.
  `conduct@cncf.io` is now named **first**, for exactly that case, and `MAINTAINERS.md` carries a
  contact. `MAINTAINERS.md` also said *Independent* where `GOVERNANCE.md` and `ROADMAP.md` spoke of
  "the founder's employer"; the affiliation is stated precisely and the others ask for a second
  organization.
- **The base images moved to Debian trixie, both pinned by digest.** bookworm carried two Critical
  findings no refresh could clear: `libssl3`'s fix does not exist in Debian 12, and a `libc6` one is
  marked *won't fix*. Neither is reachable from these binaries, which link no OpenSSL at all
  (`docs/security-scanning.md` carries the `DT_NEEDED` evidence and an OpenVEX statement), but a
  Critical costs every adopter a policy exception whether or not it is exploitable: `v0.1.0-rc.1`
  scanned 2 Critical / 6 High, the same source on trixie scans 0 / 3. The two stages must stay on the
  same Debian release, and Dependabot's `docker` updates are what move them.
- **The redaction claims now say what the redaction does.** Five places promised `strict` mode was
  "for a guarantee" while `redact.rs`'s own comment said the opposite. `strict` widens the candidate
  set and nothing more: the scope is the same in both modes, free text stays best-effort, and no mode
  touches container logs, `metrics/`, labels, image references, IP addresses, `nodeName`,
  `serviceAccountName`, `managedFields` or object references. `redaction.json` gains
  `not_redacted_fields` so the bundle says what survived inside the files the policy *does* visit,
  kept separate from `not_redacted`'s path prefixes because the two are different kinds of thing.
  `spec/IEB-SPEC.md`'s redaction table names them instead of saying "everything else", and its
  signing table no longer marks unsigned integrity ✅ against `DESIGN.md` §5's ⚠️ or calls KMS v0.2.
- **`DESIGN.md` §11 argued against the default the same tree ships.** It says the retention default
  cannot be "keep forever"; the shipped default is exactly that, and a PVC-only install reclaims
  nothing but abandoned work even with a bound set. The paragraph now admits it, and the Object Lock
  recommendation says what it costs: in compliance mode with the bucket policy `docs/design-export.md`
  recommends, nobody — including the account root — can delete a bundle until the period expires, so
  an erasure request cannot be honoured while the lock runs. Set the period against the legal
  retention period for what the bundles hold.
- `retention` reclaims a stale `.unsent` notification hand-off instead of keeping it forever. It was
  in the never-list beside the two `O_EXCL` claims and the archived keys, but those have stated
  consequences and this one had only a description: the requirement is *survive a restart*, and the
  list implements *survive the uninstall*. It is the only file under the bundle root that carries
  workload content. Reclaimed 30 minutes past its last write (`notify::COOLDOWN`), refused before
  that even when the volume is over its ceiling.
- `redaction.mode` has no admin floor, and the chart says so rather than leaving it implied: export
  destinations and notify routes are admin-only and signing under `signing.mode=kms` is pinned by the
  chart, but anyone who can patch a `CaptureProfile` can set `off`. Documented as a trust boundary;
  an admin floor is a v0.2 item.
- `spec/IEB-SPEC.md` rule 5: `incident.*` and `trigger.*` are **untrusted text** a consumer MUST
  escape for its sink before rendering — `firing_ts` is not even required to parse. `lapilli postmortem`
  already does it; making it a spec requirement means the next implementer does not rediscover that a
  value able to end a table row can write its own heading.
- `docs/egress.md` has **"Data that leaves by being read"**, and its checklist ends with it: pointing
  an MCP client at a bundle sends `resources/**`, `diffs/**` and — with `mcp.allowLogs` — unredacted
  log lines to that client's **model provider**, and because the read is *inbound* no egress
  allowlist is in that path. The page previously said the mcp container "needs no egress rule
  either", which was true of the code and silent about the data. The same sentence is now at each
  point of decision: the chart's `NOTES.txt` immediately before it hands over the mcp token,
  `integrations/holmesgpt/README.md` (hosted versus self-hosted model), and
  `docs/design-distribution-path.md`.
- `.gitignore` covered `*.ieb` but not the directory `lapilli demo` unpacks beside it, nor
  `*.summary.json`, `*.notified`, `*.ieb.owner`, `*.unsent`, `reclaimed.jsonl` or `.staging-*/`. A
  `git add -A` inside a clone could commit somebody's unredacted operational data. The frozen
  `test/fixtures/ieb/**/*.ieb` exception is intact.
- `README.md`'s "A planted credential in the demo app is checked absent from every bundle file in CI"
  read stronger than what runs: the canary is planted in two places, both name-rule cases, so the
  gate proves the name rule end to end and not the policy. It says that now.
- The chart's `license: Apache-2.0` was silently doing nothing — `license` is not a field of Helm's
  `chart.Metadata`, so the chart published for `v0.1.0-rc.1` carried no licence at all. Now
  `annotations: artifacthub.io/license`, plus `maintainers`, `kubeVersion: ">=1.30.0-0"` (the support
  claim, not the manifests' own floor; the `-0` is required or managed control planes reporting
  `v1.30.5-gke.1234` are refused), a `charts/lapilli/LICENSE` and a `charts/lapilli/README.md`.
- `merge_timeline` re-sorted the whole timeline by raw timestamp and undid `sort_timeline`'s "an event
  with no timestamp goes last" rule, putting an untimed event on the top line — the line a reader
  takes as the start of the incident. The default profile runs both collectors, so that was the
  default path.
- `DESIGN.md` §7 said the webhook token is "read per request"; it is re-read within five seconds, so
  a rotated-away token is accepted for up to that long. The `lapilli mcp` `verify` tool said it
  accepts "an unpacked directory", which it has always refused.
- `docs/metrics.md` said the series "carry no incident content" — true, and reading as "nothing
  sensitive": under `signing.mode=kms` the `key_id` label was the full key ARN on an unauthenticated
  port. The page says what it reveals and what restricts it. `docs/design-review-round10.md` carries
  a dated correction: "a webhook NetworkPolicy can't silently kill scraping" was wrong.

### Security

- `lapilli verify` streams `.ieb` files instead of extracting them, enforces path rules,
  rejects links, duplicate and case-colliding entries, and unlisted files under
  `signature/`, and applies resource limits.
- The signing declaration is part of the signed manifest; authenticity is established only
  with `--key`.

*Round 30 (`docs/design-review-round30.md`) was the last look before the first tag: six lenses over
the whole tree — design conformance, the cluster, supply chain, privacy and law, a non-Claude
security devil's advocate, and machine scanners. Everything in this section came out of it.*

- **Retention removed the staging directory of a capture that was still collecting.** The guard that
  refuses to reclaim work a live capture still holds keyed on the capture uid in the directory name
  — `docs/design-retention.md` spends a paragraph promising it, *"never on age alone: a capture may
  legitimately sit in `Sealing` for days through a KMS outage"* — and parsed that uid as the text
  after the **last** `-`. A Kubernetes uid is a UUID with four of them, so the parse returned its
  last group and the live uid it was compared against was the whole string: the comparison could
  never be true, `Refusal::InFlight` was unreachable, and every sweep classified a live capture's
  staging as abandoned and deleted it — evidence already collected, lost. The unit tests passed
  because their uids were `uidA` and `uidB`, which have no hyphen. Retention is off by default, so
  this reached installs that turned it on. The whole `<incident>-<uid>` tail is matched now, anchored
  on the `-`, and the fix is mutation-proven: with the old comparison restored both regression tests
  fail.
- **The webhook token was worth a read of any pod's logs in the cluster.** A capture names the pod
  whose logs, object body and events are sealed, and the namespace comes from the alert's labels — so
  whoever can POST chooses the target, while `watchNamespaces` was only ever used to decide which
  permissions to *ask* about. A capture outside the watched namespaces is now refused as
  `target-not-watched` with no bundle, there is an E2E step for it, and `DESIGN.md` §7 states what an
  unset list means: the install records every namespace and the token is worth that much.
- **The mcp sidecar held the controller's ServiceAccount token** while `deployment.yaml` described it
  as having "no Kubernetes API access of its own". Nothing set `automountServiceAccountToken`, so the
  default mounted the token into both containers — and the mcp container is reachable from any pod in
  the cluster and spends its time unpacking tars and rendering JSON. The pod now sets it `false` and
  only the controller container mounts a projected service-account volume.
- **The chart granted the controller write access to `CaptureProfiles`** it only ever reads. The write
  verbs would have let a compromised controller set `redaction.mode: off` or point `notify.route` at
  another team's channel — the thing `crd.rs` says editing a profile must not allow.
- **`webhook.networkPolicy` denied all other ingress to the pod**, not just the webhook port: an
  ingress NetworkPolicy is a per-pod allowlist, so the single-rule policy silently cut `/healthz`,
  `/metrics`, the ServiceMonitor and the mcp port, while `values.yaml`, `webhook.rs`, `docs/egress.md`
  and a design-review record all described it as a restriction on one port. Every port the pod serves
  is named now, `webhook.networkPolicy.healthFrom` covers the health port (open by default, because a
  wrong peer list fails readiness and drops the pod out of the webhook Service), and a new
  `mcp.networkPolicy` restricts the port that serves bundle contents — which no option could reach
  before.
- **An export destination's endpoint bypassed `lapilli-net`.** It went into the object-store builder
  with nothing but `allowHttp`, so a chart value could name any host on the internet and receive a
  sealed bundle — logs are never redacted — in plaintext, and `docs/egress.md`'s claim that link-local
  is refused "for every endpoint it parses" was false for the one path that carries evidence out of
  the cluster. Endpoints are parsed now (strict syntax, plaintext only to loopback or a cluster-local
  name, a link-local literal refused), a failure is permanent (`destination-misconfigured`), and the
  two rules that cannot follow are stated rather than implied: `object_store` builds its own client,
  shared with the credential chain, where refusing link-local would break EKS Pod Identity and GKE
  Workload Identity.
- **"Only a bundle Lapilli verified itself leaves the cluster" was a claim about a file name.**
  `upload` verified a path and then re-read it, so a writer on the volume could swap the bytes
  between the two reads. The file is read once and the verified bytes are the uploaded bytes.
- **`lapilli postmortem` interpolated every alert- and workload-chosen string raw into Markdown** —
  the rule, the firing timestamp, the capture window, event reasons, the rollout diff's
  `kind`/`name`/`field`/`before`/`after`, the incident id, the verifier's problem messages. One
  forged alert could end a table row and write its own `## Root cause` into a document that is pasted
  into a wiki and, through `lapilli mcp`, handed to an LLM. Every leaf is escaped now — text values
  for the characters that change structure, code spans with a widened backtick fence, the log line
  and the reproduce command inside a fence longer than any run inside them — and invisible characters
  (control, ESC, U+2028/2029, bidi overrides, BOM) are flattened, the same set `notify.rs` refuses.
  Ordinary pod names and timestamps render exactly as before. `trigger.rule` now also refuses `|` and
  control characters, and `trigger.firingTs` — which had **no** constraint — must be an RFC 3339
  instant of at most 64 characters.
- **The webhook refuses an alert whose `startsAt` is present but unparseable**, counting it in
  `lapilli_alerts_dropped_total{reason="bad-firing-ts"}`. Refused rather than rewritten because the
  manifest is signed: a firing time Lapilli invented over one the alert asserted would be a fact
  nobody claimed, and a dedup bucket taken from `now()` turns every resend into a new capture. A
  **missing** `startsAt` still falls back to the receive time.
- **The controller logged its full KMS key resource name at INFO** in four places, and `logs/` is
  never redacted — so any adopter capturing their own controller pod sealed their AWS account number
  or GCP project id into a bundle. `KmsKey::redacted()` elides the account and project at INFO and
  ERROR; the full name stays at DEBUG and in `status.seal.key`, where an operator reads it
  deliberately. **The fixture that demonstrated this is removed**: `test/fixtures/kms/` held a bundle
  whose `logs/controller-current.log` carried a real project id under the signature, so editing it
  would have broken the only thing the fixture was for. The run stays recorded in
  `docs/design-kms.md`.
- **`lapilli mcp read_file` told an agent a file was redacted when it was not.**
  `redacted_at_capture` was computed from the path alone, so a bundle captured with
  `redaction.mode: off` reported `true` for `resources/pod.json`. The mode now comes from the
  bundle's own record by way of the verify-result document the tool already had in hand, every answer
  carries a `redaction` object (mode, policy version, `best_effort: true`, the bundle's not-redacted
  lists), and `redacted_at_capture` means what it says: the policy ran over this file. An `off`
  bundle is served with the mode stated and the warning `lapilli verify` prints — `summary` and
  `postmortem` render the same objects, so refusing in one tool would keep no unredacted byte out of
  an agent's context — and an absent or unknown mode reads as `off`.
- **Log collection was bounded by lines, not bytes.** A container runtime splits a log entry at
  16 KiB and the kubelet returns each fragment as a line, so one instance could be 32 MiB — times
  the containers, times current and previous, times the reconcile concurrency — against a 256Mi
  limit whose envelope was measured with busybox, where the whole log is one short line. A workload
  could invalidate the published bound and OOM the controller mid-storm, taking every capture in
  flight with it. Tails are bounded at 4 MiB per instance, and a tail the bound cut is recorded in
  `logs/index.json` as `truncated: {limit_bytes, bytes, cut}`.
- **A truncated tail was presented as the crash's last words.** The kubelet spends the byte budget
  forward from the start of the tail window, so a `cut: "newest"` file is missing exactly the lines
  nearest the crash — which the spec now makes a consumer MUST NOT present as last words, and which
  `lapilli verify`'s summary, the Slack notification and `lapilli postmortem` all did. `Summary`
  carries `log_truncated` and leaves `last_line` empty whenever the end nearest the crash is gone, so
  a renderer that has never heard of `truncated` cannot assert it either.
- **The release gate ran with `contents: write` and `packages: write`** — the job that creates a kind
  cluster, pulls images and runs the E2E through three third-party actions — because those were set
  at workflow level and inherited by every job. `id-token: write` and `attestations: write` were
  granted and used by nothing. All three workflows default to `contents: read` now and each job asks
  for what it uses. Every third-party action is pinned to a commit SHA;
  `dtolnay/rust-toolchain@stable` was a force-pushed branch, so it is pinned to a `master` commit
  with the toolchain named in `with:`.
- **`aws-lc-rs` was compiled into the shipped controller** while three comments in the same manifest
  said it was not: `rustls` was declared without `default-features = false`, and rustls 0.23's
  defaults include it, so a C/cmake crypto stack came in alongside ring — 41 `aws-lc` strings and 21
  OpenSSL strings in the published `v0.1.0-rc.1` binary. `deny.toml` bans it, `openssl-sys` and
  `native-tls`, and a `cargo-deny` CI job makes that a gate. `prefer-post-quantum` goes with it: none
  of the peers negotiates a PQ hybrid group, and the bundle's integrity rests on its signature rather
  than on the transport.
- **The image is built with `--locked`**, as is every cargo invocation in CI. Without it the audited
  lock constrained only the release CLI job.
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

- **The permission self-check lost the `list` that makes it accurate.** Tightening the
  `CaptureProfiles` grant to read-only was right, but it was written as `get` alone — and `perms.rs`
  *lists* profiles, which is how the collector checks are narrowed to what the installed profiles
  actually need. Denied, the narrowing stopped silently and an install whose profiles want no logs
  reported `pods/log` as a missing permission. Nothing failed; the self-check just said a permission
  nobody needs was missing, which is the worst thing a self-check can say. The grant is `get, list`
  now, both are in the every-verb table, and the chart assertion was rewritten from "get and nothing
  else" to what it meant: both reads, never a write verb.

- **A handed-over notification was reported as a swallowed repeat right after it was posted.** The
  gap that the hand-off exists for — after a pod deletion the replacement starts before the old
  pod's SIGTERM — also puts one capture in two places inside the **new** process: its own open
  coalescing group, because the capture is newer than this process and so is not history, and the
  predecessor's hand-off. Both reported. The replay posted and wrote `sent`; the phantom group
  closed eight milliseconds later, hit the cooldown the replay had just armed, and patched
  `status.notification` to `repeat`, telling an operator the channel was never told about a message
  it had just received. Consuming a hand-off now withdraws its members from this process's own group,
  inside the dispatcher loop where nothing races, and — behind that — **`sent` is never overwritten
  by an outcome that posted nothing**: `repeat`, `already-notified`, `dropped` and `failed` all mean
  another pass had it. `failed` → `sent`, the upgrade the hand-off exists to make, still happens.
  Both halves are mutation-proven. The guard costs one `get` on the status subresource, which
  `perms.rs` now asks about.
- **A notification group flushed at shutdown is no longer lost when its one POST fails, or when
  the replacement pod claims it first.** Two ways the same message went missing on a pod delete,
  both found through the notify E2E's rollout step (CI, 2026-09-26, kind 1.37; the same commit
  passed the release gate twice). The flush gets one 3 s attempt after the group is claimed, and
  the next process files every pre-start capture as history, so a receiver unreachable for that
  moment lost the message for good with `status.notification` saying `failed`. And after a
  *deletion* (not a Recreate rollout) the replacement starts before the old pod's SIGTERM,
  reconciles the capture, claims it as history, and the flush read that claim as an announcement
  and dropped the group as `already-notified`. Now a failed shutdown flush writes the group beside
  its claim (`<leader>.unsent`, protected from retention) and the next dispatcher posts it — at
  start and every 30 s, consuming the file before the POST so the retry stays once-only — and the
  history gate's claim carries a `history` mark that the predecessor's flush may take over. The
  E2E harness now follows the dying pod's log from before the delete and prints it on failure
  with the capture's status and the receiver's log, and a new step refuses the flush on purpose
  and requires the successor to deliver. `docs/design-notify.md`.
- `lapilli verify` as a library: a `--key` that is not a public key now answers
  CANNOT_EVALUATE with `unreadable`, the code the spec assigns and the CLI already reported
  after its own pre-check; the library path used to fall through to a `signature` FAILED — a
  condition moving between codes (independent review, non-Claude model, 2026-09-25).
- **Every tar entry now counts toward the verifier's 100,000-entry limit.** Directory entries,
  pax and GNU extension records, links and entries with a non-UTF-8 name were skipped before the
  counter, and each cost a problem string and a loop iteration; identical 512-byte headers
  compress by three orders of magnitude, so a `.ieb` under the 2 GiB compressed-byte limit could
  carry millions of them (120,000 pax records: 120,001 problem lines, the limit never applied).
  The count is now taken first, for every entry, and the over-limit path is CANNOT_EVALUATE
  (`limit`) as it is for files (`verify::read_ieb_from`). Found by an independent review
  (non-Claude model), 2026-09-25, findings 2 and 6.
- **Directory entries now obey the reserved-name and case rules.** `Signature/` (any case) as an
  empty directory entry, and `Logs/` beside `logs/`, passed, because only file entries were
  checked; both are now FAILED with the `structure` code and the message the file case uses
  (`verify::Contents::add_dir`, and its mirror in `add_hashed`). A `signature/` directory entry
  spelled as reserved stays legitimate. Found by an independent review (non-Claude model),
  2026-09-25, findings 5 and 9.
- **`lapilli unpack` no longer honours the archive's modes.** A directory entry with mode 0644,
  or a file with mode 0000, unpacked as such, and the unpacked directory then verified exit 3
  ("Permission denied") while the same bytes as a `.ieb` verified OK; `lapilli mcp` stages with
  `unpack` after `verify` said OK, so its file reads hit the same denial. Files are now 0644 and
  directories 0755 whatever the header says, and fifo, device and sparse entries are refused as
  links already were (`pack::unpack`). Found in the triage of the independent review
  (non-Claude model), 2026-09-25.
- **Verifying an unpacked directory no longer reads each file into memory.** `verify_bundle_dir`
  hashed with `fs::read`, so a 256 MiB log cost 256 MiB where the `.ieb` path streamed.
  Directory files, and the producer's own hashing when sealing, now stream through a 64 KiB
  buffer, reading at most the size already charged against the limits (`verify::walk`,
  `hashtree::sha256_file`). Same verdicts and problems; a PromQL result over 64 KiB is now
  judged the same way in both modes. Found by an independent review (non-Claude model),
  2026-09-25, finding 1.
- **Spec wording made to agree with the code and the fixtures** (`spec/IEB-SPEC.md`; no rule
  changed). Rule 1 said paths fit in "≤ 255 bytes" where ustar `prefix` + `/` + `name` holds
  256 and the verifier, `check_path` and `ok-long-path.ieb` accept 256 (the error text said 255
  too; both now say 256). Rule 1 now says that bytes after the tar end-of-archive marker do not
  affect the verdict but are part of `input.sha256`, which names the object, not the archive
  inside it. Rule 8 now says the `cosign.pub` self-consistency check is defined only for a
  known `alg`, and `lapilli verify` reports a `notice` when it skips it (`verify::v1`). Raised
  by an independent review (non-Claude model), 2026-09-25, findings 3, 4 and 7.
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
- **A denied log read read as full coverage.** A `pods/log` fetch error — a 403 because the Role
  lacks `pods/log`, an API server error, a timeout — was written into `logs/index.json` as
  `unavailable` and the `logs` collector still counted as run, so a bundle with **no log bytes**
  verified `OK coverage=100%`. IEB-SPEC rule 6 already forbade this ("a producer that records an
  error there MUST leave that collector out of `collectors_run`"). Any fetch error now fails the
  `logs` collector and the bundle is PARTIAL; only the kubelet's in-band one-line error (HTTP 200,
  the instance was garbage-collected) stays soft, because it is a fact about the workload. The
  denial itself is still written to `logs/index.json` before the collector fails, so it is sealed
  and hashed.
- A capture created in the same second the controller started was filed as history and never
  announced. Kubernetes stores `creationTimestamp` at one-second resolution and the replay guard
  compared it against a sub-second `started_at`, so a capture created 0.7 s *after* the process
  started read as older than it, was claimed so no relist would reconsider it, and was silently
  never notified. That is a one-second window after every controller start, reachable whenever
  a controller rolls while an incident is firing, and it was the notify E2E flake this project
  had failed to explain twice. The guard now truncates `started_at` to the resolution of the
  value it judges.

### Migration

- Development builds before v0.1.0 wrote `lapilli.dev/ieb/v0` bundles, which no release
  reads (`lapilli verify` exits 3).

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
  `kubectl apply --server-side --force-conflicts` (see `docs/COMPATIBILITY.md` §3). The CRD
  has changed in three more ways since, all in `CaptureProfile`: `spec.deferred` (new, with
  `coverage.deferred` in every bundle), a CEL rule that refuses a name listed in both `deferred`
  and `collectors`, and a `collectors` default of `[logs, resources, events, changes]` instead
  of `[logs]`. A profile that relies on the new default or on `deferred` is only honoured once
  the CRD has been re-applied.
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
- An install whose Role lacks `pods/log` now produces PARTIAL bundles (it used to produce OK
  bundles with no logs). Grant `pods/log` on the namespaces Alertmanager can name, or, if logs
  are kept elsewhere, list `logs` in `profile.deferred` so the bundle says so instead.
- A storm larger than `webhook.maxCapturesPerPayload` (50) alerts in one payload is truncated,
  and the excess is counted rather than captured. Raise the value only with the memory to match:
  `docs/design-trigger-and-load.md` §3 has the measurements the default rests on.
- The Kairn → Lapilli rename changed every identifier the old name lived in: the bundle
  `schema_version` (`kairn.dev/ieb/v1` → `lapilli.dev/ieb/v1`), the verify-result schema
  (`kairn.dev/verify-result/v1` → `lapilli.dev/verify-result/v1`), the CRD API group
  (`kairn.dev` → `lapilli.dev`) and the bundle root (`/var/lib/kairn/bundles` →
  `/var/lib/lapilli/bundles`). A bundle written by a pre-rename development build now fails as
  `not-a-bundle` (exit 1): its `schema_version` no longer carries a Lapilli prefix. No release
  ever wrote one; the fixtures were regenerated.
