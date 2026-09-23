# Design review — round 22: retiring a capture from the controller's watch

Constitution: Loop Engineering v0.5.0. Tier: **full** (a change to what the controller watches,
on a failure with a measured date on it).

Snapshot attacked: `docs/design-capture-retirement.md` revision 1, on top of `fdb5241`, together
with `reconcile.rs`, `notify.rs`, `telemetry.rs`, `retention.rs`, `perms.rs` and the chart.

Lenses, four, in parallel: **predicate correctness** (does it retire a capture that still has
work?) · **hidden watch-cache consumers** · **operator on upgrade day, 7,000 accumulated captures**
· **devil's advocate against the choice itself**. Each was told a clean report was legal.

Independence: same model family throughout. Calibration, not independence.

## Result

**23 findings. Four of them would have shipped a failure worse than the one being fixed**, and one
was a pre-existing defect with no relation to this change that two critics found on the way past.

Round verdict: **not dry, then implemented and verified.** The design was rewritten as revision 2
before a line of it was built.

### The four that changed the design

**1 — Retiring `Failed` would have killed a documented recovery. (BLOCKER, three critics)**

Revision 1 retired on "phase is `Exported` or `Failed`", justified as "no collection or sealing
left". False twice over:

- `Failed` with a seal record is a capture *waiting for a human*: `lapilli.dev/retry-seal` re-drives
  it (`docs/kms.md:91`), an annotation edit does not bump `generation`, and a retired object
  delivers no event at all. The status message the controller writes would have told the operator
  to run the one command that no longer worked.
- `Failed` before sealing is repairable by a spec edit — `(current || status.seal.is_some())` has a
  deliberate `current` term — which is the repair path for `profile not found`,
  `export-path-not-allowed`, an ENOSPC preflight. Retirement makes every future edit inert.

One critic went further and checked the test: `test/e2e/kms.sh` asserts only "still Failed, data
kept, no bundle" — **all three of which stay true when nothing runs at all**, so the existing suite
could not have caught this. `Failed` now never retires.

**2 — The notification clause was unsatisfiable. (BLOCKER)**

Revision 1 gated on "the claim exists, or no route applies". `enqueue_notification` hands the
capture to an async dispatcher and returns `await_change()`; the `.notified` claim appears up to
120 s later. So: retire in the same pass and a dropped enqueue is never re-driven; wait for the
claim and the capture is never delivered again, so it never retires.

Revision 2's first repair was also wrong — "the dispatcher's status patch will re-trigger reconcile"
— because `report()` writes `status.notification` on the group's **leading** capture only. For a
20-pod incident that retires 1 in 20 of exactly the population this exists to bound.

What is reliable is the claim file: the dispatcher claims **every member**, which
`enqueue_notification` already depends on and says so. So `enqueue_notification` now returns
`Settled | Enqueued | Deferred`, and an `Enqueued` capture requeues past the coalescing window to
find its own claim.

**3 — Two of its exits are transient failures. (MAJOR)**

A profile GET or a summary read can fail for reasons that do not repeat, and both exited with a bare
`skip()` and no claim, relying on the next relist. Treating "no claim" as "nothing to announce"
would have retired a capture whose announcement was merely postponed. Those are `Deferred`:
requeue, retire nothing, fail closed.

**4 — The release that prevents the OOM would have been the OOM. (BLOCKER)**

Revision 1 retired through the informer and said an install with 7,000 captures "pays one reconcile
each — bounded by `reconcileConcurrency` — and then holds none of them". The phrase "and then" was
carrying the whole risk: to retire via the informer the controller must first load all 7,000 into
the cache. 7,000 × ≥19.4 KB ≈ 136 MiB against a 256 MiB limit, with the ~110 MiB a storm needs on
top — on the one code path the change added, at the population size the document had chosen as its
own example. It would also have queued 7,000 reconciles ahead of the live incident path.

The startup sweep now pages the API directly, before the watch exists, and drops each page. Peak
cost is one page.

### The finding that was not about this change at all

**The state poller listed every capture, unpaged, every 30 seconds.** Two critics found it
independently, and the repository had already written the rule it broke — `retention.rs:547`: *"Never
`ListParams::default()`: a second unpaginated copy of a large population in a pod limited to 256 MiB
is an OOM risk, and an OOMKill discards the capture in flight."*

This mattered to the design as well as on its own: bounding the watch cache while a >100 MiB
transient ran twice a minute would have turned a monotone climb into a sawtooth with the same peak.
Shipped separately, first, as `6440ec4`.

### The rejection that was a rationalisation

