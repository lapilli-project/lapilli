# Design review — round 34 (measure the premise, not the proposal)

Constitution: **v0.7.1**. No target document, which is the point: rounds 31, 32 and 33 each attacked
a proposal and each returned it to premise. This round attacked **premises** instead, and shipped
`v0.2.0`.

**Independence: not achieved.** Same limitation, fourth round running.

## Verdict: the inversion worked — four defects, no proposal

Three rounds had cost three proposals. Reversing the order — measure what a proposal assumes
*before* writing it — found four defects in shipped code instead, one of them a privacy defect in
`SECURITY.md`'s own stated scope. Nothing was proposed and nothing was returned to premise.

| # | Premise measured | What the measurement found |
|---|---|---|
| 1 | "An alert-triggered recorder reaches the incident window" | **False in a bounded way.** A CronJob's failed pod is deleted when its next run is created, and `KubeJobFailed` is `for: 15m`, so anything scheduled more often than its alert's `for:` destroys its own evidence first. Measured on kind 1.37: present at t+0, gone at t+1 |
| 2 | "`off < default < strict` is an ordering" (precondition for an admin floor) | **False.** `Mode` derives no ordering and `Off` sorts last while being weakest. 25,200 cases showed `strict` with a non-empty `plaintext` redacted **less** than `default`: an exempted name skipped redaction entirely instead of only strict's widening, so a GitHub token and a bearer header survived. [GHSA-7994-x9mx-vx43] |
| 3 | "The drop counter makes alert loss visible" | **False for half of it.** Four reasons were counted; the render carried its own list of two, so `bad-firing-ts` (shipped in 0.1.0) and `bad-rule-name` never left the process |
| 4 | "Escaping makes a crafted bundle harmless" (`ROADMAP`'s own words) | **Harmless, not small — and it amplifies.** `md` is exactly 2.0x, `code` 3.0x on a backtick run. A 1,615-byte bundle that verifies OK rendered 1,051,015 bytes; the same channel at 256 MiB rendered 268 MB |

A fifth measurement was a correction rather than a defect: round 33 held that OSC 8 and OSC 52 pass
through the Markdown escape. Checked at both surfaces — **refuted.** `invisible` begins with
`char::is_control`, which covers ESC and BEL, so the sequence is flattened. Kept as a test on each
surface rather than a note here, because the claim is plausible enough to be raised again.

## 1. The one that cost a revert, and what it bought

Fixing #3's neighbour — making the retention sweep run on a default install, so a `.unsent`
hand-off's workload content is collected as round 30 intended — broke the kms E2E on `main`:

```
FAIL (kms): kms-outage never reached Exported (phase Failed: staging-lost: …)
```

**It was reverted the same day, before it was understood.** That is the disposition worth recording.
The unit test said a live capture's staging directory is refused; the cluster said a file was gone;
`decide`, `holds_uid` and `abandoned_tail` all read correctly. Two of my hypotheses were wrong and I
checked both rather than fixing either — a namespace-scoping theory (refuted: the same `ic_api` that
had just listed 19 CRs) and "the sweep deleted the staging directory" (refuted: both directories
were still on the node).

The journal answered it. `reclaimed.jsonl` named **`.staging-<incident>-<uid>.seal.json`**, twice,
with `reason: "abandoned"` while both captures were live. `sealing::seal_file` is the staging path
with `.seal.json` appended, so it is a *sibling* whose name also begins with `.staging-`;
`abandoned_tail` stripped that prefix and returned a tail ending in `.seal.json` rather than in the
uid, so `holds_uid` could never match a live capture. **The file that exists to survive a KMS outage
was deleted during one.** Wrong from the day it was written, unreachable only because the sweep did
not run with no bound set.

So the revert did not lose a day: enabling the sweep is what made a latent defect reachable, and
reverting is what kept an un-understood regression off `main` while the journal was read. The
journal is also the artifact that answered it, and the kms suite's failure handler now dumps it —
read from the node, because the controller image is distroless and the `kubectl exec` I first wrote
answered nothing.

## 2. What the round taught the method

**Measure the premise, not the proposal.** Three rounds of critique killed three documents; one
round of measurement killed four defects. A critic attacks what is written; a measurement looks at
the world. Round 33 had already written the sentence — *"point a lens at the input, not the
design"* — and this round is what it looks like applied before a design exists rather than after.

**A proxy assertion freezes the wrong belief.** `test/e2e/run.sh` asserted "retention did not sweep
on a default install" as `lapilli_retention_sweeps_total{result="ok"} == 0`. Its own comment claimed
two things — the counter is zero *and* nothing was reclaimed — while checking only the first, which
held as a proxy because the sweep did not run at all. It now asserts the property: no bundle taken
for the byte ceiling, for age, or for a missing `IncidentCapture`.

This is the **second** time in this project: `helm-renders.sh` asserted `captureprofiles` RBAC was
"get only" where the property was "no write", and that froze a correct fix. The general form, worth
keeping: **a counter at zero, a missing file and an absent log line can all mean "that path did not
run" rather than "it did not happen."**

**Verify a claim at the site it governs.** Three mutation proofs this round were first done at sites
the check did not govern. And a claim about a document is verified by reading the document from the
test: the metrics table is now parsed out of `docs/metrics.md` in both directions, so a reason the
document omits and a reason it invents both fail.

**A release note that records its own mistake is worth more than one that does not.** `v0.2.0`'s
entry says the retention fix shipped once and was reverted the same day, and why. A reader deciding
whether to trust this project learns more from that than from a clean list.

## 3. Dispositions

| # | Finding | Disposition |
|---|---|---|
| 1 | Trigger reachability is bounded by the alert's `for:` | **BOUNDARY, documented** — `docs/design-trigger-reachability.md`; README and DESIGN §3 state it; the coverage thread closes with it rather than with a feature |
| 2 | `strict` + `plaintext` redacted less than `default` | **FIXED** in 0.2.0, invariant pinned by a property test, advisory published |
| 3 | Two drop reasons counted and never rendered | **FIXED**; reason is an enum both sides go through; the test reads the render and the document |
| 4 | The document was unbounded from a small bundle | **FIXED** at the leaf (`Summary`), which also closes the MCP `summary` tool; 43 frozen fixtures render byte-identically |
| 5 | A live capture's seal file was reclaimed | **FIXED** after a same-day revert; regression test takes the filename from `sealing::seal_file` |
| 6 | OSC 8 / 52 pass the Markdown escape (round 33) | **REFUTED**, with a test on each surface |
| 7 | `main` had no protection while `CONTRIBUTING.md` described a PR flow, and its DCO promise had no check | **FIXED**: ruleset with 13 required checks, a DCO job, CODEOWNERS, a PR template; the bypass is named in a table |
| 8 | The advisory gate was red for a permission reason, not a vulnerability | **FIXED**: moved to the cargo-deny action already pinned for `check bans`; no token, no elevated permission, `deny.toml` written out instead of inherited |
| 9 | `bans` and `advisories` were CI-only | **FIXED**: both run in `release-check.sh`, skipped with a message rather than silently |
| 10 | Round 16 candidate 3 — a trigger on Pod status | **OWNER'S FORK**, unchanged. It reverses `DESIGN.md` §8.3 and round 16 measured ~1700 bundles/day |

## 4. What this round did not close

**The first outside install.** `ROADMAP.md` §2 criterion 3 is the whole of the next leap, and no
measurement reaches it. v0.2's own scope is "whatever the first installs report", so starting it
without an adopter is guessing — which is precisely what rounds 31 to 33 did and what this round
avoided by measuring instead.

**`lapilli verify` does not report the redaction exemption list.** Left out of #2's fix on purpose:
it is the one command whose output carries a compatibility commitment, so a new line in it is its
own change (`ROADMAP.md`).

**Round 33's YAML dispositions** stay unapplied and stay correct: `serde_yaml` alias fan-out is
×146 and a 50 MB scalar is accepted, so any successor that parses YAML needs caps and a fuzz
target. Nothing parses YAML yet.
