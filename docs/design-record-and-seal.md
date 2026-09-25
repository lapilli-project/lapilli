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

The collector is `changes`, not `diffs` (`spec/IEB-SPEC.md` §6 required-files table,
`manifest.rs` `required_files()`); the first
draft's `diffs` would have been rejected by `collector.rs:146` as `unknown collector`, making
phase A PARTIAL. The fallback segment's discriminator is the **log shipper**, not Prometheus:
Grafana Alerting, `vmalert`, Thanos Ruler and the Mimir ruler all POST to Alertmanager, so a
trigger implies no particular retention.

## Phase A — capture (always on, small)

`collectors_intended = ["resources", "changes"]`. Both run. The bundle verifies **OK (0)**, and
that is correct on the letter of the spec: *"The bundle is PARTIAL if and only if some name in
`collectors_intended` is not in `collectors_run`. Nothing else decides PARTIAL"*
(`spec/IEB-SPEC.md` §6, "Coverage and required files"). PARTIAL would be a lie about a collector that never failed. The
minimal-collector shape is already exercised in CI by the independent Python producer
(`test/spec/build_from_spec.py:46`).

**OK must stop meaning what it meant**, and the mechanism is narrower than round 23 proposed:

- The manifest carries `coverage.deferred: ["logs", "events", "metrics"]` (IEB rule 6). It is
  omitted when empty, so a profile that defers nothing seals to the bytes it always did;
  regenerating the fixture set left every existing `.ieb` byte-identical. Two rules are FAILED
  when broken — no duplicates, disjoint from `collectors_intended` — because a declaration
  nothing checks is the inert shape round 24 found. A name this verifier does not know is a
  **notice, not a failure**: collector names are additive within the major, and a verifier that
  failed on a newer name would fail every future bundle (a critic caught the first draft doing
  exactly that). `null` reads as empty.
- `verify` emits a notice **gated on `deferred` being non-empty** — *not* on
  "`collectors_intended` omits an `ieb/v1` collector", which round 23 got wrong. All four
  released v0.1.0 fixtures intend `logs` alone, so the round-23 trigger fired on every one of
  them, with text that was false about them, and changed code sets that `COMPATIBILITY.md`
  pins. The notice makes no coverage claim, because on a PARTIAL bundle it is not 100%.
- The verdict line itself carries `(deferred: …)`: notices go to stderr and the verdict to
  stdout, so a `verify > log` capture would otherwise keep `coverage=100%` and lose the caveat.
- `collectors_run` / `collectors_intended` / `deferred` join `verify-result/v1`, which
  `COMPATIBILITY.md` makes **additive-only within v1**.
- `Summary` carries `collectors_deferred`, so `lapilli postmortem` (header and inventory) and a
  notification name the set. Before that, every surface a human reads showed a deferred bundle
  as a full green capture.

The profile declares it: `CaptureProfile.spec.deferred` (chart `profile.deferred`). A name in
both `collectors` and `deferred` is refused by the **API server** — a CEL rule the CRD generator
injects, since the schema derive cannot express disjointness — and by the reconciler before
anything is collected, for an API server that does not enforce CEL. It is a claim that the data
exists elsewhere, not a way to turn a collector off quietly. `lapilli_deferred_captures_total`
counts captures made under such a profile; `test/e2e/deferred.sh` proves the whole arc on kind —
release gate of 2026-09-24, Kubernetes 1.30.0 and 1.37.0, four steps green on both.

### RBAC: derived in the controller, not in the chart

Round 23 proposed deriving `$collectorRules` from Helm values with a chart test. **That cannot
bind.** `captureprofile.yaml:1` is behind `profile.create`; the profile is selected **per
capture** (`IncidentCapture.spec.profile`, `crd.rs`); and the Role grants `update`/`patch` on `captureprofiles`
(`rbac.yaml:77`) with nothing re-rendering the chart. Chart-rendered RBAC and the profile in use
diverge silently.

Derivation lives in the controller, and the decisions are in
`docs/design-permissions-by-profile.md`: needs are the **union over every profile**, re-derived
**every pass**; a check no profile needs is recorded `NotNeeded`, never omitted; an unreadable
profile list narrows nothing and asks everything. Round 24's phrasing here — *"refused with an
Event, not collected thinly"* — was decided the other way, on purpose: a denied read fails its
collector and the bundle is PARTIAL (evidence first), and a `CollectorDenied` Event on the
capture names the denied check and the self-check's time (never silent). Refusing on the
strength of a self-check would lose evidence to a guess.

