# Lapilli

> **A flight recorder for Kubernetes incidents.** The moment an alert fires, Lapilli captures
> the full incident window — events, owner-chain YAML, the logs from the container that
> *just died*, the metric shape, and what recently changed — into **one portable file you
> own**. Stop reconstructing timelines from memory and screenshots.

[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
![Status](https://img.shields.io/badge/status-pre--alpha-orange.svg)
![Language](https://img.shields.io/badge/built%20with-Rust-000000.svg)

When you get paged at 3 a.m., the logs, events, and "what changed" you need are already
rotating away. Lapilli is always watching: the instant a Prometheus/Alertmanager alert fires,
it snapshots the incident window into a self-contained **Incident Evidence Bundle (IEB)** —
a portable file you own, that any tool can read and no vendor can hold hostage.

And because the record is captured automatically the moment it matters, it also serves as
**audit-supporting** evidence you used to assemble by hand — a bonus for regulated users,
not the pitch (see [why below](#the-honest-pitch)).

## The problem

- **The evidence horizon.** By the time a human logs in, Kubernetes events (≈1h TTL), the
  crashed container's logs, and the pre-incident metric shape are gone. Manual `must-gather`
  tools can't help — nobody's awake to run them in time.
- **Timeline archaeology.** Post-incident reviews are rebuilt from memory, Slack scrollback,
  and screenshots. There's no single artifact that *is* the incident.

## What Lapilli is not

- ❌ another dashboard (Grafana, Komodor, Coroot)
- ❌ a metrics/logs store — it *reads* from Prometheus/K8s, never replaces them
- ❌ an AI auto-RCA narrative (HolmesGPT, k8sgpt) — Lapilli produces the evidence they *consume*
- ❌ a manual diagnostic collector (troubleshoot.sh, must-gather)
- ❌ a security syscall/memory dump (Falco Talon + CRIU, Sysdig captures)

Stated positively: Lapilli is triggered by operational signals, correlates Kubernetes-native
state across the incident window, seals it as an open, portable, offline-verifiable file, and
verifies and reads that file itself (`lapilli verify`, `lapilli postmortem`, `lapilli mcp`).
It depends on no observability vendor and on no AI tool. Other tools may consume the bundle;
none of them is what Lapilli is for.

## The honest pitch

"We sign it" and "we capture automatically" are **not** novel — both ship today (Talon+CRIU,
Sysdig captures, Kosli, troubleshoot.sh + cosign). Lapilli's edge isn't an architectural moat;
it's the one combination nobody offers as a single **open, operational** tool —

> triggered by **operational/reliability** signals · captured **at alert-time-plus-seconds,
> before the volatile evidence finishes rotating** · merged into **one portable file you
> own**, vendor-neutral, that any tool can read.

Vendor-neutral, portable incident evidence any tool can produce and consume is shared
infrastructure — that's the why-CNCF, and it holds without claiming "standard" today.

Full landscape and the twenty-seven adversarial review rounds that shaped this: [`DESIGN.md`](DESIGN.md)
and [`docs/design-review-round1.md`](docs/design-review-round1.md) through
[`round27`](docs/design-review-round27.md).

## Integrity, stated honestly

**Signing is optional and off by default.** The load-bearing integrity feature is
`lapilli verify` — it recomputes the bundle's hash tree, checks the bound incident context
(failing closed on mismatch), and flags partial captures, with or without a signature.

What an **unsigned** bundle does and does not give you, stated so the tick in `DESIGN.md`'s
table is not over-read: the hash tree catches *accidental* change — a truncated transfer, a
careless edit — because `manifest.json` holds the tree's root and lives inside the bundle. It
catches nothing against someone who can rewrite the file, since they recompute the tree and
rewrite the manifest too. Per `spec/IEB-SPEC.md`: **"Without `--key`, a bundle proves nothing
against anyone who could write to it."** When
you *do* enable signing (a cloud KMS key, `--set signing.mode=kms`, or a static key from
`lapilli keygen` with `--set signing.mode=static`; cosign-compatible either way) and verify with the public key you hold
(`lapilli verify --key lapilli.pub`), you get **integrity after sealing** + **producer
authenticity**; in that config sealing time is self-asserted (an
independent time anchor and transparency log are later, opt-in additions). Lapilli does **not**
claim the contents are a complete, faithful representation of cluster state — no signature
can. It is **one link** in a chain of custody the deploying org completes with WORM storage,
key custody, and access logging. See [`DESIGN.md` §5](DESIGN.md).

## Quickstart: see it capture an incident

You need `kind`, `kubectl`, `helm`, Docker, and a Rust toolchain. Until the first tagged
release publishes the image and chart, build the image locally:

```sh
kind create cluster --name lapilli
docker build -t lapilli-controller:dev . && kind load docker-image lapilli-controller:dev --name lapilli
helm install lapilli charts/lapilli -n lapilli-system --create-namespace \
  --set image.repository=lapilli-controller --set image.tag=dev --set clusterId=kind-lapilli --wait

cargo install --path crates/lapilli-cli     # the `lapilli` CLI
lapilli demo                                # bad rollout -> CrashLoopBackOff
lapilli demo --scenario oomkill             # bad rollout -> OOMKilled
```

After a release, installing is one line:
`helm install lapilli oci://ghcr.io/lapilli-project/charts/lapilli -n lapilli-system --create-namespace --set clusterId=<name>`.
The chart defaults to bundles on a PVC (kept on `helm uninstall`), signing off, and
read-only collector RBAC; `watchNamespaces` narrows that RBAC to a list of namespaces. See
[`charts/lapilli/values.yaml`](charts/lapilli/values.yaml).

`lapilli demo` deploys a healthy `checkout` app, then rolls out a v2 whose only change is one
config env var. Once the new pod has crashed, it fires an Alertmanager-shaped alert, waits
for Lapilli to seal the capture, pulls the `.ieb` out of the cluster, verifies it offline, and
prints what the bundle kept, read from the file rather than the cluster:

```
  ✓ pod checkout-d7bc7f788-kmx4w crashed (OOMKilled); its logs are now one kubelet GC away from gone
  ✓ fired KubeContainerOOMKilled → IncidentCapture ic-35cd8d53f9150625
  ✓ capture sealed and exported
  ✓ lapilli verify ./kind-lapilli-35cd8d53f9150625.ieb --cluster kind-lapilli --incident kind-lapilli-35cd8d53f9150625
      OK  hash_ok=true context_ok=true coverage=100% unsigned  (format v1, produced by lapilli 0.1.0)

What this bundle kept that the cluster was about to lose:

  last words of the crashed instance (logs/app-previous.log):
    │ [checkout] cache pages loaded: 58 MiB
    │ [checkout] cache pages loaded: 59 MiB
    │ [checkout] cache pages loaded: 60 MiB
    │ [checkout] cache pages loaded: 61 MiB
    │ [checkout] cache pages loaded: 62 MiB

  how it died (resources/pod.json):  OOMKilled (exit 137) at 2026-09-19T03:51:00Z, restartCount=1
  what changed (diffs/):             Deployment/checkout revision 1 → 2, 2s before the alert, by demo-deployer
    │ containers[name=app].env[name=CACHE_WARMUP].value: lazy → eager
  memory (metrics/):                 ▁▁▁▄▄▆▆▆ peak 44.1 MiB of 64 MiB limit (8 samples, 5s step)
  timeline (timeline.json):          4 events — Scheduled → Pulled → Created → Started
```

The `memory` line appears when Prometheus is connected (`--set metrics.prometheusUrl=...`).
The curve stops short of the limit because the last seconds before the kill fall between
scrapes, and the bundle shows exactly what Prometheus had.

The bundle lands in `./<incident-id>.ieb`, unpacked next to it in `./<incident-id>/`.
The same command is the project's kind E2E harness (`test/e2e/run.sh`).

## Verifying what you downloaded

A project whose whole claim is offline-verifiable evidence should not ask you to take its own
release on trust. Here is what each published artifact carries and the command that checks it.

**The CLI tarballs and `SHA256SUMS`.** Each one has a Sigstore-signed build-provenance attestation,
generated by the job that produced the file and stored on this repository. That is the check worth
running first: `SHA256SUMS` sits in the same release as the tarballs it describes, so on its own it
proves nothing about where either came from.

```sh
gh attestation verify lapilli-v0.1.0-aarch64-apple-darwin.tar.gz -R lapilli-project/lapilli
gh attestation verify SHA256SUMS -R lapilli-project/lapilli
sha256sum -c SHA256SUMS      # once SHA256SUMS itself is attested, this covers the other tarballs
```

A pass tells you the file was built by `release.yml` in this repository, from a named commit and
tag. It does not tell you the code at that commit is good — only that nothing was swapped in
afterwards.

**The container image.** It carries BuildKit's SBOM and provenance, which are **unsigned in-toto
attestations** in the OCI index — not Sigstore-signed SLSA provenance, and nothing verifies a
signature over them. Read honestly, they tell you how the image says it was built:

```sh
docker buildx imagetools inspect ghcr.io/lapilli-project/lapilli-controller:0.1.0 \
  --format '{{ json .Provenance }}'
docker buildx imagetools inspect ghcr.io/lapilli-project/lapilli-controller:0.1.0 \
  --format '{{ json .SBOM }}'
```

What you *can* pin without trusting any of that is the digest: the release notes give the manifest
list's digest, and the chart pulls the image by digest. Signing the image attestations with Sigstore
is on the roadmap, and is not done yet.

**Scanner findings** against the image, what the binaries actually link, and why the base images are
digest-pinned to Debian trixie are in [`docs/security-scanning.md`](docs/security-scanning.md).

**Bundles** are a separate question from all of the above, and the one this project exists for:
`lapilli verify` (§*Integrity, stated honestly*) is what checks a `.ieb`, and it needs no network
and no trust in GitHub.

## Status & roadmap

Pre-alpha. The **v0.1 walking skeleton works end to end on a kind cluster**: an
Alertmanager webhook creates an `IncidentCapture`, the controller collects the incident
window, seals it into a portable `.ieb` file, and `lapilli verify` checks it — proven in CI.

**Built (ships in `v0.1.0`)**
- Alertmanager webhook → `IncidentCapture` / `CaptureProfile` CRDs → reconcile phase machine.
- Collectors: previous-container **logs**, **resources** (Pod→ReplicaSet→Deployment owner
  chain), **events** (+ normalized `timeline.json`), **changes** (change indicators), and
  optional **metrics** (PromQL range snapshots that reach back *before* the alert; set
  `metrics.prometheusUrl` on the chart).
- **Perishable profile** (opt-in; the full recorder is the default, `deferred: []`):
  `CaptureProfile.spec.deferred` (chart `profile.deferred`) names collectors a profile
  deliberately does not run because the data is kept elsewhere. The API server refuses a name
  that is also in `collectors` (a CEL rule on the CRD). The bundle records `coverage.deferred`
  (`spec/IEB-SPEC.md` rule 6), `lapilli verify` prints `(deferred: …)` on the verdict line, and
  a deferred collector does not make the verdict `PARTIAL`. Covered by the kind E2E
  (`test/e2e/deferred.sh`).
- Sealing: content hash tree + `manifest.json` (bound incident context, `incident.target
  {namespace, pod}` as an index for readers that `verify` does not compare, coverage score and
  `coverage.deferred`), packed into a single portable **`.ieb`** file (tar + zstd).
- Optional signing, cosign-compatible DER with openssl conformance in CI:
  - **AWS KMS or GCP Cloud KMS** (`signing.mode=kms`, [`docs/kms.md`](docs/kms.md)): the
    key never enters the cluster; captures wait in `Sealing` through a KMS outage, never
    unsigned; `lapilli key fetch --kms` gets the public key from the KMS.
  - **Static key** (`lapilli keygen`).
  - Either way, `lapilli verify --key` makes authenticity rest on a key you pin, never on the
    one inside the bundle.
  - Verified against the real **GCP Cloud KMS** once (a bundle it signed is a fixture under
    `test/fixtures/kms/`); **AWS KMS is verified against LocalStack only** so far — the first
    AWS adopter is the real test, and the docs say so. Both clouds run in the kind E2E and the
    emulator tests on every change.
- **Spec diffs** (`diffs/`): what changed in each rollout inside the window, from what to
  what, when relative to the alert, and by which field manager. Read from the revision
  history Kubernetes already keeps (Deployment, StatefulSet, DaemonSet), plus an opt-in
  key-level diff of ConfigMaps whose referenced name changed. Rollback, scale-from-zero,
  paused, Recreate and ConfigMap-rename cases are covered by the kind E2E.
- **Object-store export** (S3, S3-compatible, GCS) to destinations the admin defines:
  conditional create with a verified checksum, never overwriting, retried, visible per
  capture; demo captures stay local. Tested against LocalStack's S3 with Object Lock; not yet
  against a real bucket.
- **Redaction** at capture time (env values, args, probe headers, annotations, event
  messages; best-effort, with a `strict` mode), recorded in `redaction.json`. What it does **not**
  touch in any mode — labels, pod and node IPs, `nodeName`, `serviceAccountName`, `managedFields`
  and every container log line — plus where copies of a bundle go and how long they live, is in
  [`docs/data-handling.md`](docs/data-handling.md): read that before you install.
  The kind E2E greps one planted canary out of every file of both demo bundles. It is planted in
  two places in the demo app (a `DB_PASSWORD` env value and a `-Dspring.datasource.password=`
  argument), so that gate proves the **name rule** end to end, not the policy as a whole; the value
  rule, the URL rules and the config-line rules are covered by test vectors in `redact.rs`.
- `lapilli verify` — recompute hashes, fail-closed context check, `PARTIAL` coverage; accepts
  a `.ieb` file, a directory, or an object in a bucket (`s3://`, `gs://`, presigned
  `https://`), streamed without being stored. For bucket evidence it also checks that the
  key was written only once (S3 version history) and that the key names the bundle's
  cluster and incident; `--expect-sha256` / `--version-id` pin the values the controller
  recorded in `status.exports`. Credentials come from the environment; for an AWS profile
  or SSO, run `eval "$(aws configure export-credentials --format env)"` first.
  `--output json` writes one `lapilli.dev/verify-result/v1` document for every outcome,
  stable from `v0.1.0` ([`spec/VERIFY-RESULT.md`](spec/VERIFY-RESULT.md)).
- `lapilli postmortem <bundle|dir>` — a Markdown draft that transcribes only values that exist
  in the bundle, each with the file it came from; Impact, Root cause, Contributing factors and
  Action items are emitted as empty headings. It verifies first: `OK` and `PARTIAL` render,
  `FAILED` still renders behind a banner that names which failure it was and exits 1,
  `CANNOT_EVALUATE` refuses with exit 3. The crashed container's last log line is off by
  default (`--include-log-line`). Local bundle or directory only.
  [`docs/design-postmortem.md`](docs/design-postmortem.md).
- `lapilli mcp` — Lapilli's own reader of its evidence, served over MCP to any client. Five
  tools: `find_bundles`, `verify`, `read_file`, `summary`, `postmortem`. Runs on stdio next to
  pulled bundles, or with `--http` as a second container in the controller pod, where the
  bundles are (chart `mcp.enabled`; port 8082; Service `<release>-mcp`; a bearer token in front
  of every request; the PVC mounted read-only). It serves `.ieb` files only, files inside a
  bundle only by the name the verified hash tree lists, and refuses `logs/**` unless
  `mcp.allowLogs`. Any MCP client is a consumer; `integrations/holmesgpt/` is one worked
  example. [`docs/design-distribution-path.md`](docs/design-distribution-path.md).
- **Incident notification** — when a capture is sealed, a one-screen summary goes where the
  team already looks (Slack, or a generic JSON webhook): what kind of failure it was, the
  memory peak against the limit, whether the crashed container's last log survived, what the
  last rollout changed and when and by whom, and the command that retrieves the bundle.
  **One message per incident, not per pod**, so a bad rollout across 50 replicas is one
  message. No workload content by default, and never a log line. Admin-defined routes;
  profiles may only name one. [`docs/design-notify.md`](docs/design-notify.md).
- Helm chart: PVC-backed bundles, single-namespace-capable RBAC, signing-key Secret access
  scoped to that one Secret. The egress allowlist the controller needs, and how to enforce it on
  your CNI, is in [`docs/egress.md`](docs/egress.md).
- `lapilli demo` — a synthetic bad rollout (crash loop or OOMKill) walked to a verified `.ieb`;
  doubles as the kind E2E harness.
- **Bounded local retention** (off by default): a sweep reclaims sealed bundles only once every
  destination is observed as `Uploaded`, never touches the two `O_EXCL` claim files or an
  archived signing key, and journals every reclaim to the volume
  ([`docs/design-retention.md`](docs/design-retention.md)).
- **Bounded under an alert storm**: at most `webhook.maxCapturesPerPayload` (50) captures per
  Alertmanager payload; alerts past the cap, and alerts that carry no `pod` label, are counted
  in `lapilli_alerts_dropped_total` and dropped; `reconcileConcurrency` is 2; a capture whose
  exports and notification have settled is retired from the informer (`lapilli.dev/retired`)
  so the watch cache holds open work, not history
  ([`docs/design-capture-retirement.md`](docs/design-capture-retirement.md),
  [`docs/design-trigger-and-load.md`](docs/design-trigger-and-load.md)).
- **Permission self-check** at startup and every 10 minutes — every verb the code issues, asked
  through `SelfSubjectAccessReview`, so a missing RBAC rule is a metric and a log line instead
  of a failed capture at 3 a.m. The needs are re-derived on every pass from the union of every
  `CaptureProfile` in the namespace; a check no profile needs is reported `not_needed`; a
  collector that did not run because its check was denied gets a `CollectorDenied` Event on
  the capture ([`docs/design-permissions-by-profile.md`](docs/design-permissions-by-profile.md)).
- **Metrics** on `/metrics` — 41 documented series (captures by outcome, partial and deferred
  captures, seal and export attempts, webhook outcomes and dropped alerts, captures watched,
  retired and exported-but-unretired, API-server reachability, permission checks asked, denied
  and unknown, the bundle volume's free and used bytes, retention sweeps and reclaims, the
  pinned signing key id), with 26 alert rules in [`docs/metrics.md`](docs/metrics.md) that the
  release gate **executes** under promtool rather than only printing.
- CI: fmt · clippy · tests · signing conformance (openssl, not a moving cosign CLI) · a bundle
  built from the spec alone · frozen-fixture verdicts · CRD-drift · KMS emulators · chart render
  and schema-refusal checks · **kind E2E** (demo scenarios, change diffs, object-store export,
  KMS outage and restart, notification, retention, the perishable profile, `lapilli mcp` in
  the controller pod, plus tamper, wrong-context and admission-refusal negative checks). CI
  (`ci.yml`) runs the E2E on Kubernetes 1.37; the release gate (`release-gate.yml`,
  `scripts/release-check.sh --e2e`) runs the same suites on 1.30 and 1.37.

**Next: `v0.1.0`, tagged and public**
- Everything listed under *Built* ships in the first tagged release, `v0.1.0`: the image (with
  BuildKit's SBOM and provenance — unsigned in-toto attestations, not Sigstore-signed SLSA), the
  OCI chart, and CLI binaries with `SHA256SUMS` plus a Sigstore-signed build-provenance attestation
  per tarball. What to run against each is in §*Verifying what you downloaded*. There was never
  a v0.1/v0.2 split as releases; no tag exists yet.
- What is left before the tag, and what comes after it (v0.2 product items driven by the first
  adopters, then the v0.3 trust additions such as keyless + Rekor and an RFC 3161 TSA, which
  stay opt-in) is in [`ROADMAP.md`](ROADMAP.md). This list is not repeated here.

See [`DESIGN.md`](DESIGN.md) for the full plan and the twenty-seven design-review rounds under
[`docs/`](docs/).

## Compatibility

Bundles are meant to outlive the cluster that produced them. What stays stable across
releases (the `ieb/v1` bundle format, `lapilli verify` exit codes) and what is still alpha
(CRDs, chart values) is in [`docs/COMPATIBILITY.md`](docs/COMPATIBILITY.md); changes are
listed in [`CHANGELOG.md`](CHANGELOG.md).

## Contributing

Apache-2.0. All commits require a [DCO](https://developercertificate.org/) `Signed-off-by`
line (`git commit -s`). See [`CONTRIBUTING.md`](CONTRIBUTING.md) and [`GOVERNANCE.md`](GOVERNANCE.md).

## License

[Apache License 2.0](LICENSE).
