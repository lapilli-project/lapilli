# Design Review — Round 20 (closing v0.2: two designs refused, twenty documents corrected)

Constitution: **Loop Engineering Constitution v0.5.0**. Run as a seven-agent workflow: two design
agents on the two unbuilt v0.2 items, two hostile lenses per design, and one documentation-truth
audit, all reading the tree at `db43359`.

## Round 0

- **Category:** roadmap scope plus two design/spec proposals, one of which would touch the
  **frozen** `ieb/v1` format. Irreversible in the way that matters: a format member added before
  the first tag is a member forever.
- **Break-even:** cost ≈ 7 agent runs. Downside: building either feature wrong, or closing v0.2
  on a scope nobody checked. Downside ≫ cost.
- **Sizing:** 2 designers × 2 lenses each, one round, plus an independent audit.
- **Independence: not achieved (calibration only).** Every agent was Claude. What the lenses did
  buy was *citation verification*, and that turned out to be the round's whole yield.
- **Roles.** The designs were written by agents and judged here, so author ≠ arbiter for those.
  The documentation fixes were applied by the arbiter, which makes them author-graded; the gate
  is the only check on them and that is recorded as a limit, not resolved.

## The finding that shaped the round

Both designs were **refused, and neither was implemented.** Not because their conclusions were
wrong — the conclusions survived — but because their reasoning could not be trusted:

> **Three of the four lenses independently reported that the designer's citations point at lines
> that do not contain the claimed text.** One counted six load-bearing citations that fail,
> including the single sentence the deferral argument rests on, cited twice.

A design whose citations are fabricated cannot be revised into a correct one, because there is no
way to tell which of its claims were read and which were generated. Two further self-inflicted
defects confirmed the diagnosis: one designer inserted an unmeasured `~40 s` despite the brief
forbidding invented numbers, and the other built its recommendation on the **stale roadmap table**
— concluding that KMS signing was "still-unshipped v0.2 work" when `lapilli-kms`, the chart wiring
and a twelve-step E2E suite have been in the tree for days. The audit agent, running in parallel,
independently found that same stale table. One agent was misled by exactly the defect another was
sent to find.

## Dispositions

| # | Item | Disposition |
|---|---|---|
| R1 | **signed pre-redaction Merkle root** | **MOVED TO v0.3.** The conclusion survived all four probes and two of its legs were verified here by hand. `spec/IEB-SPEC.md:141` reads *"A redacted value becomes `"<redacted>"`. No hash and no length are emitted."* — a deliberate promise. An unsalted commitment over pre-redaction bytes reverses it and becomes a **brute-force oracle for exactly the values redaction removed**, which is decisive for a small value space (an internal hostname, a six-digit code). And the adversary it claims to catch is already covered: the hash tree binds every byte. A sound version needs a second key custody, which belongs with v0.3's other custody work. |
| R2 | **event trigger without an alert rule** | **MOVED TO v0.3, premise still unestablished.** Round 16 returned it once; the fresh proposal repeated the error in different words — one lens called its central refutation a category error, since `kubectl apply` of an `IncidentCapture` is the *manually-invoked collector* `DESIGN.md` §2 lists as a non-goal, not a trigger. Nobody has shown that an operator cannot write an alert rule. |
| R3 | **the designs themselves** | **NOT IMPLEMENTED, and not revised.** See above. If either feature is taken up later it starts from a fresh reading, not from these documents. |
| R4 | **v0.2 scope** | **CLOSED BY RE-SCOPING.** With R1 and R2 moved, the only v0.2 item left unbuilt is the postmortem draft, whose design was already reviewed and revised in round 18. The roadmap table now strikes KMS signing, SLSA provenance and retention — and says plainly what is *unverified* about the first two: KMS has run only against emulators, and the provenance workflow has never executed because no tag exists. |
| R5 | **twenty stale documentation claims** | **ALL CORRECTED**, each verified here before the edit rather than taken from the agent. Six were **overclaims** — a document saying more than is true — and this project treats those and underclaims as the same defect. |

## The two worst overclaims were in the frozen spec

`spec/IEB-SPEC.md` is the contract an independent implementer reads, and the whole
"vendor-neutral portable format" claim rests on it. Two of its sentences were wrong:

- *"`lapilli verify` accepts either a `.ieb` file (unpacked to a temp dir, with
  path-traversal/symlink hardening)…"* — **it never did that.** `verify.rs`'s own module note
  reads "a `.ieb` file is **not extracted**"; entries are hashed as they stream, in one pass, with
  no seeking. `unpack` exists but only `lapilli unpack` and `demo` call it. The spec pointed a
  third-party implementer at a different verification model with a different threat surface.
- *"…and a note for a bundle without `redaction.json`"* — it is **FAILED**, exit 1, codes
  `integrity` and `manifest`. This contradicted §7 of the same document (*"`redaction.json` MUST
  be present"*), and the frozen fixture `fail-no-redaction.ieb` has pinned the real verdict since
  v0.1.0. The sentence was wrong on the day it was written, and the project's own regression test
  had been enforcing the opposite all along.

Correcting a sentence that was never true is not a format change; it is making the document say
what the fixtures already enforce. Both edits keep the old wording quoted, so the correction is
auditable.

The third overclaim was `DESIGN.md`'s: it advertised **"cosign v2.x pinned + an executable CI
conformance test (sealer signs → pinned `cosign verify-blob` accepts → gate the build)"**. CI runs
no cosign at all. `scripts/verify-conformance.sh` uses **openssl**, and its own comment records
why: cosign v3 removed detached `verify-blob`. The replacement is the stronger anchor — it proves
the signature is standard ECDSA-P256-SHA256 rather than proving one CLI version accepts it — but
the plan claimed a gate that did not exist, which is the documentation form of a guard that cannot
fail.

## Verdict

**v0.2 is closed by scope, not by construction** — and that is the honest outcome, because the two
remaining items turned out to be one feature that reverses a deliberate spec promise and one whose
premise nobody has established. **DRY for this round:** no unresolved in-scope blocker remains
against the re-scoped v0.2. What is *not* claimed: the documentation corrections were written by
the arbiter, so they carry no independent review beyond the release gate; and the lenses were all
Claude, so the citation checking they did is calibration against a shared prior, not independence.

The round's transferable lesson is about the loop, not the product: **an agent handed a stale
document will build on it.** One designer's entire deferral gate rested on a roadmap cell that was
wrong, while another agent was finding that exact cell. The audit should run *before* the designers
next time, not beside them.
