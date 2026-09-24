# Design — permissions follow the profiles

Status: **decided, implementing.** Resolves round 24's Fix I/II findings
(`docs/design-review-round24.md` §2) and one ambiguity the round left open.

## What was wrong

`perms.rs` asks the API server whether the controller holds the permissions it needs, every
600 s, so a broken RBAC binding is found at install time rather than during an incident. Two
things made the answer unreliable:

1. **It read one profile, once.** `needs_from_cluster` was awaited a single time at startup
   (`main.rs:397-404`) and `spawn` reused that `Needs` forever. The module's stated purpose is
   to catch RBAC *drift*; deriving from a profile frozen at startup gave it the mirror-image
   blind spot: add `logs` to a profile at 10:00 and no `pod-logs` check exists until the pod
   restarts.
2. **It read *the* profile, singular.** Profiles are selected **per capture**
   (`IncidentCapture.spec.profile`, `crd.rs:196-202`), the Role grants `update`/`patch` on
   them (`rbac.yaml:75-77`), and the chart may not have created them at all
   (`captureprofile.yaml:1` is behind `profile.create`). There is no single profile to derive
   from.

A third defect sat underneath both: every collector check was **unconditional**. An operator
who correctly dropped `pods/log` on an install whose profiles never ask for logs got a
permanent false alarm every 600 s — the exact noise `perms.rs:77-80` says the module exists to
avoid — and round 24 showed that with derived RBAC the same asymmetry produces a bundle that
verifies OK at coverage 100% with zero log bytes (fixed separately, `a1e9485`).

## The rules

**1. Needs are the union over every profile the controller can list.** Each pass lists
`CaptureProfile` in the controller's namespace and unions `spec.collectors`,
`diffs.configMaps` and static signing Secrets across them. This dissolves "does the profile
belong to the operator or to the alert?" *for permissions*: whatever any profile could ask a
capture to do must be held. A cluster with a perishable profile and a full profile needs the
full set, and that is the truth, not a false alarm.

**2. Needs are re-derived inside the loop.** `spawn` no longer takes a `Needs`; it takes what
it takes to compute one, and computes it on every pass. Startup is just the first pass.

**3. Collector checks are conditional on the union, and a check that was not needed is
recorded, not omitted.** `Report` gains a fourth state, `NotNeeded`, beside held / denied /
unknown. Round 24's objection was that "deferred" had no representation and read the same as
"nothing was asked". Now a pass over a perishable-only cluster reports
`pod-logs: not needed (no profile intends logs)`, and a new gauge,
`lapilli_permissions_asked`, makes a shrinking check set visible without disclosing which
check shrank (the per-check-inventory rule in `docs/metrics.md` stands).

The collector → permission mapping, from the call sites the checks already cite:

| check | needed by | a denial **stops** |
|---|---|---|
| `pods` (get, list) | `logs`, `resources`, `changes` | `logs`, `resources`, `changes` (a hard `?` in each) |
| `pod-logs` | `logs` | `logs` (since `a1e9485`) |
| `events` (list) | `events`, `changes` — `diffs.rs` lists events to date a reused ReplicaSet, and that read is a hard `?` | `events`, `changes` |
| `replicasets`, `deployments` | `resources`, `changes` | `changes` only — `resources` reads the owner chain with `if let Ok`, so a denial leaves the chain out of the bundle without failing the collector |
| `statefulsets`, `daemonsets` | `resources`, `changes` | `changes` only, for the same reason |
| `controllerrevisions` | `changes` | `changes` |
| `configmaps` | any profile with `diffs.configMaps` | — |

Two columns, because two questions are asked of the table. *Needed by* decides whether a
check is **asked** (a denial there degrades the bundle, whether or not the collector fails).
*Stops* decides what a `CollectorDenied` Event may **name as the cause** of a collector that did
not run — only a read whose denial actually fails that collector, or the Event would assert a
cause that could not have produced the symptom. A critic found the first version of this table
wrong in both directions: it had `events` as unneeded by `changes` (so the perishable profile's
own advice to drop `events` would have broken `changes` with the self-check green) and `pods` as
needed by `events` (which reads only the events API).

`metrics` needs nothing in-cluster: it reaches Prometheus by URL. Checks that are not about
collectors (`captures`, `capture-status`, `profile`, `recorded-events`, secrets) are
unconditional as before.

**4. An unreadable profile list is `unknown`, not silence.** If `list captureprofiles` fails,
every collector check is recorded `Unknown` and the pass logs at `warn` — the controller
cannot say what it needs, which is different from needing nothing. Before, an unreadable
profile was a `debug!` and then *"every permission this install needs is held"*
(`perms.rs:473-477`, `:395-398`). An **empty** list is a real state and is logged as such:
no profile, no capture will ever be configured.

**5. A capture that a denied permission thinned says so at the capture.** This is the
ambiguity round 24 left open, phrased there as *"refused with an Event, not collected
thinly"*. Refusal is the wrong call for an evidence tool: a bundle with `resources` and
`changes` and a PARTIAL verdict is strictly more evidence than no bundle, and a capture
refused on a stale self-check (RBAC fixed five minutes ago) would be evidence lost to a
guess. So:

- collection runs as it always has; a denied read fails its collector and the bundle is
  PARTIAL (that is the `a1e9485` rule);
- **after** collection, for each intended collector that did not run, the reconciler consults
  the latest permission report, and if a check that collector depends on is `Denied`, it
  publishes a Warning Event on the `IncidentCapture` —
  `CollectorDenied: logs did not run; the permission self-check at <time> found pods/log get
  denied in <namespace>` — so `kubectl describe` on the capture names the cause, not just
  the symptom.

Evidence first; never silent. The report is shared with the reconciler through the context;
if no pass has completed yet, nothing is attributed and the existing warning log stands.

**6. The CRD default agrees with the chart.** `default_collectors()` returned `["logs"]`
(`crd.rs:418-420`) while the chart's default is `[logs, resources, events, changes]`. Two
defaults for one thing that disagree is a latent bug; a hand-written profile that omitted
`collectors` silently got the thinnest possible capture — the one collector the perishable
profile drops. The CRD default now matches the chart. `CaptureProfile` is `v1alpha1`; this
changes behaviour for an omitted field, in the direction of collecting more, and is noted in
the CHANGELOG.

## What this does not do

- It does not make RBAC derive from the profile in the **chart**. Round 24 killed that: the
  chart cannot see per-capture, runtime-patched, possibly un-chart-created profiles. The
  chart keeps granting the full collector set; tightening is the operator's act, and this
  module now tells them correctly whether the tightening broke anything.
- It does not add a per-check gauge. The capability-inventory argument in `docs/metrics.md`
  is unchanged.
- It does not reconcile profiles across namespaces. Profiles live in the controller's
  namespace (`Api::namespaced(.., &ns)` in `reconcile.rs:839`); that is the population.