### What does not change, stated honestly

**The retirement clock does not move at all.** The 19.4 KB / 62-day figure is **controller
memory per `IncidentCapture` CR** (`docs/design-trigger-and-load.md` §3.3), not bundle bytes,
and the CR is created in the webhook handler *before any collector runs* — so the perishable
profile creates the same ~120 CRs/day and reaches ~7,400 on the same day. Retirement and
retention machinery stay in full.

**And the disk figure is a floor, not a median.** `charts/lapilli/values.yaml`, the
`persistence.size` comment: *"years at the 6.2 KB measured for a crash-looping busybox pod whose
whole log is one line. **Only the last of those is measured, and it is a floor** — a real capture
carries a real log window."* The population behind that number is n=201 demo bundles of one thin
workload: p50 6.2 KB, max 6.7 KB (`docs/design-retention.md`). That is a median of *demo*
bundles, which makes it a floor for real workloads, not a median of them. (Round 23 killed a
claim for citing memory as disk; its replacement then restated a floor as a median in the same
paragraph. *Update (2026-09-25):* an earlier revision of this paragraph said "no p50 exists";
one does, for the demo population, and the sentence above is the one this document, retention
and trigger-and-load now share.)

## Phase B — returned to premise

Four mechanisms were designed, four broke, and `docs/design-review-round24.md` has the evidence
for each: the `parent` signature block is unverifiable (the signed payload is the **literal
bytes of `manifest.json`**, `spec/IEB-SPEC.md` §8 Signature, so a root hash is not the preimage) and
forgeable; `<parent>-s1` overflows a 100-byte budget that the `clusterId` pattern's comment in
`crd.rs` says is exactly full at `83 + 1 + 16`, and where it overflows the identity binding
**silently disappears**;
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
  phase is `Sealing`, and `spec/IEB-SPEC.md` §5's manifest table defines `timing.sealed_at` /
  `capture_to_seal_ms`. Whatever phase B becomes, it is not `seal`.
- **No signing key leaves the controller.** Both round-23 routes broke this: a laptop key lets
  the postmortem author mint bundles that pass `verify --key`, and the in-cluster Job needs
  `create jobs` in the release namespace, which is pod-spec authorship and therefore reaches
  `.Values.signing.keySecret` through `ServiceAccount {{ fullname }}`
  (`rbac.yaml:78-92`, `:98-111`) — **the privilege the fix requires hands over the key the fix
  exists to protect.**

## Found along the way: a defect in shipped code (fixed in `a1e9485`, 2026-09-23)

**A `pods/log` permission denial produced a bundle that verified OK at coverage 100% with zero
log bytes.** `collect_logs` in `collector.rs` treated the log fetch error as soft
(`Err(e) => entry["unavailable"] = …`), returned `Ok(())`, and `logs` was pushed into
`collectors_run`. `events` was the opposite — its `events.list(&lp).await?` is a hard `?`, so an
`events` denial **did** give PARTIAL.

It was independent of this design and shipped on its own, with a negative test
(`a_denied_log_read_fails_the_collector_rather_than_reading_as_coverage`). As shipped,
`collect_logs` collects every fetch error and fails the collector when any occurred, after
writing `logs/index.json` so the denial itself is sealed and hashed; a legitimate absence
(`is_kubelet_log_error` — the kubelet kept no log) stays soft, an infrastructure failure does
not.

## Still open

- What happens to a phase-A bundle nobody ever backfills? The clocks were sized for evidence
  nobody revisits.
- Does the profile choice belong to the operator or to the alert? A cluster can have both kinds
  of workload — and this question undercuts any single derived `Needs`.
- ~~`default_collectors() -> vec!["logs"]`: the CRD's default for an omitted `spec.collectors`
  is the one collector phase A drops.~~ *Resolved (`22674cb`, 2026-09-24):* `default_collectors()`
  in `crd.rs` now returns `[logs, resources, events, changes]`, the chart's default
  (`docs/design-permissions-by-profile.md` rule 6).
