# Design Review — Round 3 (convergence confirmed → DRY)

Round 3 re-attacked the Path C revision with 3 lenses (Path C skeptic / TAG differentiation,
cross-doc consistency + integrity soundness, tracer-bullet feasibility sign-off). **No lens
returned a new architectural blocker.** Every finding was a wording/doc fix or concrete build
guidance. The design loop is therefore **DRY** — cleared to start coding Ship 1.

## Verdicts
- **Path C skeptic:** CONDITIONAL-YES, no architectural blocker. Path C dissolves the
  round-2 TAG-homelessness ping-pong at its root; OpRes charter fit is clean.
- **Consistency sweep:** CLEAN across README/DESIGN/IEB-SPEC (signing-off-default, static-key
  v0.1, audit-supporting, reference-layout, cosign-v2 pin, roadmap tables all agree; all
  links resolve; retired phrasing gone).
- **Tracer-bullet feasibility:** YES, ready to code. 3-crate workspace + kind-CI recipe +
  cosign DER contract all concrete; ~12–14 wk full v0.1, tracer ≈ 3 wk.

## Fixes applied this round
1. **`attestation/` → `signature/`** (residual of the dropped "attestation" concept) — DESIGN §4, IEB-SPEC ×2.
2. **Dropped "symmetric `[t-Δ, t+Δ]` window"** — v0.1 cannot backfill the pre-incident side
   (it's subject to the very evidence horizon it beats on the forward side). Reframed as
   **timing, not a time-machine**: "captured at alert-time-plus-seconds, before the volatile
   evidence finishes rotating." — DESIGN §3, README.
3. **Stopped overselling "correlation"** — v0.1 `timeline.json` is an ordered merge across
   sources, not causal correlation. Now framed as **timing + completeness**; causal links
   are v0.2. — DESIGN §3, README.
4. **Deleted "effort + taste moat"** and **stated the why-CNCF explicitly** (vendor-neutral
   portable incident evidence any tool can produce/consume is shared infrastructure the
   ecosystem should hold) — DESIGN §3, README.
5. **README headline** now leads the SRE win with timing/before-GC; portability is the
   SaaS-defeater one line down (it's table stakes vs open tools). — README.
6. **Corrected TAG framing** — Sandbox entry is a lightweight TOC decision; TAGs don't
   gate/sponsor it (TAG alignment is an Incubation signal). "Home/sponsor" → "TAG fit at
   Incubation review: Operational Resilience." — DESIGN header + §9.
7. **Scoped the §7 "collector RCE can't sign" claim to KMS/v0.2** — v0.1 static-key shares
   the controller trust boundary; signing is off by default so it affects only opt-in signed
   deployments. — DESIGN §7.
8. **Wrote the cosign signature-encoding contract into IEB-SPEC** — DER (`to_der`) not
   P1363 (`to_bytes`); low-S; PKCS#8 private / SPKI PEM public; the DER→base64→`cosign
   verify-blob` round-trip is the CI conformance gate. — IEB-SPEC.

## Carried into Ship 1 as build requirements (not doc fixes)
- **Negative-path E2E from day one:** the load-bearing feature is `lapilli verify` failing
  closed. The kind E2E must assert (a) tamper one byte → verify fails on hash mismatch;
  (b) force a collector error / `collectors_run < collectors_intended` → verify exits
  non-zero on PARTIAL. Without these, the integrity claim ships untested.
- **kind-CI gotchas:** SHA-tagged image + `imagePullPolicy: IfNotPresent` (after
  `kind load docker-image`); gate the webhook on workload `restartCount >= 1` so
  `previous=true` is deterministically present; pin `kindest/node:vX.Y.Z`.
- **Workspace:** `lapilli-bundle` (no kube deps: manifest/hash/seal/verify/sign — single
  source of truth shared by sealer & verifier) · `lapilli-controller` (kube-rs, CRDs via
  kube-derive + a `crdgen` subcommand with a committed-YAML diff check) · `lapilli-cli`
  (`lapilli verify`, `lapilli demo`; depends on `lapilli-bundle` only).

## Tracer-bullet task breakdown (~3 weeks) — see the round-3 feasibility report
Spine 1→2→3→4 (workspace/CI → CRDs → webhook→CR → reconcile phase machine), then the
crypto/verify triangle 6+7+9 in `lapilli-bundle` (unit-testable without a cluster), then
5+8 (previous-log collector + PVC export), finishing 10+11 (cosign conformance + kind E2E
**with the negative tests**). Milestone is "green" only after the negative tests pass.

## Status: DRY. Cleared for Ship 1.
Open items are field validation (co-maintainer, adopter signals, named-control mapping,
an independent IEB consumer), which resolve by building and shipping — not design questions.
