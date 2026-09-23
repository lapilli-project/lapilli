# Design — retiring a capture from the controller's watch

Status: **implemented.** Round 22 ran against revision 1 of this document; its findings are folded
in and named where they changed the design, because four of them would have shipped a worse failure
than the one being fixed. What is described below is what the code does.

## The failure this exists to stop

Nothing ever deletes an `IncidentCapture`. `retention.rs` reclaims *bundles*; the CR stays, and there
is no `ownerReference`, no finalizer and no TTL, so Kubernetes' garbage collector has no handle on it
either. The controller's watch cache therefore holds every capture ever made.

Measured (`docs/design-trigger-and-load.md` §3.3): **19.4 KB of controller memory per capture held**,
and that figure is a floor — the probe objects it came from carry no status, no `exports` map and
almost no `managedFields`, all of which a real capture accumulates. Against the chart's 256 MiB limit
that is ~7,400 captures before the controller can no longer absorb the ~110 MiB a 20-alert storm
needs, and ~13,300 before it OOMs sitting idle. At ~120 captures/day — one alert over a 20-pod
Deployment at Alertmanager's 4h `repeat_interval` — that is **62 days** and 111 days. A DaemonSet
alert over 50 nodes: 25 and 44.

Not a risk. A clock, ending with the recorder dying during the incident it is recording.

## Why not a TTL

A TTL on finished captures is what most operators would expect. It is **not** shipped here, and the
first draft's reason for that was wrong — worth recording, because the wrong reason was the
comfortable one.

That draft said: "once retention has reclaimed a bundle, the CR is the only thing left saying the
capture happened." The code says otherwise, in its own voice. `crd.rs:288` on `status.local`:
"**Reporting only** … the durable record of a reclaim is the journal at the bundle root, because
this field dies with the CR and a Kubernetes Event expires within the hour." That journal is
`reclaimed.jsonl`, append-only, rotated, and exempt from reclaim — "the only thing that can still
say, a year later, that Lapilli reclaimed a bundle rather than lost it" (`retention.rs:257`). The
`.notified` claim and `.ieb.owner` are likewise in `NEVER`. And nothing in the controller reads a
finished CR back. The record does not die with the CR, and the orphan pass is *premised* on humans
deleting CRs.

The real objection to a TTL is different and narrower: **a TTL manufactures orphans.** Deleting a CR
whose bundle is still on disk makes that bundle an orphan, and `retention.reclaimOrphans: true`
would then delete live evidence. So a TTL is safe only when ordered *after* reclaim — delete the CR
only once `status.local.state` says the bundle is already gone, which is exactly the case the
journal already records. That is a good feature and it bounds etcd, which this change does not.

It is not shipped now because it would not fix the clock: retention is off by default, so a
reclaim-ordered TTL would never engage on a default install — the very installs that accumulate.
Retirement works regardless of retention. The two compose; neither replaces the other.

**Scope, stated plainly: this bounds the controller, not the cluster.** Captures still accumulate in
etcd — ~44,000 a year per rule — and each retirement adds one more `managedFields` entry and one
more revision. The memory with a measured date on it is the controller's; etcd's is a separate,
unowned problem and it is named in the open questions rather than waved at.

## What this does

**Bound what the controller holds, not what exists.** A capture keeps its CR, its status, its bundle
path and its export record; `kubectl get incidentcapture` is unchanged. It simply stops being
delivered to the reconciler once there is provably nothing left to do.

Mechanism: the controller sets `lapilli.dev/retired=true` on such a capture and watches with the
selector `!lapilli.dev/retired`. Verified against a live API server: the selector excludes labelled
objects, and removing the label returns them immediately.

### Only `Exported`. Never `Failed`.

Revision 1 retired on "phase is `Exported` or `Failed`", justified as "no collection or sealing
left". That was false in two separate ways, and both would have destroyed a documented recovery path.

- **`Failed` with a seal record is a capture waiting for a human.** Its collected evidence is staged
  on the PVC and `lapilli.dev/retry-seal=<new value>` re-drives it (`reconcile.rs:78-98`,
  `docs/kms.md:91`). An annotation edit does not bump `generation`, so no generation guard catches
  this — and a retired object delivers no event at all, so the annotation would do nothing. The
  status message the controller itself writes tells the operator to run the one command that would
  have stopped working.
- **`Failed` before sealing is repairable by a spec edit.** `reconcile.rs:99-104` reads
  `phase == Failed && (current || status.seal.is_some())`; the `current` term is deliberate, and a
  spec edit bumps `generation`, makes `current` false, and falls through to capture again. That is
  the repair path for `profile X: not found`, `export-path-not-allowed`, an ENOSPC preflight, a
  transient collector failure. Retiring it makes every future spec edit inert, silently.

So `Failed` stays in the watch. It is also the right population to keep: failed captures are an
operator's problem to act on, they are small if the install is healthy, and if they are not small
that is its own alarm (`lapilli_captures{phase="failed"}` and `LapilliCapturesFailing` already
exist).

### The predicate

Every one of these, not any:

