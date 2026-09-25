# Design review — round 27 (identity: an owner-raised objection)

Constitution: **v0.6.0 → v0.7.0** (this round amends it). Target: not one document but the
*direction* of rounds 23–26, raised by the owner in one sentence after the round-26 report:

> 팔리는 것을 너무 생각해서 HolmesGPT에 의존된 것 같다. 처음의 Lapilli처럼 자아를 가진 게 낫다.

The owner then asked the arbiter to judge which earlier identity was the strongest.

## 1. The finding

**The market lenses redefined the subject, not the pitch.** Constitution v0.6.0 made two
lenses mandatory for strategy rounds — *the incumbent's user* and *frequency & trigger* — both
market lenses. Under them, round 23 narrowed the recorder to what the incumbent's user lacked
(the perishable profile), and round 26 defined success as *"a named Lapilli entry in
HolmesGPT's toolset list — a PR to another project"* (F5), which the arbiter logged as
valid-out-of-scope and then adopted as the design document's goal and the report's conclusion.
The critic was reasoning correctly under the lenses it was given; the lenses were incomplete.

Steelman: *the lenses only ever asked what an adopter needs, and adoption is the first goal.*
True, and insufficient: a lens that asks only what others lack will always answer with a
smaller product aimed at someone else's roadmap. Nothing in v0.6.0 asked whether the change
still described Lapilli in Lapilli's own terms. Refutation fails; the finding holds.

## 2. The judgement the owner delegated

Three identity statements exist in the record:

| when | statement | what it is |
|---|---|---|
| **round 1 (2026-09-17)** | triggered by **operational** signals · correlating **K8s-native state across a symmetric window** · sealed as an **open, portable, offline-verifiable evidence format** | **the identity** — trigger, content and artifact, all in Lapilli's own words |
| round 2, Path C | "the open incident flight recorder"; signing, audit and *standard* are words to earn later | a **shipping posture** — what not to *claim* yet, not what Lapilli *is* |
| rounds 23–26 | "the object body and diff are the only surviving claim; serve where the bundles are, to agents" | a **reaction** — defined by the incumbent's gaps and another project's consumer |

**Round 1's seam sentence is the strongest.** Path C never replaced it; it only counselled
restraint on three words, and that restraint still stands. The drift was not Path C — it was
letting the market lenses rewrite *content* and *audience* when their mandate was the *pitch*.

## 3. Applied

- **`DESIGN.md`** now opens with the identity as a checked constraint: operational trigger,
  correlated window, open/portable/offline-verifiable file, and — the part the last rounds made
  explicit — *Lapilli verifies and reads that file itself*, depending on no observability
  vendor and no AI tool.
- **Kept, because it passes the check:** the full recorder as the default (`deferred: []`),
  `incident.target` (the bundle says what it is about — more self-contained, not less), the
  permissions-follow-profiles work (product quality), phase B returned to premise (it would have
  made Lapilli a client of other stores), and `lapilli mcp` **as Lapilli's own interface to its
  evidence** — a standard protocol, any client.
- **Withdrawn:** the round-26 F5 framing. `docs/design-distribution-path.md`'s goal is rewritten;
  `integrations/holmesgpt/` is one worked example of a consumer and says so; the report's
  "next step is the maintainer conversation" is no longer the project's conclusion.
- **Constitution v0.7.0:** a new principle — *the subject's identity is a constraint the market
  lenses operate under, not an output they may rewrite* — an **identity lens** added to the
  mandatory floor for strategy/positioning rounds, and this drift named in Revisions.

## 4. Verdict

**Applied.** Not a code round: nothing in the tree moved except prose, and that is the point —
the code had kept its identity while the narrative around it had not. The check that would have
caught this two rounds earlier now exists in the constitution the loop runs under.

## 5. What this round does not decide

Whether signing, audit and "standard" should move from *earned later* back toward the pitch.
That is a Path C question, owner's, and this round explicitly leaves Path C's restraint in
place.
