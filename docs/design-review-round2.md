# Design Review — Round 2 (convergence verification)

Round 1 broke the design open; Round 2 re-attacked the revision with 3 lenses (wedge
defense, integrity attractiveness, v0.1 feasibility) to check whether the fixes held.
They did not fully hold: Round 2 surfaced a genuine **strategic** blocker plus concrete
technical corrections. This log records the convergence and the decision taken.

## Strategic finding (two lenses converged)

**The signed + incident-triggered + correlated-as-one-artifact differentiation is wanted by
neither persona whole.** SREs strip signing (they'll run "recorder-lite"); auditors
substitute SIEM + WORM + tickets (they don't install controllers). Consequences:

- **TAG homelessness** — TAG Operational Resilience says "your differentiator is
  compliance, go to Security & Compliance"; Security & Compliance says "your users are SREs
  doing Day-2, go to Operational Resilience." Each sees the other's concern as load-bearing.
- **in-toto/SLSA "attestation" framing backfires** — §9 summoned in-toto's rigor while §5
  honestly conceded Lapilli can't prove content fidelity. Self-contradiction inside one doc.
- **"open format / standard" is one-vendor JSON** until an independent second producer or
  consumer adopts it. A `SPEC.md` alone is necessary, nowhere near sufficient.

### DECISION: Path C — ship as the flight recorder now, earn the ambition later

Chosen over Path A (keep the format/audit ambition, requires validating a novel buyer +
a second-party format adopter *now*) and Path B (amputate the ambition entirely).

- **Ship** as **"the open incident flight recorder for Kubernetes"** — SRE-first, homed in
  **TAG Operational Resilience** (the tool's trigger, captured state, and users are all
  operational). This is the defensible, non-homeless, shippable claim.
- **Demote** signing and audit to a **documented optional/bonus** layer, not the pitch.
- **Downgrade** "open format / standard / attestation" language to **"a portable reference
  bundle layout + reference producer."** Format standardization becomes an *earned* goal:
  pursued only if adopters + an independent consumer materialize.
- **Keep** Tyler's ISMS strength alive as an opportunistic "audit-supporting" section +
  a named-control mapping (ISO 27001 A.16 / SOC 2 CC7.x) — pursued, not promised.
- **Drop** the in-toto/SLSA attestation analogy; use "signed observation record."

The buyer question ("who needs signed + triggered + correlated as one unit — regulated
forensics? SLA-dispute evidence? insurance/liability?") is not answered by fiat; it's
deferred to real adoption signals. Until then the project does not bet on it.

## Technical corrections (apply regardless of path)

### T1 — Integrity, honestly scoped to the shipping config
- **Signing OFF by default in v0.1** (the "recorder-lite" posture is the default, not a SKU).
  The load-bearing security feature is **`lapilli verify`** (recompute hash tree, check
  signature *if present*, check caller-asserted bound context **failing closed**, report
  **PARTIAL coverage with non-zero exit**) — this defeats hollow-bundle and substitution
  attacks. The signature is a bonus for the dispute/audit user.
- **§5 becomes a capability-by-config matrix**, not a flat 3-guarantee list:
  - static-key (v0.1 default): integrity-after-sealing + producer authenticity. **Sealing
    time is self-asserted; no independent time anchor.**
  - +KMS (v0.2): same, key custody by a separate team.
  - +TSA (v0.3): + independent time upper bound (works air-gapped).
  - +keyless/Rekor (v0.3, spike-gated): + transparency + independent time.
- **"audit-ready" → "audit-supporting"** everywhere; corroborating evidence that the IR
  control operated, not a compliance product.
- **WORM/object-lock export** (org-provided) substitutes for much of what Rekor gives; make
  it the documented v0.1 durability guidance.

### T2 — cosign interop is a moving target → pin it
- Raw crypto is easy in Rust (`p256`+`ecdsa`+`sha2`; KMS via `aws-sdk-kms` `ECDSA_SHA_256`).
- But cosign is mid-break: **v2 detached `--signature`/`--key`/`--insecure-ignore-tlog`**
  vs **v3 bundle-only** (`--bundle`, protobuf `.sigstore.json`). "cosign-compatible" left
  unpinned is a day-one broken promise for v3 users.
- **Fix:** pin the interop promise to **cosign v2.x detached**, and make it an **executable
  CI conformance test** (sealer signs → pinned `cosign` binary verifies → gate the build).
  v0.3 sigstore-bundle emission is a v0.2 item tied to sigstore-rs maturing.

### T3 — Canonical/reproducible hashing: avoid by construction
- **Hash file *contents* individually** (not the tar byte-stream); sort `hash_tree` entries
  by path for a stable root. Tar ordering / timestamps / zstd settings become irrelevant.
- **Signed blob = the literal bytes of `manifest.json`** (which contains the hash tree over
  all other files). Zero JSON canonicalization; exactly cosign's blob model.
- Cross-capture reproducibility is NOT required — only sealer↔verifier agreement on the
  same bytes. Write this into `spec/IEB-SPEC.md` before any sealer code exists.

### T4 — `lapilli demo` is the E2E test wearing a UX hat
- It exercises the whole product (kind + CRDs + controller + collectors + timing-sensitive
  `previous=true` + seal + sign + PVC + verify + cosign). Build it as the **standing kind
  E2E harness from milestone 1**, run every commit — not a week-N polish task.
- For determinism: `lapilli demo` POSTs the webhook payload directly (or Alertmanager with
  `group_wait: 0s`); pre-stage the crashing workload; pre-pull images.

### T5 — Further v0.1 cuts to make the estimate honest
- **Static-key only** in v0.1; KMS behind the pluggable trait → v0.2.
- **Keyless spike** is separable and OFF the critical path (it de-risks a *bonus*).
- **Self-reported (unverified) image digest** only; real SLSA provenance → v0.2.
- **Deterministic redaction** yes; **signed pre-redaction Merkle root** → v0.2/v0.3.
- RFC 3161 TSA / TUF snapshot: documentation-only in v0.1, zero code.

### T6 — Honest effort: ~12–14 weeks solo from zero (the "10" is not credible)
First milestone = a **tracer bullet** through every risky seam with minimum code:
> webhook POST → `IncidentCapture` CR → controller runs **one** collector (log tails incl.
> `previous=true` — the money collector and the timing-sensitive one) → sealer writes
> `manifest.json` with a content hash tree → static-key ECDSA signs manifest.json → PVC
> export → `lapilli verify` + pinned `cosign verify-blob` accept it — all wired as a kind CI
> E2E from day one.

Everything else (collectors 2–3, changes.json, redaction, coverage, KMS, demo UX) is
additive fill-in on a spine already proven end-to-end.

## Convergence status
Technical lens: **converged** (feasibility HOLDS with T1–T6 applied; corrected scope +
tracer-bullet milestone defined). Strategic lens: **resolved by decision** (Path C).
No further adversarial round needed before Ship 1; the open items (buyer validation, a
second-party format consumer, co-maintainer, named-control mapping, adopter signals) are
**field validation**, not design questions — they resolve through building and shipping.
