# Design — bundle lifecycle: bounded local retention

Status: **revised after review** (`docs/design-review-round17.md`). The first draft's shape survives;
its primary axis, its safety predicate, its records and all three of its gauges did not. Roadmap:
`DESIGN.md` §11, v0.2.

## The defect

**Nothing deletes a sealed bundle.** Each capture writes `<incident>.ieb` and
`<incident>.summary.json` under the bundle root, plus two claim files and, while it runs, an
uncompressed staging directory. Nothing ever removes any of it. The chart's default PVC is **1 GiB**
(`charts/kairn/values.yaml:131`).

The consequence is worse than losing old bundles. **When the PVC is full, new captures fail** — the
recorder stops recording, and what it stops recording is the incident happening now. Today that
failure is also ugly: ENOSPC surfaces from the `O_EXCL` owner-file create as
`status.message = "No space left on device (os error 28)"`, which honours none of the reason-code
convention `reconcile.rs:174` sets for capture errors.

It is also an unbounded liability. `DESIGN.md` §5: redaction is **best-effort**, so a bundle may hold
personal data the redactor missed. Keeping everything forever is not the conservative choice.

## Bytes first, age second

The first draft proposed `retention.days` alone. **On this chart's defaults, an age window never
engages before the disk fills.** Measured bundle sizes are kilobytes to a few megabytes
(`kairn-bundle/src/pack.rs:16`; a real kind capture lands in the `le=1048576` bucket,
`test/e2e/run.sh:313`). Capture identity is per `(rule, cluster, namespace/pod, minute)`
(`webhook.rs:229`), so one alert over a 20-pod Deployment at Alertmanager's default hourly repeat is
~480 captures/day ≈ 120 MB/day: **the 1 GiB PVC is full on day 9 with a 30-day window having deleted
nothing.** A DaemonSet alert across 50 nodes fills it on day 3.6. One capture may legally be the whole
volume — `PRODUCER_MAX_BYTES = 1 GiB` (`hashtree.rs:101`).

So:

| Value | Default | What it does |
|---|---|---|
| `retention.maxBytes` | `persistence.size × 0.8`, passed to the controller by the chart | The primary bound. Reclaim oldest-reclaimable-first until under it. |
| `retention.days` | `0` = off | A secondary trim, for the liability argument rather than the capacity one. |
| `retention.minFreeBytes` | 64 MiB | Preflight: a capture that cannot possibly be sealed fails **before** collecting. |
| `retention.reclaimOrphans` | `false` | See "Orphans", below. This one is dangerous and defaults off. |

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
  which `reconcile.rs:397` already anticipates.

**Never touched, permanently:**

- `<incident>.notified` — **not a sidecar; a claim.** `design-notify.md` states "the claim is never
  released… making it releasable would make replay possible", and `notify.rs:9` explains why it is a
  file and not a status field. Deleting it re-arms notification: `reconcile.rs:385`'s early return
  stops firing, the `created < started_at` guard does not catch a capture newer than the process, the
  30-minute cooldown has long expired, and **a month-old incident is announced to Slack as news.** It
  is also non-convergent — a restart re-creates the file at `reconcile.rs:404` and the next sweep
  deletes it again, forever.
- `<incident>.ieb.owner` — the `O_EXCL` incident-id claim behind the promise "its bundle is never
  overwritten" (`reconcile.rs:634`). Release it and a resent webhook can create a **new** bundle
  carrying the old incident's identity, so `kairn verify --incident X` passes on bytes collected
  months later. That is the replay/substitution defence `DESIGN.md` §7 claims.
- `keys/<key_id>.pub` — the archived signing keys. `archive_public_key` only ever writes the *current*
  signer's key (`reconcile.rs:929`) and `copy_archived_keys` only copies to destinations that reached
  `Uploaded`, so on a local-only install a rotated key exists **nowhere else**. Deleting it makes
  every bundle it signed unverifiable, including bundles safely in an Object Lock bucket.

