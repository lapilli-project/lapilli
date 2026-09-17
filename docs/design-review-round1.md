# Design Review — Round 1 (adversarial, 5 lenses)

This is the decision log from the first "loop-engineering" hardening pass on Kairn's
design. Five adversarial reviews (CNCF TAG skeptic, competitive landscape, solo-dev
feasibility, security threat model, adoption/product) attacked `DESIGN.md`. Below are the
findings that **survived**, the resulting decisions, and where the design changed.

Preserved deliberately: CNCF Sandbox now weighs "did you study prior art / talk to
incumbents?" heavily, so this log is itself evidence of due diligence for the application.

## Cross-cutting conclusion

Three independent lenses (competition, TAG, security) converged on the same point:
**"signing" and "automatic capture on trigger" are not novel** — they already ship:

- **Falco Talon + CRIU** — Falco rule fires → automatic forensic container checkpoint
  (portable, restorable). Auto-trigger + portable, on autopilot, in the CNCF ecosystem.
- **Sysdig / Falco captures (SCAP)** — capture-on-alert of the syscall stream from the
  trigger point; portable, replayable. Three of Kairn's four properties, shipping today.
- **Kosli** — tamper-evident Evidence Vault of *signed* attestations for K8s runtime,
  exports portable audit packages. Directly occupies the "signed + portable + audit" story.
- **troubleshoot.sh support-bundle + `cosign verify-blob`** — signing a portable K8s
  diagnostic bundle is documented and off-the-shelf.
- **Sidereal** (Rust + Go) — K8s operator producing portable OSCAL evidence packages with
  HMAC integrity. Weakens "no Rust project in this niche."

**Therefore the moat is a positioning wedge, not a technical moat.** What remains genuinely
unoccupied as a single named product is the exact combination:

> Triggered by **operational/reliability** signals (not security detections) · correlating
> **multiple K8s-native sources across a symmetric `[t-Δ, t+Δ]` window** (events + owner-chain
> YAML + previous-container logs + metric shape + change indicators) · sealed as an **open,
> portable, offline-verifiable evidence format** that any tool can consume.

Kairn lives in the seam between **security capture-on-detect** (Talon/CRIU/Sysdig: trigger
+ capture, but syscall-only, unsigned, uncorrelated) and **compliance evidence automation**
(Kosli/Sidereal: signed + packaged, but scheduled/continuous, not incident-window-triggered).
The seam is real but thin — so the design must lead with the wedge and the open format, and
must cite the incumbents proactively.

---

## Decisions by theme

### D1 — Positioning: SRE-first story, compliance as the second slide
- **Adoption lens:** audit-first points the product at the audience that *can't install it*
  (GRC teams don't run controllers) to solve a problem audits don't actually pose in
  Kairn's form (SOC 2 / ISO 27001 accept SIEM/WORM logs + tickets, not per-incident
  cosign+Rekor snapshots). Compliance-first also narrows the contributor funnel (small,
  non-Rust, non-K8s community).
- **TAG lens:** the compliance/evidence angle *is* the CNCF differentiation; pitch to
  **TAG Security & Compliance**.
- **Decision:** Same product, reordered story. README / GTM / community = **SRE flight
  recorder ("never lose incident context")**; CNCF differentiation = the evidence/format
  angle to TAG Security & Compliance. Not a contradiction — acquisition hook vs.
  differentiation slide.

