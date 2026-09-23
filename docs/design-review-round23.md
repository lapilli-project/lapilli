# Design review — round 23

Constitution: **v0.6.0** (vault `13 Playbooks/Loop Engineering Constitution`).
Target: `docs/design-record-and-seal.md` — the resolution of a fork raised by a market
review. Snapshot attacked: **`bf683e5`**.

## 0. Scope and gates

**Target.** One document, whole. It reifies an owner decision, so the decision itself is
what is under attack, not code — nothing has been implemented.

**Category gate.** Architecture decision touching a **frozen** format. Passes.

**Break-even.** Cost: 3 critics × 1 round + arbiter reading. Downside if wrong: a wrong call
here either breaks `ieb/v1` (which an independent Python producer implements and CI pins) or
paints the project into a corner it cannot leave without a version bump. Cost ≪ downside.

**Sizing.** Hard to reverse + frozen format → **full**, 3 lenses. The `cheap-first` 2×1 path
does not apply.

**Roles.** Author = Claude (the draft), arbiter = Claude, owner = user. Arbiter = author, so
BLOCKER/MAJOR dispositions are subject to independent re-attack; the round-2 wave carries that.

**Independence — honest.** All three critics are Claude-family: **weak independence,
calibration only.** No human or non-Claude critic. Mitigating fact: the three lenses reached
the same BLOCKER by three *disjoint* evidence paths, which is stronger than three agreements
from one path — but it is still not independence.

**Lenses.** (A) format and spec conformance · (B) incumbent operator who already refused the
install · (C) evidence integrity, explicitly tasked to find a reopened prior fix.

## 1. Findings

| # | Lens | Sev | Objection |
|---|---|---|---|
| R1 | A, B, C | **BLOCKER** | "Two bundles sharing an incident id is normal" reopens round 17's closed BLOCKER and is enforced against by four separate code paths |
| R2 | A, C | **BLOCKER** | `diffs` is not a collector name — it is `changes`. As written phase A verifies PARTIAL, contradicting the document's own claim 1 |
| R3 | A, C | **BLOCKER** | `*-backfill` names are legal *and therefore inert*: rule 6 gives unknown names no required files, so `coverage=100%` on possibly-empty directories. Legal but unchecked |
| R4 | B | **BLOCKER** | The RBAC does not shrink by one verb; `pods/log` and `events` are granted unconditionally, and tightening them by hand triggers a permanent false permission alarm |
| R5 | B | **BLOCKER** | The 19.4 KB / 62-day figure is controller memory per CR, not bundle disk bytes, and the CR is created before any collector runs — so the split moves the clock by zero |
| R6 | C | MAJOR | Signing is never mentioned; both options break the documented trust root (key on a laptop, or the only complete bundle unsigned) |
| R7 | C | MAJOR | A sealed bundle's `redaction.json` would assert a policy never applied to backfilled files, and backfilled event text bypasses a mandatory redaction rule |
| R8 | B | MAJOR | `seal` swaps a namespaced, audited in-cluster read for a human-held, broader, unaudited Loki tenant credential |
| R9 | A, C | MAJOR | Phase A's OK is correct on the letter of the spec, but OK + `coverage=100%` now means strictly less than before and nothing says so |
| R10 | A | MINOR | `capture_to_seal_ms` for a day-late seal is byte-identical to a controller that hung for a day; nothing reads `timing` |
| R11 | A | MINOR | The "readers MUST ignore fields they don't know" citation is scoped to `manifest.json`, so it authorizes nothing about index schemas |

## 2. Judge — steelman attempts

**R1.** No refutation attempted; three lenses, four enforcement points
(`export.rs:183`/`:327`, `verify_cmd.rs:136-142`, `remote.rs:337-341`,
`retention.rs:222-230`), and a named prior round. **APPLY.**

**R2.** Verified directly against `spec/IEB-SPEC.md:311` and `manifest.rs:24`. **APPLY.**

**R3.** Steelman: *"the manifest could declare the backfill instead, keeping rule 6 untouched."*
This survives partially — a manifest field is legal and is where a consumer looks. But it
leaves required-files unenforced for the backfill directories, which is the substance of the
objection. The refutation fails on the substance. **APPLY**, and the document now *admits*
the rule-6 amendment rather than claiming no spec change. Merged with R2 as one root cause:
**a new collector name is unverifiable under the frozen table.**

