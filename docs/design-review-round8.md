# Design Review — Round 8 (`kairn verify --output json`, `verify-result/v1`)

Constitution: **Loop Engineering Constitution v0.5.0**. Artifact:
[`spec/VERIFY-RESULT.md`](../spec/VERIFY-RESULT.md), the problem codes in `kairn-bundle`,
and `crates/kairn-cli/src/verify_cmd.rs`. Snapshot attacked: working-tree diff
`f7446765ba20`.

**Outcome: time-boxed, not dry.** One round. Every finding was APPLIED or given a recorded
disposition. The fixes are enforced by the fixture contract test, which was mutation-checked
and by MinIO runs, but they were not re-attacked by a second critic round.

## Round 0

- **Category:** a public compatibility contract that freezes at the first release. Within
  v1, anything wrong is permanent.
- **Break-even:** cost ≈ 3 critic runs. The downside is a forced `verify-result/v2` soon
  after release, or CI and SIEM consumers misrouting tampering. Downside ≫ cost.
- **Sizing:** medium, 3 lenses × 1 round: contract/compatibility, consumer (CI gate and
  SIEM), and correctness against HEAD.
- **Independence: not achieved (calibration only).** All critics were Claude. The human or
  non-Claude review gate in `RELEASE.md` still applies.

## Findings and dispositions

Findings raised by more than one lens are listed once, with their sources.

| # | Sev | Source | Finding | Disposition |
|---|---|---|---|---|
| 1 | BLOCKER | contract, consumer | `bundle.*` reported defaults (`hash_ok:false`, `signature:"absent"`) for checks that never ran, typed as non-null, so it could never become null within v1. | **APPLY.** `bundle` is null when no manifest could be read. The checks are null exactly when the format's rules didn't run (`producer_version` null), and the test enforces that. |
| 2 | BLOCKER | contract | The "verdict it comes with" column was false. A digest mismatch overrode CANNOT_EVALUATE, and a failed version listing turned a FAILED bundle into CANNOT_EVALUATE while discarding the bundle. | **APPLY.** Precedence is written down (FAILED > CANNOT_EVALUATE > PARTIAL > OK) and applied in one place (`Outcome::fail` / `unresolved`). The column now says what a code gives on its own. A failed listing keeps the bundle and `input`, and adds `unreadable`. |
| 3 | BLOCKER / MAJOR | consumer, contract | An unusable `--key` was reported as FAILED `signature`/`invalid`, which reads exactly like forgery. A missing key file was reported as exit 3. | **APPLY.** `--key` is read and parsed before verifying. Any failure is CANNOT_EVALUATE `unreadable`. Pinned by fixture cases. |
| 4 | MAJOR | consumer, correctness | The local file was hashed and verified through two opens: time-of-check/time-of-use (TOCTOU), a FIFO hang, `/dev/stdin` misjudged, and an unbounded hash. | **APPLY.** The file is opened once, through the same hashing, byte-limited reader as remote objects. The file is hashed to its end only when `--expect-sha256` is given or the bundle's rules ran (`/dev/zero`: 1 s). Probed with stdin and a two-writer FIFO. |
| 5 | MAJOR | consumer | `custody` meant both "written twice" and "listing denied"; a deleted key read as "no such object". | **APPLY.** A denied listing is `unreadable`, and `history.state` is `unavailable`. A key with delete markers and no current object is `custody` (FAILED), with its history. `problems[].reason`: **VALID-OUT-OF-SCOPE**; it is additive and can come later. |
| 6 | MAJOR | contract, correctness | Code drift and a dead branch: a non-JSON manifest was coded `manifest`; the duplicate-key check ran before `schema_version` dispatch (against IEB-SPEC §9); path-rule violations in the listing were coded `integrity`; with no manifest, only the first container problem was reported. | **APPLY.** Dispatch runs first. A non-JSON manifest is `not-a-bundle`. Listing path violations are `structure`. With no manifest, all container problems are reported plus `not-a-bundle`. Two fixtures' pinned codes changed accordingly, before release. |
| 7 | MAJOR | contract | The evolution rules were inconsistent across the two docs; `signature` couldn't absorb planned signature kinds; moving a condition to another code wasn't forbidden; the test checked presence but not types. | **APPLY.** Each member is marked open or closed, with fail-safe rules for unknown values. `signature` is defined as a summary, with details reserved for a later array. "Move a condition to a different code" is forbidden. The test checks every member's type and the null rule; mutations (a renamed code, a retyped member) were confirmed to fail it. Fixtures were added for context, digest and an unusable `--key`. |
| 8 | MINOR | consumer | Closed stdout panicked (exit 101). | **APPLY.** JSON mode exits 3 with no panic. Text mode keeps the verdict's exit code (`| head`). The spec says any other exit code means "no result". |
| 9 | MINOR | correctness | Exit changes weren't in the CHANGELOG: `--expect-sha256` on a directory 3 → 64, and a digest mismatch on a can't-evaluate bundle 3 → 1. | **APPLY** (CHANGELOG). Both directions are kept: a usage error, and fail-closed on a known digest mismatch. |
| 10 | MINOR | consumer | Missing information: verification time, key id, trigger/window, hash root. | **VALID-OUT-OF-SCOPE.** All of it is additive within v1. |

## Verdict

**Time-boxed, not dry.** Every BLOCKER and MAJOR is APPLIED. The only exceptions are the
additive items recorded as out of scope (5's `reason`, 10), which the evolution rules allow
later without a v2. The contract is enforced by:

- 43 fixture cases in both output modes, with their problem codes pinned, including the
  operator-input cases;
- a type and null-rule check of every document, mutation-checked;
- MinIO runs of the precedence cases: deleted key, denied listing on a good bundle and on a
  tampered one, and `--current-only`.

No second critic round attacked these fixes.
