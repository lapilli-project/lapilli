# Design — record the perishable; the backfill half returns to premise

Status: **phase A proceeds, phase B returned to premise.** Two rounds of adversarial review:
`docs/design-review-round23.md` (5 BLOCKERs), `docs/design-review-round24.md` (11
NEW-BLOCKERs, every one of them in a round-23 fix). The failures were not scattered — all of
phase B's mechanisms broke and phase A's premise survived both rounds.

## Why this exists

A market review (vault `17 Reviews/Lapilli 시장 분석 — Review Round 1`) ran an incumbent
lens — *a team already running Grafana + Loki + Prometheus + kube-state-metrics + API audit
logs* — which refused the install and said: *ship the format and the verifier; drop the
recorder.* The owner chose neither pole: drop the recorder and the one claim that survived the
whole review is lost — the point-in-time **object body**; keep it as is and it keeps collecting
what a retention stack already holds.

Two rounds of hardening then produced a result **opposite to the market critic's guess**. The
recorder half is fine. **The backfill half is what does not work.**

## The split axis (survived both rounds)

Can this be reconstructed later from something that already stores it?

| Collector (`ieb/v1` name) | Reconstructible later? | Phase |
|---|---|---|
| `resources` (object body) | **No** — overwritten by the next apply | **A — record now** |
| `changes` (`changes.json` + `diffs/`) | **No, on a clock** — `revisionHistoryLimit` default 10 | **A — record now** |
| `logs` | Only if a log shipper runs, inside *its* retention | B |
| `events` | Only if an event exporter runs; TTL 1h either way | B |
| `metrics` | Prometheus is the store, inside *its* retention | B |

Five critics across two rounds tried to find a later reconstruction path for env, args, limits,
probes and annotations of the object that was *running* — through kube-state-metrics and through
the API audit log — and none found one. **That claim is the only load-bearing one, and it held.**

The collector is `changes`, not `diffs` (`spec/IEB-SPEC.md:311`, `manifest.rs:24`); the first
draft's `diffs` would have been rejected by `collector.rs:146` as `unknown collector`, making
phase A PARTIAL. The fallback segment's discriminator is the **log shipper**, not Prometheus:
Grafana Alerting, `vmalert`, Thanos Ruler and the Mimir ruler all POST to Alertmanager, so a
trigger implies no particular retention.

## Phase A — capture (always on, small)

`collectors_intended = ["resources", "changes"]`. Both run. The bundle verifies **OK (0)**, and
that is correct on the letter of the spec: *"The bundle is PARTIAL if and only if some name in
`collectors_intended` is not in `collectors_run`. Nothing else decides PARTIAL"*
(`spec/IEB-SPEC.md:299`). PARTIAL would be a lie about a collector that never failed. The
minimal-collector shape is already exercised in CI by the independent Python producer
(`test/spec/build_from_spec.py:46`).

**OK must stop meaning what it meant**, and the mechanism is narrower than round 23 proposed:

- The manifest carries `deferred: ["logs", "events", "metrics"]`. Unknown manifest members are
  proven tolerated — `build_from_spec.py:52` ships `x_future_field` deliberately.
- `verify` emits a notice **gated on `deferred` being present and non-empty** — *not* on
  "`collectors_intended` omits an `ieb/v1` collector", which round 23 got wrong. All four
  released v0.1.0 fixtures intend `logs` alone, so the round-23 trigger fired on every one of
  them, with text (*"deferred by profile …"*) that was false about them, and changed code sets
  that `COMPATIBILITY.md:242` pins and `:155` freezes.
- `collectors_run` / `collectors_intended` join `verify-result/v1`, which `COMPATIBILITY.md`
  makes **additive-only within v1**.

`lapilli postmortem` prints the same fact in its header; `lapilli_bundles_unsealed` and an alert
rule make it visible without opening a file.

### RBAC: derived in the controller, not in the chart

Round 23 proposed deriving `$collectorRules` from Helm values with a chart test. **That cannot
bind.** `captureprofile.yaml:1` is behind `profile.create`; the profile is selected **per
capture** (`crd.rs:196-202`); and the Role grants `update`/`patch` on `captureprofiles`
(`rbac.yaml:77`) with nothing re-rendering the chart. Chart-rendered RBAC and the profile in use
diverge silently.

Derivation belongs in the controller, which can see both. A resolved profile naming a collector
whose permission check is denied must be **refused with an Event**, not collected thinly.

