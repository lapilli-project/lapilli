# Lapilli — Design & Architecture

> **Lapilli is a flight recorder for Kubernetes incidents.** The moment an alert fires, it
> captures the full incident window — the events, the owner-chain YAML, the logs from the
> container that *just died*, the metric shape, and what recently changed — into **one
> portable file**. You stop reconstructing timelines from memory and screenshots.

Status: `pre-alpha` — v0.1 walking skeleton works end to end on kind (proven in CI) ·
Language: Rust · TAG fit (Incubation review): **Operational Resilience** · Deliverable: an
operational incident recorder + a portable reference bundle layout.

> **How this doc was hardened.** Nineteen adversarial review rounds have run
> ([`docs/design-review-round1.md`](docs/design-review-round1.md) through
> [`round19`](docs/design-review-round19.md)), and the later ones shaped this document as much as
> the first two: round 16 returned the event trigger to premise and rewrote §11's v0.2 cell,
> round 17 rebuilt the retention design before any code existed, and round 19 rejected a
> pre-redaction commitment scheme. Round 2 chose **Path C**:
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
> **at alert-time-plus-seconds, before the volatile evidence (previous-container logs,
> un-coalesced events) finishes rotating away** · merged into **one portable file you own**,
> vendor-neutral, that any tool can read and that outlives any cluster or platform.

Two different comparisons, two different wins:
- **vs open tools** (must-gather, troubleshoot.sh): they're **manual and arrive after the
  evidence horizon**; Lapilli is automatic and captures at t+seconds, so the volatile evidence
  still exists. This is **timing + completeness**, not a claim of deep "correlation" — v0.1's
  `timeline.json` is an ordered merge across sources; richer causal links are v0.2.
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
  (which collectors ran, % of intended set); capture→seal latency.
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
- **`diffs/`** *(v0.2)* — before/after pod-template diffs of every rollout in the window
  (and of paused, unrolled edits), with when (relative to firing) and who (client-asserted
  field manager). Read from the revision history Kubernetes already keeps, so there is **no
  history store**. Deployment, StatefulSet and DaemonSet, plus an opt-in key-level diff of
  ConfigMaps whose referenced name changed (kustomize-style). See
  [`docs/design-change-diff.md`](docs/design-change-diff.md).
- **`metrics/`** *(v0.2, optional)* — PromQL range snapshots, raw `query_range` responses
  plus an index. This is the **one source that honestly reaches before the alert**, since
  Prometheus kept the history: the range is `[firing − pre, min(firing + post, capture
  time)]`. The v0.1 limit ("timing, not a time-machine") still holds for events and logs,
  but not for metrics. Built-in queries: cAdvisor memory/CPU and kube-state-metrics
  limits/restarts. Timeouts, a response-size cap, and a points-per-series cap bound the cost.
- **`signature/`** *(optional)* — a detached signature over `manifest.json`, present only
  when signing is enabled (off by default; see §5).
- **`redaction.json`** *(v0.2)* — redaction policy version, mode, dropped fields, per-file
  counts. Redaction v1 is applied at the source (env values, args, probe headers,
  annotations, event messages) and is **best-effort** by design; `strict` mode for a
  guarantee. Container logs are never redacted: they are the evidence. v0.1 bundles had no
  redaction and should be treated as sensitive.

Verification is offline:

```
lapilli verify incident-2026-09-11T02-14-33.ieb --cluster <id> --incident <id> --key lapilli.pub
# recompute per-file hashes → check against manifest → check bound context (fail closed) →
# report coverage (non-zero exit if PARTIAL) → with --key, require a signature by that key.
```

When signing is enabled, the signature is the **literal bytes of `manifest.json`** signed
with cosign-compatible ECDSA-P256, verifiable with upstream **cosign v2.x**
`cosign verify-blob` (pinned; see §8).

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
| KMS ECDSA | ✅ | ✅ (separate custody) | ❌ (self-asserted) | ✅ | v0.2 |
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
                                    │  (kube-rs)          │  dedup by {rule,cluster,firing-bucket}
                                    └─────────┬──────────┘
                        ┌───────────────┬─────┴────────┬───────────────┐
                        ▼               ▼              ▼               ▼
                  K8s-API/owner   events snapshot  log tails      change-indicators
                  (collectors parallel + failure-isolated; a partial capture is recorded in
                   the coverage score, never blocks the seal)
                        └───────────────┴──────┬───────┴───────────────┘
                                               ▼
                                   correlator + redactor (best-effort, at the source; `strict` mode)
                                               ▼
             sealer (content hash tree → manifest.json with coverage + bound context)
                                               ▼
             signer (OPTIONAL, off by default: static-key ECDSA · KMS in v0.2)
                                               ▼
             exporter (PVC, with WORM/object-lock guidance · S3/GCS, see docs/design-export.md)
                                               ▼
             notifier (OPTIONAL, off by default: one grouped message per incident once every
                       destination has settled — docs/design-notify.md)
                                               ▼
             retention (OPTIONAL, off by default: bounded local sweep — docs/design-retention.md)
