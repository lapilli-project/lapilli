# Design — bundle lifecycle: bounded local retention

Status: **implemented and shipped, off by default.** Designed here, returned by
`docs/design-review-round17.md` with four blockers — the first draft's shape survived; its primary
axis, its safety predicate, its records and all three of its gauges did not — then revised and
built. Implementation found three more that no lens could have: Helm renders a YAML integer as a
float, the byte ceiling compared two different quantities, and the journal ordering was backwards.
Roadmap: `DESIGN.md` §11, v0.2, delivered.

## The defect

**Nothing deleted a sealed bundle before this.** The problem statement is kept in the present
tense below because it is the argument the design answers, and the constraints it derives still
bind anyone changing the sweep — but the behaviour described here is what the repository looked
like *before* `crates/lapilli-controller/src/retention.rs` landed.

Each capture writes `<incident>.ieb` and
`<incident>.summary.json` under the bundle root, plus two claim files and, while it runs, an
uncompressed staging directory. Nothing ever removes any of it. The chart's default PVC is **1 GiB**
(`persistence.size` in `charts/lapilli/values.yaml`).

The consequence is worse than losing old bundles. **When the PVC is full, new captures fail** — the
recorder stops recording, and what it stops recording is the incident happening now. Today that
failure is also ugly: ENOSPC surfaces from the `O_EXCL` owner-file create as
`status.message = "No space left on device (os error 28)"`, which honours none of the reason-code
convention `reconcile.rs` sets for capture errors ("capture errors start with their reason code",
in `reconcile()`'s error handling).

It is also an unbounded liability. `DESIGN.md` §5: redaction is **best-effort**, so a bundle may hold
personal data the redactor missed. Keeping everything forever is not the conservative choice.

## Bytes first, age second

The first draft proposed `retention.days` alone. **On this chart's defaults, an age window cannot be
relied on to engage before the disk fills.** Capture identity is per `(rule, cluster, namespace/pod, minute)`
(`create_capture` / `deterministic_name` in `webhook.rs`), so one alert over a 20-pod Deployment at Alertmanager's default `repeat_interval`
of **4h** is ~120 captures/day.

The rest is sensitive to bundle size, and that is where this paragraph has been weakest. The only
population anyone has measured is **n=201 demo bundles of one thin workload — p50 6.2 KB, max 6.7 KB** —
captures of crash-looping busybox pods whose entire log is one line. That is a median of *demo*
bundles, which makes it a floor for real workloads, not a median of them — as
`charts/lapilli/values.yaml` says at `persistence.size`: *"Only the last of those is measured, and
it is a floor — a real capture carries a real log window."* The producer cap is 1 GiB
(`PRODUCER_MAX_BYTES`, `hashtree.rs`), so one capture may legally be the whole volume.

So the honest arithmetic is a range across three orders of magnitude, and the conclusion has to hold
across all of it rather than at a chosen point:

| bundle size | per day (120 captures) | 1 GiB PVC full on |
|---|---|---|
| 6.2 KB (measured, thin workload) | 0.74 MB | day ~1,400 |
| 250 KB | 30 MB | day 34 |
| 1 MiB | 126 MB | day 8 |

At the measured floor an age window wins easily; at a megabyte the disk wins long before a 30-day
window engages. **Which bound fires first is therefore a property of the workload, not of the
design** — and that is precisely why bytes must be the primary bound: it is the only one that holds
across the range. `days` remains as a secondary trim for the liability argument.

A DaemonSet alert across 50 nodes is ~300 captures/day, which moves every row up by 2.5×.

Two corrections are folded into the numbers above, both of which made earlier versions of this
paragraph wrong in the same direction — too confident:

- it said "hourly repeat" and concluded day 9 flatly. Alertmanager's default `repeat_interval` is
  **4h**, so the capture rate was 4× too high.
- it then asserted a 250 KB–1 MiB band as though it were measured. It was not: it came from a
  bundle landing in the `le=1048576` histogram bucket, which only bounds the size from above. The
  one real measurement is 40× below the bottom of that band.

The conclusion — bytes primary, age secondary — survives both corrections. Its *margin* did not, and
saying so is the point of rewriting this rather than patching the number.

So:

| Value | Default | What it does |
|---|---|---|
| `retention.maxBytes` | `0` = off; `""` derives `persistence.size × 0.8` | The primary bound, on **the bytes Lapilli's own files occupy** — not on the filesystem's used bytes. A PVC is usually backed by a filesystem far larger than the request (hostPath, local-path, kind), so `statvfs` used-bytes and `persistence.size` are different quantities; comparing them tripped the ceiling immediately on a kind cluster. Reclaim oldest-first until under it. |
| `retention.days` | `0` = off | A secondary trim, for the liability argument rather than the capacity one. |
| `retention.minFreeBytes` | 64 MiB | Preflight: a capture that cannot possibly be sealed fails **before** collecting. |
| `retention.reclaimOrphans` | `false` | See "Orphans", below. This one is dangerous and defaults off. |

`statvfs` keeps the two jobs it is right for — the free-space gauge and the preflight, which really are about the filesystem rather than about us.

`minFreeBytes` is the part that actually protects the primary path: one `statvfs` of the bundle root
in `run_capture` before staging, and if it is short the capture fails immediately with a `pvc-full:`
reason code instead of half-collecting and dying on a raw ENOSPC.

## What is reclaimable, by name

The first draft said "the `.ieb` and its sidecars", and that phrase is what produced its worst bug.
There are no sidecars. There is an explicit list.

**Reclaimed:**

- `<incident>.ieb` — the bundle.
- `<incident>.summary.json` — the notification's source, needed only until the notification settles.
- `.staging-<incident>-<uid>/` and `.<incident>-<uid>.ieb.tmp` **whose capture is no longer live** —
  abandoned work. These are *uncompressed* and on the same volume, so on the broken install this
  feature exists for they are the largest reclaimable thing there is. Keyed on the capture UID in the
  name, never on age alone: a capture may legitimately sit in `Sealing` for days through a KMS outage,
  which `seal_with_kms`'s retry budget (`MAX_ATTEMPTS` × `backoff`) already anticipates.
- `<leader>.unsent` (and the `<leader>.unsent.tmp` a killed write leaves behind) **once
  `notify::COOLDOWN` has passed since it was last written** — a notification hand-off no dispatcher
  can still send. **Added in round 30; this was the fourth "never touched" entry and should not have
  been.** It is the only file under the bundle root that carries content taken from the workload:
  `notify.rs`'s `hand_off` serializes a whole `Group` — namespace, owner, every member's pod name,
  the cluster, and every member's `Summary` including the change diff's before/after values — so
  that the next process can post a message the shutdown flush's POST could not
  (`hand_off`/`take_handoffs`). The requirement is *survive a restart*; the `NEVER` list implements
  *survive forever*. Scale the controller to zero, or uninstall, and that file stayed on the volume
  with nothing left that would ever read it.

  The condition is **age past the notification cooldown** (`retention::HANDOFF_QUIET`), and that is
  one number carrying two independent arguments:

  - *It can no longer be delivered.* A running dispatcher takes every hand-off at start, before any
    new work, and again every `notify::HANDOFF_RESCAN` (30 s) — that rescan exists because the
    predecessor may still be alive and write one *after* the successor first looked. The cooldown is
    sixty of those passes. A hand-off that survives all of them is in an install with no dispatcher
    reading it (notify removed from the values, or the controller scaled to zero), or is one
    `take_handoffs` has already refused to consume, which it logs.
  - *It would no longer be news if it could be.* `COOLDOWN` is the window in which notify itself
    treats a verdict as already announced, and the dispatcher drops its own cooldown entries at
    exactly this age — "a lost count is a lost count, not lost evidence". Posting a half-hour-old
    "this is happening now" into a channel is not what the hand-off exists for. That *is* the
    `.notified` case, which is why that suffix stays permanent.

  Inside the window it is as untouchable as a claim: refused under `handoff-sendable`, **regardless
  of the byte ceiling**, because a full volume is not a reason to destroy a message that is still
  going to be sent. Deliberately **not** keyed on "the capture retired": retirement only requires the
  `.notified` claim to be on disk (`docs/design-capture-retirement.md`) and the dispatcher claims a
  group *before* it posts it, so a retired capture can still have a hand-off waiting — the one file
  this must not take. Nothing about the *evidence* rides on it: the bundle, the summary, the
  `.notified` claim and `status.notification` all outlive it. The journal records the file name,
  which carries the leader's incident id; the candidate is not incident-keyed, so it cannot dilute
  the orphan-storm fraction.

**Never touched, permanently:**

- `<incident>.notified` — **not a sidecar; a claim.** `design-notify.md` states "the claim is never
  released… making it releasable would make replay possible", and `notify.rs:9` explains why it is a
  file and not a status field. Deleting it re-arms notification: the claim check at the top of
  `enqueue_notification` stops returning early, the `created < started_at` guard does not catch a
  capture newer than the process, the
  30-minute cooldown has long expired, and **a month-old incident is announced to Slack as news.** It
  is also non-convergent — a restart re-creates the file (`notify::claim`, called from
  `enqueue_notification`'s history guard) and the next sweep
  deletes it again, forever.
- `<incident>.ieb.owner` — the `O_EXCL` incident-id claim behind the promise "its bundle is never
  overwritten" (`run_capture` in `reconcile.rs`). Release it and a resent webhook can create a **new** bundle
  carrying the old incident's identity, so `lapilli verify --incident X` passes on bytes collected
  months later. That is the replay/substitution defence `DESIGN.md` §7 claims.
- `keys/<key_id>.pub` — the archived signing keys. `archive_public_key` only ever writes the *current*
  signer's key (`reconcile.rs`) and `copy_archived_keys` only copies to destinations that reached
  `Uploaded`, so on a local-only install a rotated key exists **nowhere else**. Deleting it makes
  every bundle it signed unverifiable, including bundles safely in an Object Lock bucket.

The two claim files are four small inodes per capture and are not the capacity problem. If their
growth ever needs addressing it needs its own design, with a tombstone that preserves the claim's
*meaning* rather than an unlink.

Each entry above earns its place with a **stated consequence** of reclaiming it — notification is
re-armed, an incident id is freed for a second bundle, bundles become unverifiable. That is the bar,
and "it is not a bundle" does not meet it. `.unsent` sat in this list with a description and no such
consequence, which is how the one file holding workload content became the one file that lived
forever.

## When it refuses

**A bundle is reclaimable only when a remote copy demonstrably exists**, or when the operator has
explicitly opted into local-only reclamation.

The first draft used `ExportState::settled()`. That is wrong in the worst direction: `settled()` is
true for `Refused`, `Conflict` and `Failed` (`ExportState::settled()` in `crd.rs`), and the
`lapilli_export_destinations` row of `docs/metrics.md` defines exactly
those as "**that evidence never reached the destination and never will**". So the draft's headline
refusal permitted deleting the only copy precisely when there is no second copy — and it makes the
documented `LapilliExportsLost` alert unactionable, because its remediation is "go get the local copy".
`Refused` arrives from ordinary misconfiguration: a mistyped destination name, a `too-large` bundle,
or an S3-compatible backend that ignores conditional writes and disables the destination outright.

So: reclaim requires **every referenced destination observed as `Uploaded`**. `Refused`, `Conflict`
and `Failed` are refusals, counted, so "retention cannot keep up" is visible rather than silent.
`Conflict` especially: it means a different object already holds this incident's key, so the local
file is the only thing that can show the remote is not the evidence.

**And the decision is never read from `status`.** This codebase refuses that everywhere —
`enqueue_notification` in `reconcile.rs` recomputes where the bundle is "never from `status`, which anyone with patch access could point
elsewhere". The controller's own Role grants `patch` on `incidentcaptures/status`, so a compromised
collector flipping one `Pending` to `Uploaded` would get a **targeted** delete out of a feature whose
stated mitigation is that it cannot target. Export state is re-derived by the controller, and age
comes from the file's own mtime.

**An install with no destinations** — the default — has no remote copy at all, so reclaim there
destroys the only copy. That is a real choice and it gets its own switch,
`retention.allowUnexported`, default off, said plainly in the values comment, the schema description
and `NOTES.txt`. Without it, a PVC-only install reclaims only abandoned staging and `.tmp` files, which
is still the largest win available.

**If the unlink fails** — `EPERM`/`EROFS`, which is what a WORM-backed PVC does — the sweep refuses,
counts it under its own reason, and **never records the bundle as reclaimed**. `DESIGN.md` §11 already
says a tool that reports success while the object remains is worse than one that refuses. This matters
because the `persistence` comment in `values.yaml` tells audit installs to "back this with WORM/object-lock storage": for them
the PVC *is* the locked store, so §11's "the store gets a vote" is not declined by staying local — it
is relocated, and it still has to be honoured. Those installs also inherit the erasure consequence
below ("What this is called"): on a WORM-backed PVC retention cannot delete a bundle no matter what
the policy says, and neither can anyone else until the lock expires.

## Orphans, and why they default off

A file whose `IncidentCapture` no longer exists has no age to check against and nothing to record in.
The first draft reclaimed them unconditionally. That is unsafe for a reason that has nothing to do with
evidence value: **nothing in the controller ever deletes an `IncidentCapture`** — `grep` for `.delete(`
finds nothing — so "no live CR" is a statement about human behaviour. One `kubectl delete
incidentcapture --all`, or the documented CRD upgrade path (Helm does not upgrade `crds/`, so it is a
delete-and-recreate that cascades every CR away), turns the next sweep into *delete every bundle*.

So the orphan pass:

- is behind `retention.reclaimOrphans`, **default false**;
- runs only on a **complete, paged, successful** list — never on an error, never on a partial page, and
  never on the assumption that an empty list means an empty cluster;
- **refuses and alerts** rather than deleting when orphans exceed a small share of the population
  (>100 files or >5%), which is the signature of a CR wipe rather than of a human tidying one capture;
- selects by **allowlist**: only `<incident>.ieb` / `<incident>.summary.json` whose `<incident>` parses
  as a valid incident id. `keys/` is excluded by name.

Abandoned staging directories and `.tmp` files are *not* orphans in this sense — they are keyed on a
capture UID and reclaimed regardless of this switch.

## The record

The first draft claimed to satisfy §11's "deleting must be at least as recorded as capturing" with a
status patch, a Kubernetes Event and a counter. **None of the three is durable.** Events expire (1 h
default TTL). `status.local` dies with the CR — and the orphan case is *premised* on a human deleting
the CR. Counters reset on restart, which `COMPATIBILITY.md` §2 (Metrics) states outright.

So there is one durable record and it is authoritative: an append-only **`reclaimed.jsonl`** at the
bundle root, excluded from every pass, size-capped and rotated. One line per reclaim: incident id,
capture UID, the bundle's `sha256` from its manifest, byte count, the export states the decision was
made on, the reason, and the RFC 3339 instant.

The line is written **after** the file is gone, not before. Recording first looks safer and is worse:
a removal that fails then leaves a journal line claiming a reclaim that did not happen, which is the
"reports success while the object remains" failure §11 names — observed exactly that way on a live
cluster, where a permission error produced both a warning saying the file was *not* reclaimed and a
journal line saying it was. The residual is the reverse and smaller: a journal write that fails after
a successful removal leaves the bytes gone and unrecorded, so that case is logged at error level with
the same JSON.

`status.local` and the Event become convenience views of that file — exactly the relationship
`design-notify.md` already sets up between `status.notification` and the `.notified` claim. The
journal is what lets a missing bundle years later be distinguished from a lost one.

This also answers the first draft's open question 3 the other way round. The question was not "is the
Event plus the metric enough"; neither is a record.

## Bounds

Round 14 and round 15 each found a loop in this crate with no time budget. This is the place not to
make it three.

- `SWEEP_BUDGET` around the whole pass, as round 15 gave `perms.rs` its `PASS_BUDGET`.
- `MAX_RECLAIMS_PER_SWEEP` (a few hundred), with the remaining backlog exposed as a gauge so "catching
  up" is visible rather than silent.
- The filesystem pass in `spawn_blocking`, so a slow RWO volume cannot hold a tokio worker while the
  webhook waits.
- **No unpaginated list.** The capture population is read from the controller's existing reflector
  store, or with `ListParams::default().limit(500)`. A second unpaginated copy of 10,000 objects in a
  pod whose memory limit is 256 MiB (`resources.limits.memory` in `values.yaml`) is an OOM risk, and an OOMKill discards the
  capture in flight.

The cost, computed rather than guessed, the way round 15 required: enabling retention on a year-old
install is ~50,000 unlinks **plus one PATCH and one Event per capture** if each is recorded
individually — 100,000 API writes in one pass, an Event flood and client-side throttling. The
per-sweep cap plus the journal is what keeps it to a few hundred writes per pass. The directory scan
itself is *not* the cost: a 40,000-entry flat directory scans in 0.179 s warm on local SSD, measured
during this round.

## What is measured

All three gauges from the first draft were computed by the sweep, which made them unimplementable
("still emitted when retention is off"), freezable, and aimed at the wrong quantity.

- `lapilli_bundle_fs_bytes{state="used"|"free"}` — one `statvfs` of the bundle root on the **always-on**
  poller, not the sweep. O(1), correct when the volume is full, and it counts staging directories and
  leftovers that a per-`.ieb` sum cannot. Every cluster already scrapes
  `kubelet_volume_stats_available_bytes`; this exists because it is scoped to the bundle root and
  needs no kubelet-metrics access.
- `lapilli_retention_sweeps_total{result}` (the first draft named it as a "local sweep runs"
  counter; the shipped name is in `telemetry.rs`) — emitted **from process start**, so `absent()` works on it
  and an alert can guard against a stale pass with `unless on(instance)`, which is round 14's S1 and S3
  applied rather than rediscovered.
- `lapilli_bundles_reclaimed_total{reason}` and `lapilli_reclaimed_bytes_total` — renamed from the first
  draft's `lapilli_bundle_bytes_reclaimed_total`, which collided with the existing `lapilli_bundle_bytes`
  histogram of bundle sizes.

Any sweep-derived gauge is **absent** until the first successful pass, never `0`
(the state-gauge paragraph in `docs/metrics.md`). The alert rules go into `docs/metrics.md` *and* into
`scripts/alert-rules-check.sh`, because that gate exists precisely because rules asserted in prose had
never been executed. *As shipped:* `LapilliBundleVolumeFilling` ("the bundle volume fills up") and
`LapilliRetentionNotSweeping` ("retention stops sweeping") have promtool unit tests in that script;
`LapilliRetentionCannotKeepUp` is only parsed and checked as valid PromQL by `promtool check rules`,
with no timeline test.

## What this is called

Not "reclaiming a cache". On the default install there is one copy and this deletes it; the first
draft's own "uncomfortable case" section conceded that while its title denied it. This is **bounded
local retention, with export as the durability story** — and where an installation needs evidence that
Lapilli itself cannot destroy, the answer is a destination with Object Lock, not a sentence in a design
doc.

**And Object Lock is not free of consequence, so recommending it has to say what it costs.** A
compliance-mode retention period — which is the mode `docs/design-export.md` recommends, together
with a bucket policy that denies `s3:DeleteObject*` and `s3:PutObjectRetention` — means the objects
**cannot be deleted or shortened by anyone until that period expires**, including the account root.
Bundles can hold personal data: `DESIGN.md` §5 states redaction is best-effort, logs are never
redacted at all, and a bundle carries pod names, node names, event messages and container logs.
So an erasure request (GDPR Art. 17, or the equivalent in your jurisdiction) against a bundle in
such a bucket **cannot be honoured** for as long as the lock holds.

That is a legitimate configuration — it is the point of WORM — but it makes the retention period a
legal decision, not a storage one. **Set the Object Lock retention period against the legal retention
period for what the bundles hold, not against how long the evidence is convenient to keep**, and
know what is in them before you lock them: see [`data-handling.md`](data-handling.md).

## Who can change the redaction policy

Worth stating plainly next to the paragraph above, because the two compose badly: **`redaction.mode`
is the one privacy control in this chart with no admin floor.** `reconcile.rs` uses the profile's
mode exactly as given (`Redactor::new(pspec.redaction.policy())`), so a `CaptureProfile` set to
`mode: off` makes every bundle built from that profile carry env values, container args, probe
headers and annotations **unredacted**, to every export destination that profile names — and into a
WORM bucket, where the previous section's consequence applies to the unredacted copy.

The trust boundary, as it actually stands:

| Control | Who decides | Can a profile change it? |
|---|---|---|
| export destinations | admin, in `export.destinations` | no — a profile may only *name* one |
| notify routes | admin, in `notify.routes` | no — a profile may only *name* one |
| signing | admin — under `signing.mode=kms` the chart renders the profile's `signing.mode: none` and the controller signs everything itself | no |
| **redaction** | **the profile** | **yes, including `off`** |

So the people who can turn redaction off are: anyone with `update`/`patch` on
`captureprofiles.lapilli.dev` in the release namespace (`CaptureProfile` is namespaced, and the chart
creates the default profile in the release namespace), anyone with the cluster-wide equivalent, and
anyone who can change the Helm values. The controller itself cannot — its Role asks for `get` on
`captureprofiles` and nothing more (`charts/lapilli/templates/rbac.yaml`), which is the right shape
and also means the controller cannot enforce a floor it is not given.

What keeps this honest rather than silent: `mode: off` is **recorded in the bundle** and
`lapilli verify` flags it, and the chart's `redaction` comment now names who can flip it. What is
missing is an admin floor the profile cannot go below — `redaction.minimumMode`, rendered into the
controller rather than the profile, refusing any profile that asks for less. That is a **v0.2**
roadmap item, not a pre-release change: adding an enforcement point on the capture path days before
a first tag is how a recorder stops recording.

`DESIGN.md` §5 also removes one line of the first draft's reasoning: "sealing time is self-asserted by
the controller clock; there is no independent time anchor". So "it can wait, but it cannot target" is
only true of an attacker who cannot move the clock or touch an mtime. The real mitigations are the
`Uploaded` requirement, the orphan switch, the per-sweep cap and the journal.
