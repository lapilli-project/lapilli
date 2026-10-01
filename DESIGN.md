# Lapilli — Design & Architecture

> **Lapilli is a flight recorder for Kubernetes incidents.** The moment an alert fires, it
> captures the full incident window — the events, the owner-chain YAML, the logs from the
> container that *just died*, the metric shape, and what recently changed — into **one
> portable file**. You stop reconstructing timelines from memory and screenshots.

> **Identity (the sentence everything else is checked against).** Lapilli is *triggered by
> operational signals*, *correlates Kubernetes-native state across the incident window*, and
> *seals it as an open, portable, offline-verifiable evidence file* — then **verifies and reads
> that file itself** (`lapilli verify`, `lapilli postmortem`, `lapilli mcp`), depending on no
> observability vendor and no AI tool. Round 1 named this seam; Path C (round 2) only said
> which *words* to earn before using. Everything since is checked against it: the perishable
> profile is opt-in and the full recorder is the default; the backfill half of round 24 was
> returned to premise because it would have made Lapilli a client of other stores; and the
> MCP server is Lapilli's own way of answering questions about its evidence — any client,
> HolmesGPT among them, is a consumer, never the reason. A market or strategy round that
> would redefine this sentence, rather than the pitch around it, is out of scope for that
> round (`docs/design-review-round27.md`).

Status: `pre-alpha` — v0.1 walking skeleton works end to end on kind (proven in CI) ·
Language: Rust · TAG fit (Incubation review): **Operational Resilience** · Deliverable: an
operational incident recorder + a portable reference bundle layout.

> **How this doc was hardened.** Twenty-seven adversarial review rounds have run
> ([`docs/design-review-round1.md`](docs/design-review-round1.md) through
> [`round27`](docs/design-review-round27.md)), and the later ones shaped this document as much as
> the first two: round 16 returned the event trigger to premise, round 17 rebuilt the retention
> design before any code existed, round 19 rejected a pre-redaction commitment scheme, round 21
> measured the controller under an alert storm and bounded it, round 22 retired finished
> captures from its memory, rounds 23–25 returned the backfill idea to premise and shipped the
> perishable profile with permissions that follow it, round 26 built `lapilli mcp`, and round 27
> put the identity sentence above at the head of everything. Round 2 chose **Path C**:
> ship the flight recorder now; treat signing, audit, and format-standardization as
> *optional / earned-later*, not as the pitch. This doc reflects that.

---

## 1. Problem

Incident context is ephemeral, and by the time a human looks, it's gone.

1. **The evidence horizon.** By the time you're paged and logged in, the volatile evidence
   — Kubernetes events (≈1h TTL, coalesced), the crashed container's logs, the pre-incident
   metric shape — has already rotated away. Manual `must-gather` tools can't help: nobody's
   awake at 02:14 to run them in time.
2. **Timeline archaeology.** Post-incident reviews are rebuilt from memory, Slack
   scrollback, and screenshots. There's no single artifact that *is* the incident.

Lapilli captures the window **automatically, at the moment it matters**, into a **portable
bundle you own**.

## 2. What Lapilli is — and is not

Lapilli watches for a **trigger** (v0.1: a Prometheus/Alertmanager alert). On trigger it
captures a **time-window-correlated** snapshot and writes a portable **Incident Evidence
Bundle (IEB)** to durable storage.

### Non-goals (scope discipline is a feature)

- ❌ Not another dashboard or live UI (Grafana, Komodor, Coroot).
- ❌ Not a metrics/logs/traces store — Lapilli *reads* from Prometheus/K8s, never replaces them.
- ❌ Not an AI "auto-RCA" narrative (HolmesGPT, k8sgpt, Robusta) — Lapilli produces a bundle
  those tools can *consume*.
- ❌ Not a manually-invoked diagnostic collector (troubleshoot.sh, must-gather, Crashd).
- ❌ Not a security-DFIR syscall/memory capture (Falco Talon + CRIU, Sysdig captures) —
  Lapilli captures *operational K8s state across a window*, not a single-container dump.
- ❌ Not a compliance product, not a SIEM, not a long-term log warehouse.

## 3. The honest differentiation (no overclaim)

"We sign it" and "we capture automatically" are **not** novel — both ship today (Falco
Talon + CRIU, Sysdig captures, Kosli, troubleshoot.sh + `cosign verify-blob`; a Rust+Go
evidence operator, Sidereal, exists too). See
[`docs/design-review-round1.md`](docs/design-review-round1.md) for the full teardown.