The two claim files are four small inodes per capture and are not the capacity problem. If their
growth ever needs addressing it needs its own design, with a tombstone that preserves the claim's
*meaning* rather than an unlink.

## When it refuses

**A bundle is reclaimable only when a remote copy demonstrably exists**, or when the operator has
explicitly opted into local-only reclamation.

The first draft used `ExportState::settled()`. That is wrong in the worst direction: `settled()` is
true for `Refused`, `Conflict` and `Failed` (`crd.rs:353`), and `docs/metrics.md:45` defines exactly
those as "**that evidence never reached the destination and never will**". So the draft's headline
refusal permitted deleting the only copy precisely when there is no second copy — and it makes the
documented `KairnExportsLost` alert unactionable, because its remediation is "go get the local copy".
`Refused` arrives from ordinary misconfiguration: a mistyped destination name, a `too-large` bundle,
or an S3-compatible backend that ignores conditional writes and disables the destination outright.

So: reclaim requires **every referenced destination observed as `Uploaded`**. `Refused`, `Conflict`
and `Failed` are refusals, counted, so "retention cannot keep up" is visible rather than silent.
`Conflict` especially: it means a different object already holds this incident's key, so the local
file is the only thing that can show the remote is not the evidence.

**And the decision is never read from `status`.** This codebase refuses that everywhere —
`reconcile.rs:455` recomputes "never from `status`, which anyone with patch access could point
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
because `values.yaml:128` tells audit installs to "back this with WORM/object-lock storage": for them
the PVC *is* the locked store, so §11's "the store gets a vote" is not declined by staying local — it
is relocated, and it still has to be honoured.

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
the CR. Counters reset on restart, which `COMPATIBILITY.md:186` states outright.

So there is one durable record and it is authoritative: an append-only **`reclaimed.jsonl`** at the
bundle root, excluded from every pass, size-capped and rotated. One line per reclaim: incident id,
capture UID, the bundle's `sha256` from its manifest, byte count, the export states the decision was
made on, the reason, and the RFC 3339 instant.

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
  pod whose memory limit is 256 MiB (`values.yaml:150`) is an OOM risk, and an OOMKill discards the
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

- `kairn_bundle_fs_bytes{state="used"|"free"}` — one `statvfs` of the bundle root on the **always-on**
  poller, not the sweep. O(1), correct when the volume is full, and it counts staging directories and
  leftovers that a per-`.ieb` sum cannot. Every cluster already scrapes
  `kubelet_volume_stats_available_bytes`; this exists because it is scoped to the bundle root and
  needs no kubelet-metrics access.
- `kairn_local_sweep_runs_total{result}` — emitted **from process start**, so `absent()` works on it
  and an alert can guard against a stale pass with `unless on(instance)`, which is round 14's S1 and S3
  applied rather than rediscovered.
- `kairn_bundles_reclaimed_total{reason}` and `kairn_reclaimed_bytes_total` — renamed from the first
  draft's `kairn_bundle_bytes_reclaimed_total`, which collided with the existing `kairn_bundle_bytes`
  histogram of bundle sizes.

Any sweep-derived gauge is **absent** until the first successful pass, never `0`
(`docs/metrics.md:39`). The alert rules go into `docs/metrics.md` *and* into
`scripts/alert-rules-check.sh`, because that gate exists precisely because rules asserted in prose had
never been executed.

## What this is called

Not "reclaiming a cache". On the default install there is one copy and this deletes it; the first
draft's own "uncomfortable case" section conceded that while its title denied it. This is **bounded
local retention, with export as the durability story** — and where an installation needs evidence that
Kairn itself cannot destroy, the answer is a destination with Object Lock, not a sentence in a design
doc.

`DESIGN.md` §5 also removes one line of the first draft's reasoning: "sealing time is self-asserted by
the controller clock; there is no independent time anchor". So "it can wait, but it cannot target" is
only true of an attacker who cannot move the clock or touch an mtime. The real mitigations are the
`Uploaded` requirement, the orphan switch, the per-sweep cap and the journal.