`perms.rs` must re-read the profile **inside** its loop: `needs_from_cluster` is awaited once
(`main.rs:397-404`) and `spawn` reuses the value forever (`perms.rs:506-512`), so deriving from
a startup-frozen profile gives the drift detector the mirror-image blind spot. `Report`'s
tri-state map (`perms.rs:317-319`) also has no state for *deferred* — it is an absent key — and
an unreadable profile currently `debug!`s and returns `(false, None)` (`perms.rs:473-477`),
after which `check_once` logs *"every permission this install needs is held"* (`:395-398`).
Needs a fourth `NotNeeded` state and a `warn`.

### What does not change, stated honestly

**The retirement clock does not move at all.** The 19.4 KB / 62-day figure is **controller
memory per `IncidentCapture` CR** (`docs/design-trigger-and-load.md:214-222`), not bundle bytes,
and the CR is created in the webhook handler *before any collector runs* — so the perishable
profile creates the same ~120 CRs/day and reaches ~7,400 on the same day. Retirement and
retention machinery stay in full.

**And the disk figure is a floor, not a median.** `values.yaml:164-165`: *"years at the 6.2 KB
measured for a crash-looping busybox pod whose whole log is one line. **Only the last of those
is measured, and it is a floor** — a real capture carries a real log window."* The only measured
bundle is a 6.2 KB floor; **no p50 exists.** (Round 23 killed a claim for citing memory as disk;
its replacement then restated a floor as a median in the same paragraph.)

## Phase B — returned to premise

Four mechanisms were designed, four broke, and `docs/design-review-round24.md` has the evidence
for each: the `parent` signature block is unverifiable (the signed payload is the **literal
bytes of `manifest.json`**, `spec/IEB-SPEC.md:320`, so a root hash is not the preimage) and
forgeable; `<parent>-s1` overflows a 100-byte budget that `crd.rs:211-213` says is exactly full
at `83 + 1 + 16`, and where it overflows the identity binding **silently disappears**;
`redaction.mode` is one value per bundle so mixed provenance is inexpressible; and `notice`
changes no verdict, so it cannot carry a material caveat.

One underlying cause: **a human-run, unsigned, off-cluster producer merging late data into a
signed evidence bundle is fighting the format's trust model, not a gap in it.** Each refusal is
a decision the format made on purpose. A round 3 would be the fourth attempt to route around
four deliberate decisions.

So the premise to re-examine is not *how* to merge late data into a bundle, but **whether a
backfill belongs in a bundle at all** — as against a separate artifact that *references* a
signed phase-A bundle and never claims to be one.

Two things are settled regardless and carry forward:

- **The name.** `seal` already means *sign* here — `sealing.rs` is the KMS signing module, the CR
  phase is `Sealing`, and `spec/IEB-SPEC.md:293` defines `timing.sealed_at` /
  `capture_to_seal_ms`. Whatever phase B becomes, it is not `seal`.
- **No signing key leaves the controller.** Both round-23 routes broke this: a laptop key lets
  the postmortem author mint bundles that pass `verify --key`, and the in-cluster Job needs
  `create jobs` in the release namespace, which is pod-spec authorship and therefore reaches
  `.Values.signing.keySecret` through `ServiceAccount {{ fullname }}`
  (`rbac.yaml:78-92`, `:98-111`) — **the privilege the fix requires hands over the key the fix
  exists to protect.**

## Found along the way: a defect in shipped code

**A `pods/log` permission denial produces a bundle that verifies OK at coverage 100% with zero
log bytes.** `collector.rs:215-225` treats the log fetch error as soft
(`Err(e) => entry["unavailable"] = …`), `collect_logs` returns `Ok(())` at `:234`, and `:149`
pushes `logs` into `collectors_run`. `events` is the opposite — `collector.rs:327`'s
`events.list(&lp).await?` is a hard `?`, so an `events` denial **does** give PARTIAL.

This exists today and is independent of this design; it ships on its own with a negative test.
A legitimate absence (`is_kubelet_log_error` — the kubelet kept no log) must stay soft; an
infrastructure failure must not.

## Still open

- What happens to a phase-A bundle nobody ever backfills? The clocks were sized for evidence
  nobody revisits.
- Does the profile choice belong to the operator or to the alert? A cluster can have both kinds
  of workload — and this question undercuts any single derived `Needs`.
- `crd.rs:418-420` `default_collectors() -> vec!["logs"]`: the CRD's default for an omitted
  `spec.collectors` is the one collector phase A drops.
