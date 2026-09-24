# Design review — round 25 (phase A implementation)

Constitution: **v0.6.0**. Targets: the phase-A implementation of
`docs/design-record-and-seal.md` and `docs/design-permissions-by-profile.md`, shipped as
`563fe10` (format), `65d462d` (format follow-up), `22674cb` (permissions), `5e4caf8`
(perishable profile), `6a05207` (converge fixes). Two critic waves, one per snapshot pair;
both critics rotated (neither had blessed what it attacked).

## 0. Scope and gates

Category: a frozen-format addition, a controller behaviour change, and a CRD rule. Break-even:
the format change lands before v0.1.0 and the fixture set is not yet frozen, so the cost of
being wrong is a fixture regeneration now versus a `v2` later; the permissions change turns an
advisory check into one a `CollectorDenied` Event cites as a cause, so a wrong table would
publish false causes. Tier: **full**. Independence: both critics Claude-family — **calibration
only** — with one mitigating fact recorded below: the second critic stood up its own kind
cluster and tested the CEL rule and the E2E's restore path against a real API server, which is
evidence, not opinion.

## 1. Wave 1 — the `deferred` contract (`563fe10`)

| # | Sev | Finding | Disposition |
|---|---|---|---|
| A1 | MAJOR | The notice said "Coverage is 100% of what was intended" unconditionally; on a PARTIAL+deferred bundle it printed a falsehood beside `coverage=50%` — the exact defect class the field was added to close | **APPLY** — no coverage claim in the notice; test asserts the notice on a PARTIAL bundle contains no "100%" |
| A2 | MAJOR | `postmortem`, `Summary`, notify rendered a deferred bundle as a full green capture | **APPLY** — `Summary.collectors_deferred` (classified *fact*), postmortem header + inventory, notification verdict part |
| A3 | MAJOR | "Every deferred name MUST be in the table, else FAILED" is a moving allowlist: `COMPATIBILITY.md` makes collector names additive, so a v0.1 verifier would FAIL every future bundle that defers a newer collector — **the moving denylist round 24 killed, rebuilt one field over** | **APPLY** — unknown name is a notice; `fail-deferred-unknown.ieb` → `ok-deferred-unknown.ieb`; COMPATIBILITY coverage bullet lists the two surviving FAILED conditions |
| A4 | MAJOR | `"deferred": null` (a Go nil slice) FAILED at parse where the previous reader ignored it as unknown | **APPLY** — `deserialize_with` null→empty; spec: absent, `null`, `[]` are the same |
| A5 | MINOR | Empty `collectors_intended` scores 1.0 with no code: the one pre-existing path to OK with nothing in the bundle | **APPLY** — notice, verdict untouched |
| — | below threshold | fixture harness's null-exactly-when check omitted the three new members; pre-existing: postmortem's PARTIAL banner filtered on a code named `coverage` that does not exist, so it read "PARTIAL: none did not run" on every partial bundle | **APPLY** both |

Steelman worth recording: A3. *"The rule exists so `deferred` cannot be inert."* True, and it
does not survive contact with the additive-names policy: the inertness concern is about a
name that means nothing, and a notice says exactly that; FAILED breaks forward compatibility.
Refutation fails.

**REFUTED: 0.** Clean axes the critic named: all 47 pinned cases keep exit and code set;
existing `.ieb` bytes identical; explicit `[]` verifies identically to absent; the Python
producer's unknown-field probe still passes; the verdict-line suffix keeps `grep coverage=100%`
matching.

## 2. Wave 2 — permissions and the perishable profile (`22674cb..5e4caf8`)

| Slice | Verdict | Finding | Disposition |
|---|---|---|---|
| S1 needs = union, re-derived | WEAK | `NEEDED_BY` under-claimed `events` for `changes` (`diffs.rs:201-204`, a hard `?`) — so the perishable profile's own advice to drop `events` would break `changes` with the self-check green; over-claimed `pods` for `events`; `resources` reads the owner chain softly, so `denied_for("resources")` named causes that could not have produced the symptom; check comments cited stale lines | **APPLY** — table fixed; **split into `NEEDED_BY` (asked) and `STOPPED_BY` (may be named as cause)**; a test pins that every cause is also a need and that `denied_for("changes")` includes `events`; citations refreshed |
| S2 NotNeeded + metrics | WEAK | `asked` derived as `len − not_needed` counted Unknowns recorded without sending, so the HELP text's "0 means the profiles could not be listed" was unreachable | **APPLY** — counted at the send; HELP and `metrics.md` reworded. The fix then made `held = asked − denied − unknown` underflow in exactly that case — a test caught it — so `held` is counted too |
| S3 CollectorDenied attribution | **HOLDS** | probes: RwLock in async (guard dropped in-expression, no await under it); stale-report windows (NotNeeded never attributes; Denied-then-fixed is a timestamped claim); Event spam bounded at ≤4 per capture; `action: "Seal"` mislabel is inert | — |
| S4 CEL rule | NEW-BLOCKER | The rule itself holds on a real API server (create, update, defaults-first). `config/crd/crds.json` was not regenerated, so the `crd-drift` job and `release-check.sh` failed on HEAD | **APPLY** — both copies are byte-identical crdgen output |
| S5 the E2E script | NEW-BLOCKER | `restore()` re-applied `get -o json` dumps with `resourceVersion`, refused after the suite's own writes and hidden by `\|\| true`; `^PARTIAL ` could never match the demo's indented verdict line | **APPLY** — server metadata stripped, restore loud, anchor `^ *PARTIAL  hash_ok=` |
| add'l | WEAK | `lapilli_deferred_captures_total` incremented before guards that can still refuse the capture | **APPLY** — moved before `collect_all` |
| add'l | WEAK | CHANGELOG and design doc said the E2E "proves" the arc before it had run once — a claim ahead of its evidence, the round-24 pattern | **APPLY** — "exercises" until the run; see §3 |