Lapilli's edge is **not an architectural moat**, and we don't pretend otherwise. The honest,
currently-unoccupied combination is:

> triggered by **operational/reliability** signals (not security detections) · captured
> **at alert-time-plus-seconds**, while the previous-container logs and un-coalesced events that
> outlive the alert are still readable and days before a postmortem would look for them · merged
> into **one portable file you own**, vendor-neutral, that any tool can read and that outlives any
> cluster or platform.

**The boundary that phrasing now carries, measured rather than assumed.** An operational signal has a
`for:` delay — fifteen minutes on the standard kube-prometheus-stack rules — so evidence destroyed
inside that window is unreachable to an alert-triggered recorder *by construction*, and no target
shape or collector changes it. Measured on kind: a CronJob's failed pod and its logs are gone one
schedule interval after the failure, which for anything running more often than every fifteen minutes
is before `KubeJobFailed` fires at all. Three rounds (31, 32, 33) argued about which rules to accept
before anyone measured whether the evidence was still there.
[`docs/design-trigger-reachability.md`](docs/design-trigger-reachability.md) is that measurement, and
it leaves round 16's candidate 3 — a trigger on Pod status rather than on an alert — open as the only
route to the fast-perishing class.

Stated more narrowly after round 29, which checked this against what a team already has: the
API audit log at `RequestResponse` level *does* hold the object as persisted (GKE records
create/update/delete that way by default), and Sloop records resource history — so "the object
as it was" alone is not the edge. What no existing thing gives is **the object *and* its
status, the previous container's log and the rollout diff, sealed together at the alert into
one file, with no audit pipeline to have set up first and nothing to reassemble**. That is the
sentence to pitch, and the team it fits best is the one without an audit-log pipeline.

Two different comparisons, two different wins:
- **vs open tools** (must-gather, troubleshoot.sh): they're **manual and arrive after the
  evidence horizon**; Lapilli is automatic and captures at t+seconds, so the volatile evidence
  still exists. This is **timing + completeness**, not a claim of deep "correlation" — v0.1's
  `timeline.json` is an ordered merge across sources, and `diffs/` says what changed and when
  relative to the alert; causal links beyond that are not promised.
- **vs SaaS incumbents** (Elastic, Wiz): the win is **portability and ownership** — your
  evidence is a vendor-neutral file, not a row in someone's platform.

**Why this belongs in a neutral foundation (the why-CNCF):** vendor-neutral, portable
incident evidence that *any* tool in the cloud-native ecosystem can produce and consume is
shared infrastructure — exactly the neutral ground CNCF exists to hold. This holds without
claiming "standard" today.

Honest caveat, stated openly: a vendor-backed tool already in this neighborhood (Red Hat's
event-driven-diagnostic-operator) is a couple of features from this combination. Lapilli wins
by being the *open, portable, operational-domain* recorder a community adopts — not by
holding a technical secret.

### Landscape (cited proactively)

| Tool | Lang | CNCF | Op-trigger | Window | Portable | Not a duplicate because |
|---|---|---|:--:|:--:|:--:|---|
| troubleshoot.sh support-bundle | Go | — | ❌ | ❌ | ✅ | **manual**; no window correlation |
| Falco Talon + CRIU | Go | Ecosystem | ❌(sec) | ⚠️ | ✅ | security syscall/mem dump, single container |
| Sysdig / Falco captures | C++/Go | Ecosystem | ❌(sec) | ✅ | ✅ | syscall stream only, not K8s object/log/metric |
| Kosli | — | — | ❌ | ❌ | ✅ | continuous provenance, not incident-window |
| Sidereal | Rust+Go | — | ❌(6h) | ❌ | ✅ | scheduled posture, not incident capture |
| salesforce/sloop | Go | — | ❌ | ⚠️(history) | ❌ | records resource state history in its own store for a UI; no trigger, no seal, no file — the object *as it was* without the log, the diff or the status at the alert |
| HolmesGPT (Robusta) | Python | Sandbox (2025-10) | ❌ | ❌ | ❌ | an AI investigator that *reads* live sources through toolsets; produces no evidence file. A consumer of `.ieb` (round 26/27), and the project the TOC will compare Lapilli to — same TAG |
| HolmesGPT / k8sgpt | Py/Go | Sandbox | ⚠️ | ❌ | ❌ | ephemeral RCA narrative; can *consume* an IEB |
| Robusta | Py | — | ✅ | ⚠️ | ❌ | tracks changes and routes enrichment to chat; no portable artifact |
| RH event-driven-diagnostic-operator | Go | — | ✅ | ⚠️ | ⚠️ | Go/OpenShift, no window/portable format |

