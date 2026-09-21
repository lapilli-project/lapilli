# Design Review — Round 17 (bundle retention, revised before any code)

Constitution: **Loop Engineering Constitution v0.5.0**. Artifact: `docs/design-retention.md` — a
**proposal**, reviewed before implementation, as round 16 established is the cheap place to do it.

**Snapshot: the proposal as first written**, preserved here by quotation. Two lenses, one round: a
safety/evidence lens, because this is a feature that *deletes data* inside a tool whose purpose is
preserving it; and an operations lens, told to check whether the feature solves the problem it opens
with.

## Round 0

- **Category:** security-sensitive and irreversible. A delete path in an evidence recorder is the one
  feature where being wrong destroys the product's output rather than degrading it. `DESIGN.md` §11 had
  already written down three constraints for it; this round exists to check whether the proposal
  actually met them.
- **Break-even:** cost ≈ 2 critic runs. Downside: a sweep that deletes the only copy of evidence, or
  that re-arms a notification, or that does not engage before the disk fills. All three turned out to
  be present. Downside ≫ cost, by a wide margin.
- **Sizing:** 2 lenses × 1 round, at the design stage.
- **Independence: not achieved (calibration only).** Both critics were Claude. One traced file
  lifecycles through the real code; one did arithmetic from the chart's real defaults and built a
  40,000-file directory to time a scan. Calibration, not independence.

## Findings

Merged where both lenses found the same thing, which they did three times.

| # | Sev | Finding | Steelman attempt | Disposition |
|---|---|---|---|---|
| R1 | BLOCKER | **"The `.ieb` and its sidecars" was the bug.** `<incident>.notified` and `<incident>.ieb.owner` are not sidecars; they are the two permanent `O_EXCL` claims behind once-only notification and the incident-id promise. Deleting `.notified` re-arms notification — `reconcile.rs:385`'s early return stops firing, the `created < started_at` guard does not catch a capture newer than the process, the 30-minute cooldown has expired, so **a month-old incident posts to Slack as news** — and it is non-convergent, because a restart re-creates the file (`reconcile.rs:404`) and the next sweep deletes it again. Deleting `.ieb.owner` frees the id claim, so a resent webhook creates a **new** bundle carrying the old incident's identity and `kairn verify --incident X` passes on bytes collected months later, which is the replay/substitution defence `DESIGN.md` §7 claims. | None. Both lenses produced the sequence independently, and neither needs a race. | **APPLY.** The design now carries an explicit two-column list of what is reclaimed and what is never touched, because the phrase "and its sidecars" is what allowed this. |
| R2 | BLOCKER | **The safety predicate was inverted.** `ExportState::settled()` is true for `Refused`, `Conflict` and `Failed` (`crd.rs:353`), which `docs/metrics.md:45` defines as "that evidence never reached the destination and never will" — so the headline refusal permitted deleting the only copy in exactly the cases where there is no second copy, and it made the documented `KairnExportsLost` alert unactionable, since its remediation is to go get the local copy. `Refused` arrives from ordinary misconfiguration, including an S3-compatible backend that ignores conditional writes and disables the destination. Worse, the decision was read from `status`, which this codebase refuses as an input everywhere (`reconcile.rs:455`) — and the controller's own Role can patch it, so a compromised collector flipping one `Pending` to `Uploaded` obtains a **targeted** delete out of a feature whose stated mitigation is that it cannot target. | None; the enum and the doc contradict the proposal directly. | **APPLY.** Reclaim requires every destination observed as `Uploaded`, re-derived rather than read from status; the three terminal states are refusals, counted. Local-only installs get an explicit `retention.allowUnexported` switch, default off. |
| R3 | BLOCKER | **Age never engages before the disk fills.** Arithmetic from the chart's own defaults: 1 GiB PVC, capture identity per `(rule, cluster, ns/pod, minute)`, one alert over a 20-pod Deployment at Alertmanager's hourly repeat ≈ 480 captures/day ≈ 120 MB/day, so the volume is **full on day 9 with a 30-day window having deleted nothing**; a DaemonSet alert over 50 nodes fills it on day 3.6. The proposal's own open question 1 raised this and left it open. | None. The document conceded the point in its own words ("then retention has failed at its one job") and then shipped the axis that fails. | **APPLY.** `retention.maxBytes` becomes the primary bound, defaulted by the chart from `persistence.size × 0.8`; `days` is a secondary trim. And `retention.minFreeBytes` as a **preflight** `statvfs`, so a capture that cannot be sealed fails with a `pvc-full:` reason code instead of a raw ENOSPC that honours none of `reconcile.rs:174`'s convention. |
| R4 | BLOCKER | **The orphan pass is a mass-delete waiting for a housekeeping command.** `grep` proves nothing in the controller ever deletes an `IncidentCapture`, so "no live CR" is a statement about human behaviour — and one `kubectl delete incidentcapture --all`, or the *documented* CRD upgrade path (Helm does not upgrade `crds/`, so it is a delete-and-recreate that cascades every CR away), turns the next sweep into "delete every bundle older than N days", with no CR to record it in. | None. The CRD path is the project's own documented procedure. | **APPLY.** Behind `retention.reclaimOrphans`, default false; only on a complete, paged, successful list; refuses and alerts above >100 files or >5% of the population; and selects by allowlist rather than by exclusion. |
| R5 | MAJOR | **The orphan pass would have deleted the archived signing keys.** `keys/<key_id>.pub` has no CR and ages out immediately. `archive_public_key` writes only the *current* signer's key (`reconcile.rs:929`) and `copy_archived_keys` only copies to destinations that reached `Uploaded`, so on a local-only install a rotated key exists nowhere else — and deleting it makes every bundle it signed unverifiable, **including bundles safely inside the Object Lock bucket the proposal pointed to for durability.** Staging directories and `.tmp` files were also unclassified, and the proposal said "a *file* under the bundle root", which misses directories entirely. | None. | **APPLY,** and it turned into a gain: the abandoned staging directories are *uncompressed* and on the same volume, so they are the largest reclaimable thing on the broken install this feature targets. The proposal had missed its best target while aiming at its worst. |
| R6 | MAJOR | **None of the three "records" is durable.** Events expire (1 h default TTL); `status.local` dies with the CR, and the orphan case is *premised* on a human deleting the CR; counters reset on restart, which `COMPATIBILITY.md:186` states. So an hour after a sweep there is no evidence Kairn deleted anything, and §11's "deleting must be at least as recorded as capturing" was not met. | None, and the proposal's open question 3 presupposed that one of them was a record. | **APPLY.** An append-only `reclaimed.jsonl` at the bundle root, excluded from every pass, holding the incident id, UID, the bundle's `sha256`, bytes, the export states the decision rested on, the reason and the instant. `status.local` and the Event become views of it — the relationship `design-notify.md` already has between `status.notification` and the `.notified` claim. |
| R7 | MAJOR | **No bound was named, and the cost was not computed.** Round 14 and round 15 each found a loop in this crate with no time budget; this would have been the third. The unbounded part is not the directory scan — an honest failed probe measured a 40,000-entry flat directory at **0.179 s** warm — but the unpaginated CR list into a pod limited to 256 MiB, and one PATCH plus one Event per capture: enabling retention on a year-old install is ~50,000 unlinks and **100,000 API writes in a single pass**. | None. Round 15 set the standard the proposal did not meet, having computed SSAR cost rather than guessing it. | **APPLY.** `SWEEP_BUDGET` around the pass, `MAX_RECLAIMS_PER_SWEEP` with the backlog as a gauge, `spawn_blocking` for the filesystem pass, and no unpaginated list — the reflector store or `limit(500)`. |
| R8 | MAJOR | **All three gauges were wrong.** Computed by the sweep, so "still emitted when retention is off" is unimplementable; frozen at their last value on a failed pass, with no freshness series to guard an alert — which is round 14's S1 and S3 repeated, in a file that documents both; and measuring the wrong quantity, since an operator needs free bytes on the volume, not a count of the files Kairn remembers writing. One name also collided with the existing `kairn_bundle_bytes` histogram. | None. | **APPLY.** `kairn_bundle_fs_bytes{state}` from one `statvfs` on the always-on poller — O(1), correct when full, and it counts staging and leftovers a per-`.ieb` sum cannot. `kairn_local_sweep_runs_total{result}` emitted from process start as the `absent()` sentinel. Sweep-derived gauges absent, never `0`, before the first successful pass. Renamed to `kairn_reclaimed_bytes_total`. And the alert goes into `scripts/alert-rules-check.sh`, not only into prose. |
| R9 | MAJOR | **"Reclaiming a cache, not deleting evidence" was not earned**, and §11's second constraint was relocated rather than declined. `values.yaml:128` tells audit installs to "back this with WORM/object-lock storage" — so for the install the chart steers them toward, **the PVC is the locked store**, and an unlink there fails `EPERM`/`EROFS`. Separately, "it can wait, but it cannot target" rests on a clock `DESIGN.md` §5 describes as "self-asserted by the controller clock; there is no independent time anchor". | Tried: "the reframe is about where durability lives, and export is genuinely the answer." It is — but the document's own "uncomfortable case" section conceded that the default install has one copy and this deletes it, while the title denied it. A framing a document contradicts three paragraphs later is not a framing. | **APPLY.** Renamed to "bounded local retention, with export as the durability story". A failed unlink refuses, counts, and never records the bundle as reclaimed. |