Revision 1 rejected a TTL because "once retention has reclaimed a bundle, the CR is the only thing
left saying the capture happened". The devil's advocate showed the code says otherwise, in its own
voice — `crd.rs:288`: *"the durable record of a reclaim is the journal at the bundle root, because
this field dies with the CR"*, and `retention.rs:257` calls `reclaimed.jsonl` *"the only thing that
can still say, a year later, that Lapilli reclaimed a bundle rather than lost it."* The orphan pass
is **premised** on humans deleting CRs.

The real objection is narrower: **a TTL manufactures orphans**, so TTL plus `reclaimOrphans: true`
deletes live bundles. That makes the safe TTL "delete only once the bundle is already reclaimed" —
a good feature, which bounds etcd, and which retirement does not replace. It is not shipped now
because retention is off by default, so it would never engage on the installs that accumulate. The
document now says all of that instead of the comfortable version.

Also conceded: **this bounds the controller, not the cluster.** etcd still grows ~44,000 objects a
year per rule. Named as an open question with no owner rather than waved at.

### Also applied, from the smaller findings

`resourceVersion` precondition on the label PATCH, so a `retry-seal` landing between the predicate
and the write is not swallowed (409 → abandon, decide again). `Patch::Merge`, never `Apply`, or SSA
prunes the status fields the shared field manager owns. `patch` added to the permission preflight,
whose own rule is that every verb the code issues is asked about, and which did not ask about this
one — the chart grants it, so this is about a hand-written Role passing preflight and then silently
never retiring. `ObjectNotFound` no longer counts into `lapilli_reconcile_errors_total`, whose
documented meaning is "a dead watch, no capture will ever be noticed again"; retirement makes that
error ordinary, and an alert that fires in normal operation is one that gets silenced.

Observability, because otherwise the fix is unverifiable in someone else's cluster:
`lapilli_captures_watched` (the number the memory tracks), `lapilli_captures_retired`,
`lapilli_captures_exported_unretired` (retirement binds only while this is near zero) and
`lapilli_captures_retired_total`. The accumulation alert is rewritten against `captures_watched`;
the old one counted every capture that exists, which grows forever by design, so on a healthy old
install it fired and stayed fired with text that had become false.

## Then the gate found one more, and it was the oldest bug in this file

The release gate failed the notify suite, and the controller log read as a contradiction: one
process created a capture at `02:26:58.899` and 89 ms later called that capture *"predates this
controller process"*.

Both statements were true. **Kubernetes stores `creationTimestamp` at one-second resolution.** The
replay guard compared it against a sub-second `Utc::now()`, so a capture created 0.7 s *after* the
process started carried `02:26:58.000` against a start of `02:26:58.157` — newer than the process,
filed as older, claimed so no relist would reconsider, never announced.

That is a one-second window after every start in which the first capture is silently never
announced, reachable whenever a controller rolls while an incident is firing.

**It is the notify E2E flake this project failed to explain twice.** Earlier in the same body of
work the identical assertion failed with the identical shape; EPIPE plus `pipefail` was
hypothesised, measured, disproved, and the cause recorded as unknown with the assertion made
self-diagnosing instead of fixed. It was this. Nothing in this round was looking for it — retirement
added one log line at the end of a capture's life, and the contradiction became visible.

## Verified

Unit: eight tests on the predicate, mutation-verified three ways (retiring `Failed` fails, dropping
the generation guard fails, treating `Pending` exports as settled fails) plus the truncation test,
mutation-verified by restoring the raw comparison. The poller's paging has its own two-page fake API
server, mutation-verified by discarding the continue token.

Live, on kind: a real capture retires and logs how to undo it; a `Failed` capture stays watched with
no label; the gauges read 2 exist / 1 watched / 1 retired / 0 exported-unretired; removing the label
returns the capture within a second and it retires again; and — with the controller scaled to zero so
the old pod could not do it first — the startup sweep reports `seen=2 retired=1` with a timestamp
ordering that proves it ran *before* the watch was built.

The first attempt at that last check reported `seen=2 retired=0` and would have read as a pass. The
old pod had retired the capture before terminating, so the sweep's retire path was never exercised.

Release gate: **`release-check OK`** on Kubernetes 1.30 and 1.37, notify suite included, with
`lapilli_captures_retired_total 20` on the kept cluster — every capture of the storm suite retired
through the reconcile path while every assertion held.

## What this round says about the method

**A design reviewed before it is built is cheaper by the width of the thing it was going to break.**
Four findings here were not improvements to a working change; each was a shipped regression avoided.
Two of them (retiring `Failed`, the leader-only status patch) were invisible from the design document
and only reachable by reading `reconcile.rs` and `notify.rs`. The critics that found them were the
ones told to read code, not prose.

**The second lesson is the same one round 21 ended on, and it repeated.** The most valuable finding
of the round — the unpaged poller — was a defect in an instrument's *cost*, not in the system's
logic, and it would have silently cancelled the benefit of everything else. And the measurement of
the sweep passed before it measured anything.