**R4.** Steelman: *"most of that RBAC is genuinely needed — phase A reads pods and the apps
resources."* True, and it does not touch the objection, which is precisely about `pods/log`
and `events`. Refutation fails. **APPLY** — and this is load-bearing: it is the single change
the operator named as flipping a *would-install: no*.

**R5.** Steelman: *"disk still matters somewhat."* Refuted by measurement — p50 6.2 KB against
a 1Gi default. **APPLY** as a straight correction.

**R6.** Steelman: *"phase B could sign with a separate seal key, and the key id would let a
consumer distinguish."* Plausible, but it preserves the distinction only for consumers who
pin the controller's key, and a workstation key that mints `verify --key` passes is a strictly
worse trust root than no key at all. The critic's resolution costs nothing by comparison.
Refutation fails. **APPLY.**

**R7.** No refutation found. **APPLY.**

**R8.** Steelman: *"the operator could scope the token themselves."* Does not address that the
read leaves the cluster audit trail. **APPLY** (in-cluster Job + a zero-credential
`--dry-run`).

**R9.** Two lenses reached this from opposite directions and **converged on the same fix**:
keep OK (PARTIAL would be a lie about a collector that never failed —
`spec/IEB-SPEC.md:299` is if-and-only-if) and add a notice. **APPLY.**

**R10, R11.** **APPLY** as written; both are corrections, not design changes.

**REFUTED: 0.** As in the market-analysis round, a first draft that had never been attacked
produced no refutable objections.

### Claims that survived attack, with evidence

Recording these because a clean axis is a finding:

- **Minimal-collector bundles verify OK.** `test/spec/build_from_spec.py:46` ships a
  `logs`-only bundle and CI verifies it (`.github/workflows/ci.yml:64`). The only
  unconditional file is `redaction.json` (`verify.rs:867-876`). Claim 1's substance holds;
  only the *name* was wrong.
- **A manifest parent pointer is format-legal.** `build_from_spec.py:52` deliberately ships
  `x_future_field` to prove an unknown manifest member still verifies OK.
- **The object body really is unreconstructible.** Three critics independently looked for a
  path through kube-state-metrics and the API audit log and none found one. This is the only
  thing keeping the design alive.
- **The "no log shipper" segment is real**, but the discriminator is the log shipper, not
  Prometheus — Grafana Alerting, `vmalert`, Thanos Ruler and the Mimir ruler all POST to
  Alertmanager. The draft's framing was confused; the segment is not.
- **New `*-backfill/` paths cannot evade the hash tree**, duplicate-path or case-collision
  checks (rules 2, 3, 10 apply per-path with no collector carve-out).

## 3. Applied

`docs/design-record-and-seal.md` rewritten. Substantive changes: sealed bundles get their own
id `<parent>-s1` with a `parent` reference block and are never written to the controller's
bundle root; `seal` MUST NOT hold a signing key and always writes `signing: null`, carrying
the parent's signature instead; rule 6 gains admitted rows for the backfill names; a
`timeline.json` entry from a backfill MUST carry `source` + `retrieved_at`; `seal` MUST
rewrite `redaction.json` or declare `mode: "off"`; `seal` is an in-cluster Job; `$collectorRules`
and `perms.rs` `Needs` derive from the profile with a chart test asserting no `pods/log` and no
`events` on a perishable render; the 19.4 KB claim replaced with the true statement that the
retirement clock does not move.

## 4. Verdict

**not dry.** Round 2 is required: five BLOCKERs were folded in one pass and the arbiter wrote
the fix, so every one of them needs re-attack by rotated critics. The specific things round 2
must try to break:

1. Does `<parent>-s1` actually survive all four id-enforcement paths, or does it trip one the
   round-1 critics did not enumerate?
2. Is the rule-6 amendment genuinely additive, or does it change the verdict of some bundle
   that can already exist?
3. Does the notice mechanism reach a consumer that matters, or is it inert in the same way
   the collector names were?
4. Does the RBAC derivation actually hold, given that phase A still reads pods across
   namespaces?
5. Is an unsigned complete bundle plus a signed core actually better than the alternative, or
   does it just move the problem?