**Attacks that failed, reported because they are what makes the rest worth anything.** The
`Phase`-before-`Exported` refusal is sound, since staging is removed inside `pack_into_place`
(`reconcile.rs:833`). `.summary.json` does not leak the crashed container's last words —
`without_log_line()` (`reconcile.rs:822`). Putting `retention.*` on the chart rather than in
`CaptureProfile` correctly keeps it out of reach of `patch captureprofiles`, which the controller's Role
does grant. Timer-not-reconcile matches the `spawn_state_poller` precedent. The CRD addition is legal
under `COMPATIBILITY.md` §3 (alpha, additive-only). The poller's namespaced `Api` is complete, because
every capture is created in the controller's own namespace (`webhook.rs:264`), so a namespaced list is
not silently partial. Status patching and Events genuinely do work on a full PVC — so the proposal's
open question 4 had the right belief, aimed at the wrong risk. And the directory scan the proposal
worried about is cheap, measured.

## Verdict

**Applied, with a revision large enough that the document was rewritten rather than patched.** The
shape held — a timed sweep reclaiming local files — and almost nothing else did: the primary axis
flipped from age to bytes, the safety predicate inverted, the file list became explicit in both
directions, the orphan pass grew a switch and three guards, the record became a journal, and all three
gauges were replaced.

Two of the findings were the project's own recorded lessons recurring: round 14's absent-vs-zero and
frozen-gauge rules were repeated in a design whose author had written the round-14 log, and the
missing time budget would have been the third instance after rounds 14 and 15. That is worth naming
plainly — **reading one's own review logs is not the same as applying them**, and the only thing that
reliably catches it is another pass.

What R17 does **not** establish: none of this is implemented. The next round belongs on the code, where
the remaining defects will be concrete — in particular whether the `Uploaded` re-derivation is actually
independent of `status`, and whether the journal survives the full-PVC case it exists to record.