```

### 6.1 Control plane — `IncidentCapture` + `CaptureProfile` CRDs
A trigger creates an `IncidentCapture` CR named by a hash of `{rule, cluster, target
namespace/pod, firing-ts bucket}` (natural dedup for Alertmanager resends/grouping; the
target is part of the key so one rule firing for two pods in the same minute stays two
captures); the controller reconciles it
through `Pending → Capturing → Sealing → Exported | Failed`. Reconcile is idempotent via
phase + `observedGeneration` + a deterministic bundle name, so re-reconcile never
re-captures. `CaptureProfile` holds reusable policy (window, collector set, redaction rules,
export target validated against an **admin-set allowlist**, signing mode).

### 6.2 Data plane — collectors
Bounded, parallel, **failure-isolated**: one collector erroring degrades the bundle
(recorded in the coverage score) but never blocks the seal. A partial bundle beats no bundle.

### 6.3 Why Rust (honest)
Not a differentiator by itself — CNCF is language-agnostic. Rust *fits* because an always-on,
in-cluster component handling evidence under memory pressure wants memory safety and no GC
pauses; `kube-rs` (CNCF Sandbox) is a mature controller-runtime-class foundation.

## 7. Security & threat model (summary; full analysis in the round-1 log)

- **Never read Secret values** — detect Secret change via `resourceVersion`/`generation`,
  not by hashing plaintext (which would need `get secrets` cluster-wide).
- **Scoped RBAC** to explicit GVKs/namespaces, never `*`.
- **Egress allowlist** pinned to the export endpoint; `CaptureProfile` targets validated
  against an admin allowlist so the sanctioned export path can't become an exfil channel.
- **Signing-key isolation is a KMS-config property (v0.2).** In the v0.1 static-key config
  the key shares the controller's trust boundary, so a collector RCE could reach it;
  deployments needing true isolation should use KMS (v0.2) or run signing out-of-process.
  Signing is off by default in v0.1, so this affects only opt-in signed deployments.
- **Authenticated webhook** — the Alertmanager webhook requires a bearer token by default
  (chart-generated Secret; Alertmanager sends it via `http_config.authorization`), read per
  request so rotation needs no restart, compared in constant time, failing closed if the
  token file is missing. An optional NetworkPolicy limits who can reach the port.
- **Replay/substitution defense** — the incident-identity tuple is bound into `manifest.json`
  (and thus the signature, when signing is on); `lapilli verify` fails closed if
  caller-asserted context doesn't match.
- **Keyless identity caveat (v0.3)** — for an in-cluster SA the OIDC issuer is the cluster
  itself (circular for audit); prefer an external IdP or a KMS key held by a separate team.

## 8. v0.1 — true minimum scope (walking skeleton that proves the value)

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
> ECDSA signs manifest.json → PVC export → `lapilli verify` + pinned `cosign verify-blob`
> accept it — all wired as a kind CI E2E from day one.

**Deferred (was creeping into v0.1):** KMS backend (→v0.2), keyless + Rekor + its spike
(separable, off the critical path; de-risks a *bonus*), real spec change-diff (→v0.2, no
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
4. **Consumable output** — a clean bundle layout + optional adapters so tools you already use
   (HolmesGPT/k8sgpt) can read it. Adoption of the *layout* is how "format" gets earned.

## 11. Roadmap

| Version | Theme | Scope |
|---|---|---|
| **v0.1** | Incident flight recorder | §8 minimum scope (unsigned default; optional static-key signing) |
| **v0.2** | Depth + durability | ~~PromQL metric window~~ (done) · **postmortem draft** (round 11's product lens called this the stronger feature; recorded here in round 16 after the conclusion never reached this table) · ~~redactor v1~~ (done) → ~~spec change-diff (Deployment/StatefulSet/DaemonSet + opt-in ConfigMap follow)~~ (done; no always-on recorder, see `docs/design-change-diff.md`) · ~~S3/GCS export~~ (done: `docs/design-export.md`) · ~~KMS signing~~ (done: AWS + GCP, `lapilli-kms`; **real-cloud smoke test still outstanding** — only emulators have run) · ~~SLSA provenance~~ (in `release.yml`: `provenance: mode=max`, `sbom: true`; **unexercised, because no tag exists yet**) · ~~bundle lifecycle: retention and deletion~~ (done, off by default: `docs/design-retention.md`, `docs/design-review-round17.md`) → **remaining: postmortem draft only.** Moved to v0.3 by round 19: signed pre-redaction Merkle root, event trigger without an alert rule |
| **v0.3** | Audit-grade trust (opt-in) | keyless + Rekor (spike) · RFC 3161 TSA (air-gap time) · embedded TUF-root long-term verification · named-control mapping · **signed pre-redaction commitment** (moved from v0.2 by round 19: as specified it is an unsalted oracle for exactly the values redaction removed, reversing §5's "no hash and no length are emitted"; a sound version needs a second key custody, which belongs with this row's other custody work) · **event trigger without an alert rule** (returned to premise in round 16; round 19 declined to revive it — the premise that an operator cannot write an alert rule is still unestablished, and an always-on watcher is a different product from the one §3 positions) |
| **research (out of Sandbox scope)** | eBPF causality | `aya` node agent: always-on ring buffer dumped into the bundle on trigger — the multi-crash backlog + kernel causality graph. Long-term research, **not** a submitted deliverable. |

### On bundle lifecycle (v0.2)

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

**Redaction is best-effort** (§5). A bundle may hold personal data the redactor missed. That is an
argument *for* bounded retention, not for trusting the redactor — and it means the retention default
cannot be "keep forever" just because keeping is the safe choice for evidence.