| Condition | Why |
|---|---|
| `status.phase == Exported` | the only phase with no revival path: "once exported it is never re-captured, even if its spec is edited" (`reconcile.rs:105-107`) |
| every `status.exports` entry is `settled()` | `ExportState::Pending` means a retry is still due (`crd.rs:396`) |
| notification is **resolved**, not merely absent | see below |
| `status.observedGeneration == metadata.generation` | the spec has not changed under us |
| the label PATCH carries a `resourceVersion` precondition | see below |

### Notification: the clause revision 1 could not have satisfied

Revision 1 said "the capture's notification claim exists, or no route applies". That is unsatisfiable
as written, and the reason is worth keeping in the document because it is not obvious.

`enqueue_notification` hands the capture to an **async dispatcher** and returns
`Action::await_change()`. The `.notified` claim is written by the dispatcher up to `COALESCE_MAX`
(120 s) later, inside a spawned task. So the reconcile that did the notification work cannot see the
claim, and there are only two branches, both wrong:

- retire in the same pass → an `enqueue` that the full queue dropped is never re-driven, where today
  the next relist re-enqueues it;
- wait for the claim → the capture is never delivered again, so it never retires and the clock keeps
  running for every announced capture.

There is a way out, and it is not the dispatcher's status patch: `report()` writes
`status.notification` on the group's **leading** capture only (`notify.rs:1541`), so for a 20-pod
incident the other nineteen get no watch event at all.

What *is* reliable is the claim file. The dispatcher claims **every member of a group**, not just
the one it names in the message — `enqueue_notification` depends on that already, and says so: the
steady state is one `stat`, and it is correct only because every member is claimed. So the claim is
a true per-capture signal; it simply appears later than the reconcile that caused it.

So `enqueue_notification` returns a tri-state — `Settled`, `Enqueued`, `Deferred` — and reconcile
acts on it:

| outcome | what reconcile returns | retires? |
|---|---|---|
| `Settled` — claimed already, or a determined negative | `await_change()` | yes |
| `Enqueued` — handed to the dispatcher | `requeue(COALESCE_MAX + 15s)` | no, this time |
| `Deferred` — a profile GET or summary read failed | `requeue(60s)` | no |

The requeue on `Enqueued` is the whole trick: it comes back after the coalescing window, finds the
claim, and retires. One extra reconcile per announced capture, about two minutes later.

`Settled` therefore means: the `.notified` claim is already on disk, **or** a negative that was
actually determined — no dispatcher configured, the profile read succeeded and named no route, the
capture predates this process (which claims, so history is never replayed), an empty summary, or
`skip_remote_export` with the route not including demo captures.

`Settled` explicitly does **not** mean "we could not tell". A profile GET or a summary read can
fail for transient reasons and used to exit with a bare `skip()` and no claim, relying on the next
relist to try again. Mapping that onto "no route applies" would retire a capture whose announcement
was merely postponed, so those two paths are `Deferred`: requeue, retire nothing. Fail closed.

### The PATCH needs a precondition

The predicate is computed from the reflector's snapshot. A `retry-seal` annotation or a spec edit
landing between that read and the label write would be swallowed — the object leaves the watch
carrying work nobody saw. That is most likely at exactly the worst moment: an operator mass-setting
`retry-seal` after a KMS outage while the controller sweeps.

So the label patch carries the observed `resourceVersion`. On conflict the retirement is abandoned
and the next reconcile re-decides. A dropped retirement costs one object's worth of cache until then;
a swallowed edit costs the evidence.

## The upgrade is the dangerous part

Revision 1 said an install with 7,000 accumulated captures "pays one reconcile each — bounded by
`reconcileConcurrency` — and then holds none of them". The phrase "and then" was carrying the whole
risk. To retire via the informer the controller must first **load all 7,000 into the watch cache**,
which is 7,000 × ≥19.4 KB ≈ 136 MiB plus the floor, against a 256 MiB limit, with the ~110 MiB a
storm needs on top. **The release that exists to prevent the OOM would have been the OOM**, on the
one code path it adds, at the population size the document used as its own example.

It would also have pushed 7,000 retirement reconciles through the same two slots the live incident
path uses, so a real alert would queue behind them for the length of the sweep.

So the initial sweep does not go through the informer at all:

1. On startup, before the controller is built: paginated `list` with `limit=500`, no selector.
2. For each page: evaluate the predicate, `PATCH` the label on those that pass, **drop the page.**
3. Then build the `Controller` with `label_selector: "!lapilli.dev/retired"`.

Peak memory is one page, not the population. The controller starts with a bounded cache and an empty
queue. The sweep is paced so 7,000 PATCHes are not a write burst against etcd — a burst there lands
in `lapilli_reconcile_errors_total` looking like an RBAC problem.

The sweep is idempotent and resumable: every PATCH is durable, so a restart mid-sweep continues. It
does not block readiness forever — if it cannot finish, it logs what it got through and the
controller starts anyway, because a recorder that will not start is worse than one with a large
cache.

## What retirement actually gives up