## 4. The Incident Evidence Bundle (IEB)

A single portable `.ieb` archive (tar + zstd) for one incident. The layout is documented in
[`spec/IEB-SPEC.md`](spec/IEB-SPEC.md) as a **reference bundle layout** (not, yet, a
"standard" — that word is earned only when an independent producer or consumer adopts it).

- **`manifest.json`** — schema version; the bound incident identity tuple
  `{incident-id (unique), cluster-id, trigger rule + firing timestamp, capture window
  [t-Δ, t+Δ]}`; Lapilli version + self-reported running image digest; a SHA-256 hash tree over
  every file (**contents hashed individually**, entries sorted by path); a **coverage score**
  (which collectors ran, % of intended set, and which were **deferred** on purpose by a
  perishable profile — recorded, not counted as missing); the incident's **target**
  `{namespace, pod}` as an index for readers (not compared by `verify`); capture→seal latency.
- **`timeline.json`** — normalized, ordered events across sources.
- **`resources/`** — point-in-time YAML of the involved objects and their owner chain.
- **`logs/`** — bounded log tails of involved containers, **including the last-terminated
  instance** (`previous=true`), captured within seconds of the alert *before kubelet GC
  removes it*. Our edge here is **timing, not depth** (the API exposes only the single last
  terminated instance; a multi-crash backlog is roadmap — §8/§11). `lapilli demo` showed the
  horizon is real at the scale of seconds: in a fast crash loop the kubelet had already
  garbage-collected the previous instance's logs 2–3 s after the crash. `logs/index.json`
  therefore records which instance each file came from and names the instances that were
  already gone, rather than leaving a silent hole (see the spec).