**What the first E2E run found that no critic did.** The API server refused the CRD:
*"estimated rule cost exceeds budget by factor of more than 100x."* CEL cost is estimated from
the schema's bounds, and `exists(c, c in …)` over two unbounded arrays of unbounded strings is
unbounded. `collectors` and `deferred` now carry `maxItems: 16` / `maxLength: 64`. **No unit
test, lint, drift check or critic reading can see this — only an API server estimates it.**
This is the layer the loop cannot replace.

## 3. E2E

Three reduced runs (`SKIP="diffs export kms notify"`):

1. **Refused at `helm install`** — the CEL cost budget (§2). Fixed with bounds.
2. **`helm install` passed; `cel` step passed** (the API server refused the overlapping profile
   with the rule's own message); **`perishable` step captured on the real cluster**: the demo's
   verdict line read `OK  hash_ok=true context_ok=true coverage=100% (deferred: logs,events)`,
   the bundle held `resources/pod.json` and the `revision 1 → 2` diff and no `logs/`. The step
   then **failed on the script's own assertion**, not the product: it demanded exactly one
   `notice`, and kind's Prometheus window held no series, so the verifier's pre-existing
   empty-PromQL notice stood beside the deferred one. The assertion now requires that every
   code be a notice and that the deferred one be present.
3. Rerun with the corrected assertion: the inventory assertion then failed on its own
   formatting expectation (`summary_list` wraps names in backticks); corrected.
4. **Fourth run, image built from `6a05207`, kind v1.37.0 — all four steps passed:**
   - `cel` — the API server refused the overlapping profile with the rule's own message;
   - `perishable` — `OK … coverage=100% (deferred: logs,events)`, no `logs/`, no `events.json`,
     `resources/pod.json` and `diffs/index.json` present, manifest and `verify-result/v1` carry
     the set, postmortem header and inventory name it, `lapilli_deferred_captures_total` moved;
   - `denied` — with `pods/log` removed from the ClusterRole and the full profile: **PARTIAL**,
     the denial sealed in `logs/index.json` with no log file written, and a `CollectorDenied`
     Event naming `pod-logs` and the self-check's time;
   - `not-needed` — the same tightened Role under the perishable profile: **OK**,
     `lapilli_permissions_denied` back to **0**, `result="not_needed"` counted, and no new
     `CollectorDenied` Event.

   The restore path put the profile and the ClusterRole back without error. That is the arc the
   two design documents describe, observed rather than argued.

5. **Full gate, first attempt (`eac6320`), kind v1.30.0 — failed on a pre-existing step**,
   `run.sh`'s permission negative: empty the controller's Role and expect
   `lapilli_permissions_denied 4`, `lapilli_permissions_unknown 0`, held ≥ 8 — *"a denial must
   not read as unanswerable"*. With the Role emptied, `list captureprofiles` fails **because of
   the denial**, and rule 4 as first written turned that into eight `Unknown` collector checks.
   Neither critic nor the reduced runs reached this step. Rule 4 is now: an unreadable list
   narrows nothing, so every check is asked — a denial is a denial, `Unknown` is for an
   authorizer that did not answer. The design doc, HELP text, `metrics.md` and the unit test
   were rewritten to match; `Need::Unknown` no longer exists.

The full gate (both Kubernetes minors, every suite) on the final commit follows.

## 4. Verdict

**Wave 1: dry after apply** (0 REFUTED, all applied, no NEW-BLOCKER on the fixes' second
look — the wave-2 critic re-read the format change and raised nothing new against it).
**Wave 2: not dry** until the E2E has run green on `6a05207`; every code finding is applied
and unit-pinned, and the remaining uncertainty is the one only a cluster answers.

## 5. Carried forward

- **Per-alert profile selection** stays open, on purpose. The webhook attaches one `--profile`
  to every capture. A label-selected profile would resolve "operator or alert?" for
  collection the way the union resolved it for permissions — but a tenant could then route
  their evidence through another team's profile (its notify route, its export destinations)
  by setting a label on their own alert. Needs an allow-list design and its own round.
- The `CollectorDenied` Event's `action` is `Seal` because `publish` is shared; harmless, and
  a `publish` with an action parameter is a two-line change when the next Event needs it.
- `docs/design-record-and-seal.md` phase B remains returned-to-premise.