Revision 1 claimed a destination added later would stop applying to old captures. **That was
invented.** `drive_exports` iterates the existing `status.exports` map and `seed_exports` runs only
at seal time (`reconcile.rs:107-111`, `:256-274`, `:286-289`), so a newly added destination does not
reach an already-`Exported` capture today either. Nothing is given up there.

What is actually given up is narrower and real: **an operator can no longer re-drive a wrongly
settled export by editing `status`.** A `Refused{not-allowed}` caused by a destinations file that
failed to parse at startup, or a `Conflict`, is today repairable by clearing that entry and letting
the next reconcile retry. After retirement a status patch on the object is inert.

The recovery has an order, and it must be documented in that order:

```
# 1. un-retire FIRST — a status edit on a retired capture does nothing
kubectl -n lapilli-system label incidentcapture <name> lapilli.dev/retired-
# 2. then clear the settled export entry
```

Un-retiring is immediate, not "on the next resync": a label change that makes an object match the
selector is delivered as an ADDED event. Which is also why `--all` on 7,000 objects refills the cache
in real time and is not a safe recovery at that size — scope it, with `-l lapilli.dev/retired=true`
and a name or age filter.

## Observability, because otherwise the fix is unverifiable

Revision 1 added no metric, so an operator could not tell retirement was working, and the existing
`LapilliCapturesAccumulating` rule — `sum(lapilli_captures) > 5000` — would have fired forever on a
7,000-capture install with a summary that had become false ("nothing deletes them and the
controller's memory grows with the count"). A permanently firing warning with wrong text is how an
alert gets silenced.

- `lapilli_captures{phase, retired="true"|"false"}` — the state poller already does an unselected
  `api.list()`, so both the true population and the watched population come from the one call it
  already makes.
- `lapilli_captures_retired_total` — a counter, so a sweep that stalls is visible.
- `lapilli_captures_unretirable{reason}` — the residual. Two populations can never retire and they
  are exactly the ones this change exists to bound: an export stuck `Pending` because its
  destination vanished from config, and a notification that never resolves. Counting them is the
  difference between "the bound binds" and "we assume it does".
- `LapilliCapturesAccumulating` is rewritten against `sum(lapilli_captures{retired="false"})`, which
  is the series it should always have used.

## What is not affected

Checked, because a selector on a watch is exactly the change that quietly breaks a consumer that
looked like it read the same data:

| Consumer | Reads | Affected |
|---|---|---|
| `retention.rs` orphan pass (`list_captures`) | `api.list()`, no selector (`retention.rs:550-562`) | **No.** This matters most: if it read the cache, every retired capture would look like an orphan and `reclaimOrphans: true` would delete live bundles. |
| `lapilli_captures{phase}` gauges (state poller) | `api.list()`, no selector | No — and it is what the accumulation alert needs. |
| `incident-id-in-use` claim | `O_EXCL` owner file on the PVC | No. |
| webhook dedup | 409 on a deterministic name, not a patch | No — a resend onto a retired capture loses nothing. |
| `kubectl get incidentcapture` | the API | No. |

The rule this establishes: **the watch is scoped; nothing that needs a true population may read the
watch.**

## Migration notes this change owes the CHANGELOG

- Retirement exists, is label-driven, and `lapilli.dev/retired` is now the control surface that
  decides whether the controller looks at an object. `COMPATIBILITY.md` §3 should say so.
- `kubectl edit` on a **retired** capture is a no-op until the label is removed. So is a status patch.
- `LapilliCapturesAccumulating` changes expression and text; the old one fires forever after this.
- A downgrade is safe: the old controller has no selector and reconciles labelled captures exactly as
  before. The label is inert to it.
- Adding an export destination backfills nothing — true before this change too, and worth stating
  because operators assume otherwise.

## Open questions for review

1. Is the predicate complete *now*? Revision 1's was wrong in three places and every one was found
   by reading `reconcile.rs` rather than by reasoning about phases. That is evidence about the method,
   not just about the predicate.
2. Should the startup sweep have its own concurrency, separate from `reconcileConcurrency`? It is
   pure PATCH traffic and the reconcile bound was chosen for capture work.
3. A capture whose export is `Pending` forever because its destination vanished: revision 1 left this
   open; the code says a destinations file that no longer names it makes it `Refused{not-allowed}`,
   which is `settled()`, so it **does** retire. Confirm that on a live cluster rather than from
   reading, because it is the difference between a bound that binds and one that does not.
4. **What bounds etcd?** Retirement bounds the controller and leaves ~44,000 objects a year per
   rule in the cluster's datastore. The reclaim-ordered TTL above is the candidate, and it only
   engages where retention is on. Unowned.
5. **Is 19.4 KB the right figure?** It comes from five points, n=1 each, whose per-500 increments
   were +10.0, +4.6, +18.7 and +4.5 MiB — a 4.2x spread with the shape of allocator steps rather
   than a per-object cost. The delete experiment in the same section frees 8.8 KiB per capture, not
   19.4. The direction is not in doubt and the ceiling is; re-measure with `REPEATS=3` and real
   captures before quoting a date to anyone.