- **`changes.json`** — **change indicators** from metadata K8s already carries
  (`generation`, `managedFields` timestamps + actors, ReplicaSet revision annotations).
  *(An earlier draft also listed "ConfigMap content
  hashes"; v0.1 never implemented them.)*
- **`diffs/`** — before/after pod-template diffs of every rollout in the window
  (and of paused, unrolled edits), with when (relative to firing) and who (client-asserted
  field manager). Read from the revision history Kubernetes already keeps, so there is **no
  history store**. Deployment, StatefulSet and DaemonSet, plus an opt-in key-level diff of
  ConfigMaps whose referenced name changed (kustomize-style). See
  [`docs/design-change-diff.md`](docs/design-change-diff.md).
- **`metrics/`** *(optional)* — PromQL range snapshots, raw `query_range` responses
  plus an index. This is the **one source that honestly reaches before the alert**, since
  Prometheus kept the history: the range is `[firing − pre, min(firing + post, capture
  time)]`. The v0.1 limit ("timing, not a time-machine") still holds for events and logs,
  but not for metrics. Built-in queries: cAdvisor memory/CPU and kube-state-metrics
  limits/restarts. Timeouts, a response-size cap, and a points-per-series cap bound the cost.
- **`signature/`** *(optional)* — a detached signature over `manifest.json`, present only
  when signing is enabled (off by default; see §5).
- **`redaction.json`** — redaction policy version, mode, dropped fields, per-file counts, and
  the two lists that say what was *not* touched. Redaction v1 is applied at the source and is
  **best-effort in every mode**. Its candidate set is narrow: env values, `command`/`args`
  (including `sh -c` scripts and exec probes), probe and lifecycle HTTP header values,
  annotations, and event messages. `strict` **widens that candidate set** — every candidate
  value is redacted unless its name is in `redaction.plaintext`, and an unknown `name=value`
  token in free text has its value redacted too — but it is **not a guarantee**: free text can
  hide a secret in a shape no rule matches, and the scope is the same in both modes. No mode
  touches container logs (they are the evidence), `metrics/`, labels, image references, IP
  addresses, `nodeName`, `serviceAccountName`, `managedFields`, or object references such as
  `secretKeyRef` and `imagePullSecrets` names. `redaction.json` names those omissions itself:
  `not_redacted` lists whole trees the policy never visits, `not_redacted_fields` the fields
  that survive inside the files it does (both advisory — `spec/IEB-SPEC.md` §7 fixes only
  `mode`). `mode: off` redacts nothing at all, is recorded, and `lapilli verify` warns loudly
  about it. The field-by-field statement, with what each carries in a real cluster, is
  [`docs/data-handling.md`](docs/data-handling.md). Every `ieb/v1` bundle carries this file; one
  without it is FAILED.

Verification is offline:

```
lapilli verify incident-2026-09-11T02-14-33.ieb --cluster <id> --incident <id> --key lapilli.pub
# recompute per-file hashes → check against manifest → check bound context (fail closed) →
# report coverage (non-zero exit if PARTIAL) → with --key, require a signature by that key.
```

When signing is enabled, the signature is the **literal bytes of `manifest.json`** signed
with cosign-compatible ECDSA-P256 (DER, low-S). The conformance anchor is **openssl**, not a
cosign CLI version (`scripts/verify-conformance.sh`; see §8 item 6).

## 5. Integrity model (capability-by-config, honest)

A signature proves **integrity** (not altered after sealing) and **authenticity** (who
sealed it). It says nothing about **fidelity** (that the bytes truthfully represent cluster
state) — Lapilli signs its own output, so the trust root is an unmodified Lapilli controller.
**Signing is OFF by default in v0.1**; the load-bearing integrity feature is `lapilli verify`
(hash-tree recompute + bound-context fail-closed + coverage PARTIAL), which works with or
without a signature.

Read the unsigned row below with the spec's qualifier attached, because the tick is easy to
over-read. `manifest.json` carries the hash tree's root and sits **inside the bundle**, so an
unsigned bundle detects *accidental* change — a truncated transfer, a careless edit, a
corrupted byte — and detects nothing at all against anyone who can rewrite the file: they
recompute the tree, rewrite the manifest, and `verify` returns 0. `spec/IEB-SPEC.md` says it
without hedging: **"Without `--key`, a bundle proves nothing against anyone who could write to
it."** Tamper-*evidence* against a deliberate actor begins at the signature, and authenticity
begins at a key the verifier already holds.

| Config | Integrity after sealing | Producer authenticity | Independent time | Air-gap | Availability |
|---|:--:|:--:|:--:|:--:|---|
| unsigned (default) | ⚠️ accidental change only (hash tree) | ❌ | ❌ | ✅ | v0.1 |
| static-key ECDSA | ✅ | ✅ (key you hold) | ❌ (self-asserted) | ✅ | v0.1 (opt-in) |
| KMS ECDSA (AWS, GCP) | ✅ | ✅ (separate custody) | ❌ (self-asserted) | ✅ | v0.1 (opt-in) |
| + RFC 3161 TSA | ✅ | ✅ | ✅ (upper bound) | ✅ | v0.3 |
| keyless + Rekor | ✅ | ✅ (pinned OIDC id) | ✅ (transparency) | ❌ | v0.3 (spike-gated) |

"Producer authenticity" holds only when the verifier pins the producer's public key
(`lapilli verify --key`), obtained out of band. The key embedded in the bundle is never
trusted, since a forger can swap it along with the signature (see `spec/IEB-SPEC.md`).

**Explicitly, in the v0.1 default (and static-key) config: sealing time is self-asserted by
the controller clock; there is no independent time anchor.** An independent time bound
arrives only with TSA (v0.3) or a passing keyless spike.

**What Lapilli never claims:** that contents faithfully/completely represent cluster state
(a compromised control plane can fabricate a bundle that still verifies), that capture time
is authentic, or that redaction removed nothing material. **Lapilli is one link in a chain of
custody, not the whole chain** — audit-grade custody also needs, from the deploying org,
independent key custody, **WORM/object-lock storage** (which substitutes for much of what a
transparency log provides), and access logging.

The audit value is therefore **audit-*supporting*** (corroborating evidence that the
incident-response control operated), not "audit-ready." See §9.

## 6. Architecture

```
  Alertmanager ──▶ webhook receiver ──▶ creates IncidentCapture CR
                                              │
                                    ┌─────────▼──────────┐
                                    │  Lapilli Controller   │  Pending→Capturing→Sealing→Exported|Failed
                                    │  (kube-rs)          │  dedup by {rule,cluster,target,firing-bucket}
                                    │                     │  permission self-check every 10 min, needs
                                    │                     │  derived from the installed CaptureProfiles
                                    └─────────┬──────────┘
                    ┌───────────┬─────────────┼─────────────┬───────────────┐
                    ▼           ▼             ▼             ▼               ▼
              resources      events         logs        changes (+diffs/)   metrics (optional)
              (owner chain)  (snapshot)   (incl. previous)  (revision history)  (PromQL range)
                  (collectors parallel + failure-isolated; a partial capture is recorded in
                   the coverage score, never blocks the seal; a profile may defer collectors
                   on purpose — recorded as `coverage.deferred`, not as missing)
                    └───────────┴─────────────┼─────────────┴───────────────┘
                                               ▼
                                   correlator + redactor (at the source)
                  (best-effort in every mode; `strict` only widens the candidate set, it is not a
                   guarantee; what was not touched is listed in the bundle's `redaction.json`)
                                               ▼
             sealer (content hash tree → manifest.json with coverage + bound context)
                                               ▼
             signer (OPTIONAL, off by default: static-key ECDSA · AWS KMS · GCP Cloud KMS)
                                               ▼
             exporter (PVC, with WORM/object-lock guidance · S3/GCS, see docs/design-export.md)
                                               ▼
             notifier (OPTIONAL, off by default: one grouped message per incident once every
                       destination has settled — docs/design-notify.md)
                                               ▼
             retention (OPTIONAL, off by default: bounded local sweep — docs/design-retention.md)
                                               ▼
             retirement: a settled capture leaves the controller's watch (`lapilli.dev/retired`),
                         its CR stays — docs/design-capture-retirement.md

  readers of the sealed file (Lapilli's own; no vendor, no AI tool required):
             lapilli verify · lapilli postmortem · lapilli mcp (a second container beside the
             controller, PVC read-only, bearer token — any MCP client is a consumer)
```

### 6.1 Control plane — `IncidentCapture` + `CaptureProfile` CRDs
A trigger creates an `IncidentCapture` CR named by a hash of `{rule, cluster, target
namespace/pod, firing-ts bucket}` (natural dedup for Alertmanager resends/grouping; the
target is part of the key so one rule firing for two pods in the same minute stays two
captures); the controller reconciles it
through `Pending → Capturing → Sealing → Exported | Failed`. Reconcile is idempotent via
phase + `observedGeneration` + a deterministic bundle name, so re-reconcile never
re-captures. `CaptureProfile` holds reusable policy: the window (`preSeconds`/`postSeconds`),
the collector set and the collectors **deferred** on purpose (the API server refuses a name in
both — a CEL rule the CRD carries), redaction, diffs and metrics options, the notify route, the
signing mode (ignored under `signing.mode=kms`, where the chart decides), and export
destinations **named** from the admin-defined list in the chart — an unknown name is refused,
so a profile can never introduce an endpoint.

### 6.2 Data plane — collectors
Bounded, parallel, **failure-isolated**: one collector erroring degrades the bundle
(recorded in the coverage score) but never blocks the seal. A partial bundle beats no bundle.

### 6.3 Why Rust (honest)
Not a differentiator by itself — CNCF is language-agnostic. Rust *fits* because an always-on,
in-cluster component handling evidence under memory pressure wants memory safety and no GC
pauses; `kube-rs` (CNCF Sandbox) is a mature controller-runtime-class foundation.

## 7. Security & threat model (summary; full analysis in the round-1 log)

- **Never read Secret values.** The collectors never touch Secrets at all (the chart grants
  `get` only on the Secrets it names by `resourceNames` — the signing key and its own tokens);
  an in-place overwrite of a referenced ConfigMap/Secret is reported in `diffs/` as
  unrecoverable rather than reconstructed.
- **Scoped RBAC** to explicit GVKs/namespaces, never `*`.
- **Egress allowlist** pinned to the export endpoint; `CaptureProfile` targets validated
  against an admin allowlist so the sanctioned export path can't become an exfil channel.
- **Signing-key isolation is a KMS-config property.** In the static-key config the key shares
  the controller's trust boundary, so a collector RCE could reach it; deployments needing true
  isolation use `signing.mode=kms` (AWS KMS or GCP Cloud KMS; the key never enters the
  cluster — `docs/kms.md`). Signing is off by default, so this affects only opt-in signed
  deployments. The key never leaves the controller in any mode: round 24 rejected an in-cluster
  Job because the RBAC it needs is the RBAC that reaches the key Secret.
- **Authenticated webhook** — the Alertmanager webhook requires a bearer token by default
  (chart-generated Secret; Alertmanager sends it via `http_config.authorization`), re-read
  from disk within five seconds so rotation needs no restart (a rotated-away token is
  therefore accepted for up to that long), compared in constant time, failing closed if the
  token file is missing or shorter than 32 characters.
- **What the webhook token is worth.** An alert names the pod to capture, so whoever can POST
  chooses whose logs, object body and events are sealed into a bundle. A capture is therefore
  held to `watchNamespaces`: a target outside the list is refused as `target-not-watched`, with
  no bundle. **With no list the install records every namespace** — that is what the collector
  ClusterRole grants — and the webhook token is then worth a read of any pod's logs in the
  cluster. Name the namespaces, and restrict who can reach the port
  (`webhook.networkPolicy`). Round 30 found this: the list had only ever been used to decide
  which permissions to ask about. An optional NetworkPolicy limits who can reach the port (`webhook.networkPolicy`). Being a
  NetworkPolicy it denies **all** other ingress to the pod, so the chart names the health and mcp
  ports in the same object rather than leaving them dead (`docs/egress.md`).
- **Replay/substitution defense** — the incident-identity tuple is bound into `manifest.json`
  (and thus the signature, when signing is on); `lapilli verify` fails closed if
  caller-asserted context doesn't match.
- **Keyless identity caveat (v0.3)** — for an in-cluster SA the OIDC issuer is the cluster
  itself (circular for audit); prefer an external IdP or a KMS key held by a separate team.

## 8. v0.1 — true minimum scope (walking skeleton that proves the value)

> **Historical.** This is the minimum set round 2 fixed so the skeleton would exist before
> anything else did. It was reached, and the first release ships considerably more (§11 and
> `README.md`); the list is kept as the record of what "minimum" meant, not as the current
> feature set.

Unique value preserved: *an operational alert fires → out comes a self-contained, portable
bundle with the stuff you'd otherwise lose (previous-container logs + change indicators).*

**In v0.1:**
1. **Alertmanager webhook** receiver → `IncidentCapture` CR. (Only trigger in v0.1.)
2. **Two CRDs** (`IncidentCapture` phase machine + `CaptureProfile`).
3. **Three failure-isolated collectors:** K8s-API resource + owner chain; events snapshot
   (read at capture time, not a watcher); log tails incl. `previous=true`.
4. **`changes.json` = change *indicators*** from free metadata (no history subsystem).
5. **Sealer:** content-hashed SHA-256 tree + `manifest.json` (coverage score + bound context
   tuple + self-reported image digest). Signed payload = literal `manifest.json`.
6. **Signer (optional, OFF by default):** **static-key ECDSA (cosign-compatible DER)** behind a
   pluggable trait, with an **executable conformance gate** that verifies a freshly signed
   manifest using **openssl** and refuses a tampered one (`scripts/verify-conformance.sh`).
   *Not* cosign: this plan originally pinned `cosign verify-blob`, and cosign v3 removed detached
   signature verification. A neutral primitive is the stronger anchor anyway — it proves the
   signature is standard ECDSA-P256-SHA256 rather than proving one CLI version accepts it.
7. **Exporter: PVC only**, documented to land in a **WORM/object-lock** store.
8. **`lapilli verify`** (hash recompute + bound-context fail-closed + coverage PARTIAL/non-zero
   exit) and **`lapilli demo`** built as the **standing kind E2E harness** (POST the webhook
   directly for timing determinism; pre-stage the crashing workload; pre-pull images).

**First milestone — a tracer bullet through every risky seam, minimum code:**
> webhook POST → `IncidentCapture` CR → controller runs **one** collector (log tails incl.
> `previous=true`) → sealer writes `manifest.json` with a content hash tree → static-key
> ECDSA signs manifest.json → PVC export → `lapilli verify` accepts it and openssl confirms the
> signature — all wired as a kind CI E2E from day one.

**Deferred (was creeping into v0.1):** ~~KMS backend~~ (done: AWS + GCP), keyless + Rekor + its spike
(separable, off the critical path; de-risks a *bonus*), ~~real spec change-diff~~ (done, no
recorder needed), ~~S3/GCS/OCI export~~ (done), ~~PromQL collector~~ (done), SLSA provenance
(in the release workflow, unexercised until the first tag), signed pre-redaction Merkle root
(→v0.3 — round 19 found it reverses §5's "no hash and no length" promise and needs a second key
custody), RFC 3161 TSA / TUF snapshot (→v0.3, doc-only in v0.1), event trigger without an alert
rule (returned to premise in round 16; round 19 declined to revive it for v0.2).

**Honest effort:** ~12–14 weeks solo from zero (not 10). Security machinery kept in v0.1 is
cheap (two bindings + coverage); the cuts above are what keep the estimate credible.

## 9. CNCF alignment & path (do NOT apply at design stage)

- **Ship first.** Sandbox postpones code-less design docs. Apply only with a tagged v0.1, a
  kind-cluster demo, and an early-adopter signal.
- **TAG fit: Operational Resilience.** Sandbox entry is a lightweight TOC decision — TAGs
  don't gate or sponsor it; TAG alignment matters at Incubation/Graduation due diligence.
  The TAG that would review Lapilli there is **Operational Resilience** (troubleshooting /
  reliability / Day-2 + the 2025 fold-in of observability), and the charter fit is clean:
  the tool's trigger, captured state, and users are all operational. Compliance is a
  *downstream consumer* of the bundle, not the reviewing TAG.
- **Audit angle = opportunistic, not promised.** Keep an "audit-*supporting*" section and
  pursue a named-control mapping (ISO 27001 A.16 / SOC 2 CC7.x) + 2–3 "we'd use this"
  signals. If they don't materialize, it stays a footnote — the project stands on the SRE
  value alone.
- **"Format/standard" is earned, not claimed.** Ship a reference bundle layout; only call it
  a standard once an independent producer or consumer adopts it.
- **Bus factor:** recruit ≥1 co-maintainer from another org before applying.
- **Keep out of the submitted pitch:** the eBPF roadmap (research), any "we own the Rust
  niche" claim, and the in-toto/SLSA "attestation" analogy (Lapilli is a *signed observation
  record*, not an attestation).