### D2 — Reframe the deliverable around the open IEB format + reference producer
- Lead with the **`.ieb` spec** ("your incident evidence, a portable file you own, that
  any tool can read and no vendor can hold hostage"), with Kairn as the reference producer.
- Publish a draft `spec/IEB-SPEC.md` **in-repo now** (not roadmap v0.3).
- Pull the "producer, not competitor" consumer adapters (HolmesGPT/k8sgpt) forward.

### D3 — Integrity claim: honest, layered, no "chain of custody"
- Drop "chain of custody," "proof of capture time," and any implication that a valid
  signature = truthful/complete evidence.
- New claim = three guarantees (integrity-after-sealing, producer authenticity, online
  sealing-time upper bound via Rekor) + an explicit "does NOT guarantee" list (content
  fidelity/completeness, capture time, redaction completeness). Kairn is **one link** in a
  chain of custody the deploying org completes (independent key custody, WORM storage,
  access logging). See rewritten §"Integrity model".

### D4 — Signing backend: static/KMS default, keyless behind a spike gate
- `sigstore` (sigstore-rs) is verify-focused; signing is experimental. The separate
  `sigstore-rust` workspace claims keyless but is v0.1.x. Unattended in-cluster keyless
  needs **ambient/workload-identity OIDC** (projected SA token → Fulcio), unproven in Rust
  and the real time sink.
- **Decision:** v0.1 default = **static-key / KMS ECDSA (cosign-compatible)**, signing
  backend **pluggable**, `kairn verify` interoperates with upstream `cosign verify-blob`.
  Keyless is promoted only if a ~1-week in-cluster spike (projected SA token → Fulcio →
  Rekor → verify with upstream cosign) passes. This also fixes the internal contradiction
  (roadmap had Rekor/KMS in v0.3 while §3.2 made keyless the v0.1 default).

### D5 — Cut "subsystems in disguise" from v0.1
- **change-diff → "change indicators":** real spec diff needs a prior-state history store
  (always-on stateful subsystem). v0.1 emits only free-metadata change evidence
  (`generation`, `managedFields` timestamps/actors, revision annotations, ConfigMap
  hashes). Real diff = v0.2, which *justifies* building the rolling recorder deliberately.
- **log tails:** the API serves only the **last-terminated** instance, best-effort until
  kubelet GC. Reword: "last-terminated instance, captured before GC." Our edge is
  **timing**, not depth. Multi-crash backlog = roadmap (needs the same always-on buffer,
  aligns with the eBPF ring-buffer idea).
- **trigger:** Alertmanager webhook only in v0.1 (push, idempotent, reliable). Warning-event
  watcher is lossy (event TTL, coalescing, API rate-limits) → v0.2, best-effort.
- **export:** PVC only in v0.1 (`object_store` trait for S3/GCS later).
- **redaction:** deterministic subset only; **never read Secret values** (see D6).

### D6 — Security hardening (threat model)
- **Never read Secret `.data`** — hashing plaintext needs `get secrets` cluster-wide, which
  contradicts least-privilege. Detect Secret change via `resourceVersion`/`generation`.
- **Scoped RBAC** to explicit GVKs/namespaces, not `*`.
- **Egress allowlist** pinned to the export endpoint; `CaptureProfile` export targets
  validated against an admin-set allowlist (attacker can't redirect the sanctioned exfil
  channel).
- **Separate the signing key/identity from the collector code path** so a collector RCE
  doesn't grant signing.
- **Replay/substitution defense:** bind `{incident-id, cluster-id, trigger-rule+firing-ts,
  capture-window}` into the signed manifest (and Rekor entry); `kairn verify` checks caller-
  asserted context and fails closed on mismatch.
- **Redaction transparency:** sign a Merkle root of the **pre-redaction** file hash set
  (values never leave) + the redaction policy version/hash + per-file redaction magnitude.
- **Coverage score:** sign a completeness score (which collectors ran, % of intended set);
  `kairn verify` surfaces degradation loudly ("PARTIAL", non-zero exit) so a green
  signature over a hollow bundle can't read as authoritative.
- **Producer attestation:** ship SLSA build provenance for the Kairn image (signed by CI,
  not the runtime) and record the running image digest in the manifest, so "sealed by a
  genuine unmodified Kairn build" is checkable.
- **Long-term offline verify:** embed the full cert chain + Rekor SET/inclusion proof + a
  snapshot of the Sigstore TUF trust root at seal time (multi-year audit retention).
- **Air-gap honesty:** KMS mode loses transparency log + independent timestamp; add an
  RFC 3161 TSA for an independent upper-bound time; document the weaker guarantee in a
  side-by-side table.

### D7 — CNCF application readiness (do NOT apply at design stage)
- **Ship v0.1 first** — code + a real/kind-cluster demo + a tagged release + early
  adopter signal. A design doc is postponed on sight.
- **Fix stale facts:** TAG Observability was folded into **TAG Operational Resilience**
  (2025); audit/compliance is **TAG Security & Compliance**. Cite in-toto / SLSA /
  TrestleGRC (OSCAL) as precedent that CNCF accepts compliance/attestation tooling.
- **De-risk bus factor:** recruit ≥1 co-maintainer from another org; name them.
- **Demand evidence:** map the IEB to a *named* control (e.g. ISO 27001 A.16 incident
  management / SOC 2 CC7.x) and gather 2–3 concrete "we'd use this" signals.
- **Drop from the submitted pitch:** eBPF roadmap (research, out of scope), "we own the
  Rust niche" (CNCF is language-agnostic).

### D8 — GTM wedge (fixes deferred-value adoption)
- **`kairn demo`** — synthetic incident (OOMKill/crashloop/bad rollout) → a real `.ieb` in
  5 minutes, before any real outage. The "aha" = recovered previous-container logs +
  change indicators.
- **Trigger on everyday failures** (CrashLoopBackOff, OOMKill, failed rollout), not just
  paged Sev1s, so value compounds weekly.
- **recorder-lite** — single-namespace, PVC-only, no signing, minimal RBAC, 2-minute
  install. Signing / cluster-wide / S3 export are opt-in upgrades after the hook.

---

## Realistic v0.1 effort: ~10–13 focused weeks (solo), gated by the 1-week keyless spike.
Original v0.1 was ~2× that with two unproven-in-Rust dependencies on the critical path.