## 10. GTM wedge (fixes the deferred-value adoption trap)

1. **`lapilli demo`** — value visible in 5 minutes, no real outage; the "aha" is the recovered
   previous-container logs + change indicators (needs no signature at all).
2. **Trigger on everyday failures** (CrashLoopBackOff, OOMKill, failed rollout), so the
   evidence folder fills up weekly, not once a year.
3. **Default is lightweight** — single-namespace-capable, PVC-only, signing off, minimal
   RBAC, 2-minute install. Signing / cluster-wide / S3 export are opt-in upgrades.
4. **Consumable output** — a clean bundle layout, and `lapilli mcp` answering questions about
   the evidence over a standard protocol, so any client the team already uses can read it
   (HolmesGPT is one worked example under `integrations/`; a consumer, not the reason).
   Adoption of the *layout* is how "format" gets earned.

## 11. Roadmap

Order and priority live in [`ROADMAP.md`](ROADMAP.md); this table only says which version a
thing belongs to. There was never a v0.1/v0.2 *release* split: everything built so far shipped
in the first tag. `v0.2.0` (2026-10-01) added no feature — it is four defect fixes, one of them a
privacy defect, and it moved the minor rather than the patch because a published crate's public API
broke (`CHANGELOG.md`).

| Version | Theme | Scope |
|---|---|---|
| **v0.1.0** (released 2026-09-28) | The incident flight recorder, complete | §8's minimum, plus everything built since under the identity sentence: PromQL metric window · redactor v1 · spec change-diff (`docs/design-change-diff.md`) · S3/GCS export (`docs/design-export.md`) · KMS signing, AWS + GCP (`lapilli-kms`; real-cloud smoke: **GCP done 2026-09-25**, `test/fixtures/kms/`; AWS still outstanding) · remote verify and `verify-result/v1` (`docs/design-remote-verify.md`, `spec/VERIFY-RESULT.md`) · controller metrics with executable alert rules (`docs/metrics.md`) · notification (`docs/design-notify.md`) · bounded retention, off by default (`docs/design-retention.md`) · `status.message` (`docs/design-status-message.md`) · storm bounds and capture retirement (`docs/design-trigger-and-load.md`, `docs/design-capture-retirement.md`) · permissions that follow the profiles (`docs/design-permissions-by-profile.md`) · the perishable profile and `coverage.deferred` (`docs/design-record-and-seal.md`, phase A) · `incident.target` · `lapilli postmortem` (`docs/design-postmortem.md`) · `lapilli mcp` (`docs/design-distribution-path.md`) · provenance and a cargo-auditable-backed SBOM in `release.yml`, both exercised by `v0.1.0-rc.1` (the SBOM enumerated only base-image packages until round 30; BuildKit's attestations are unsigned in-toto, not Sigstore-signed SLSA). Gates: `docs/COMPATIBILITY.md`, `RELEASE.md`. |
| **v0.2** | What adopters hit first | Per-alert profile selection (an allow-list design first: a label-selected profile must not let a tenant route evidence through another team's notify route or export destinations) · node-level captures · cluster cost and single-replica topology · the round 21–22 measurements at n=3 and off a laptop · whatever the first installs report. **Not scheduled**, returned to premise: phase B backfill (`docs/design-record-and-seal.md`); the event trigger without an alert rule (rounds 16 and 19 — if revived it starts from Pod status, `docs/design-trigger-and-load.md` §5). |
| **v0.3** | Audit-grade trust (opt-in) | keyless + Rekor (spike) · RFC 3161 TSA (air-gap time) · embedded TUF-root long-term verification · named-control mapping · `keys/<key_id>.pub` publication · **signed pre-redaction commitment** (moved here by round 19: as specified it is an unsalted oracle for exactly the values redaction removed, reversing §5's "no hash and no length are emitted"; a sound version needs a second key custody, which belongs with this row's other custody work). All of it *earned later* under Path C; none of it moves before adopters ask. |
| **research (out of Sandbox scope)** | eBPF causality | `aya` node agent: always-on ring buffer dumped into the bundle on trigger — the multi-crash backlog + kernel causality graph. Long-term research, **not** a submitted deliverable. |

### On bundle lifecycle (ships in v0.1.0, off by default)

**Retention is built, off by default** (`docs/design-retention.md`). What follows is the argument
that shaped it, kept because the constraints still bind anyone changing it — but the opening
sentence below was true only before that work landed, and is quoted here as history, not as
current behaviour.

> ~~**Nothing deletes a sealed bundle today.**~~ They accumulate on the PVC and at every export
destination for as long as the install lives. That is a capacity problem for any busy cluster and a
liability problem for anyone who has to answer for what they still hold — neither of which is
specific to a compliance regime, which is why this belongs in the product rather than in a
deployment guide.

It is deliberately **not** a `retentionDays` flag, and the three constraints below were designed
and adversarially reviewed before any code existed (`docs/design-retention.md`,
`docs/design-review-round17.md`). **This is now built and off by default.**

**A delete feature in an evidence tool is a destroy-evidence feature.** Whatever can remove a bundle
can also make an inconvenient incident disappear. So deletion has to be at least as recorded as
capture is, and it must not be reachable by a path an attacker who already has the cluster would
have anyway. The obvious shape — the controller prunes what it wrote — gives the controller exactly
that power.

**The store gets a vote.** A destination under S3 Object Lock will refuse the delete, by design. A
tool that reports success while the object remains is worse than one that refuses, so retention has
to be expressed per destination and reconciled against what the store actually permits, the way
export already reconciles a conflicting object rather than overwriting it.

**Redaction is best-effort** (§4), and a bundle also holds whole categories redaction never visits
— container logs, labels, IPs, `managedFields` ([`docs/data-handling.md`](docs/data-handling.md)).
That is an argument *for* bounded retention, not for trusting the redactor — and it means the
retention default cannot be "keep forever" just because keeping is the safe choice for evidence.

**The shipped default is keep forever, and this is where that is admitted.** `retention.maxBytes: 0`
and `retention.days: 0`, and a reclaim additionally needs every destination the capture references
observed `Uploaded` unless `allowUnexported` is set — so a PVC-only install reclaims nothing but
abandoned work even with a bound configured. Deliberate, because the alternative was a delete path
nobody opted into; but it leaves the liability half of the argument above unresolved by the product
and standing on the operator. Round 30 found this paragraph arguing against the default the same
tree ships.

**And an Object Lock retention period is a legal decision, not a storage one.** In compliance mode
with the bucket policy `docs/design-export.md` recommends, the bundles cannot be deleted or
shortened by anyone — including the account root — until the period expires. Since a bundle can hold
personal data, an erasure request against a locked bundle cannot be honoured while the lock runs.
That is what WORM is for and it is also its cost: set the period against the legal retention period
for what the bundles hold.

