# Design Review — Round 10 (controller metrics)

Constitution: **Loop Engineering Constitution v0.5.0**. Artifact:
`crates/lapilli-controller/src/telemetry.rs`, its call sites, [`metrics.md`](metrics.md), and
the chart's `telemetry` values. Snapshot attacked: the working-tree diff
`scratchpad/r10-metrics.diff`.

**Outcome: time-boxed, not dry.** One round, two lenses. Every finding is APPLIED except
one recorded as out of scope. The fixes are covered by unit tests and the kind E2E on 1.30
and 1.37, not by a second critic round.

## Round 0

- **Category:** a public, additive-only compatibility surface (metric names, labels and
  bucket boundaries freeze at v0.1.0) plus the operator's only view of failures.
- **Break-even:** cost ≈ 2 critic runs. The downside: a metric set that can't answer "is
  evidence being lost?", or buckets that are wrong forever. Downside > cost.
- **Sizing:** medium, 2 lenses × 1 round: SRE (would these page you correctly) and
  correctness/compatibility (exposition format, instrument placement).
- **Independence: not achieved (calibration only).** Both critics were Claude. The
  correctness critic did run the rendered output through Prometheus's own parser and linter
  (`expfmt` + `promlint`, the pair `promtool check metrics` uses), which is a real external
  check of the format.

## Findings

| # | Sev | Lens | Finding | Disposition |
|---|---|---|---|---|
| 1 | BLOCKER | both, independently | `lapilli_bundle_bytes` reused the **seconds** buckets (max 900), so every real bundle fell in `+Inf` and no size distribution existed. Re-bucketing later would delete documented `le` values, which the stability promise forbids: a one-way door. | **APPLY.** `Histogram` is generic over its bucket array; bundle sizes get byte buckets (64 KiB … 1 GiB, the producer cap) and an exact integer byte sum. Unit tests assert a populated byte bucket, and the E2E asserts sizes land in them. |
| 2 | BLOCKER | SRE (corr. found the restart half) | The gauges were sets maintained by reconcile side effects: deleting a waiting capture pinned them above zero **for the process lifetime** (reconcile is never called for a deleted object), a restart read 0 for up to an hour, and the refusal path never cleared them. The documented alerts would fire forever or stay silent. | **APPLY.** Both gauges are counted from the API by a 30 s poller and are absent until the first poll. The name-keyed sets are gone (they also grew unboundedly). |
| 3 | MAJOR | corr. | `lapilli_seal_attempts_total` counted packing failures and pre-signing staging errors as KMS failures, and a signed-but-unpacked bundle as a failure, so it couldn't be reconciled with CloudTrail as `kms.md` tells auditors to do. | **APPLY.** The counter wraps only the KMS call; `lapilli_seal_pack_failures_total` is separate. |
| 4 | MAJOR | SRE | Permanently lost exports were invisible: `settled()` is "not pending", so refused, conflict and attempts-exhausted all drove `lapilli_exports_unsettled` back to 0 — the alert resolved itself exactly when the evidence was gone for good. | **APPLY.** `lapilli_export_destinations{state}` exposes terminal states, with a non-resolving alert and a conflict-specific one (an integrity event, not a transient failure). |
| 5 | MAJOR | both | No reconcile-error counter and no phase gauge: "the alert arrived and then nothing happened" (an RBAC 403 on status patches, an API outage) moved no series at all. | **APPLY.** `lapilli_reconcile_errors_total` in `error_policy`, and `lapilli_captures{phase}` from the poller, with alerts for both. |
| 6 | MAJOR | SRE | `LapilliSigningKeyChanged` used `changes()` over a series that is always 1: rotation ends one series and starts another, so it could never fire. | **APPLY.** Compare the label against the key id auditors hold. |
| 7 | MINOR | corr. | "E2E asserts every documented series" was false: two documented names were asserted nowhere, so a rename would keep CI green. | **APPLY.** A unit test pins the exact emitted set against the documented list (and fails if a series is added without documenting it); the E2E asserts all 23 series. |
| 8 | MINOR | corr. | Histogram bucket increments happened before `+Inf`, so a scrape could see a bucket above the count. | **APPLY.** `+Inf` first. |
| 9 | MINOR | SRE | `lapilli_signing_key_info` only appears after the first KMS-signed capture, so "what key is in use" is empty on a fresh pod. | **VALID-OUT-OF-SCOPE.** The preflight pins the key at startup and the log carries it; publishing before the first seal is a small additive change, tracked. |

Also fixed while testing: the E2E asserted counters across pod restarts (helm upgrades in
the KMS scenario reset them) — it now makes its own traffic in the pod it scrapes.

## Probed and clean (from the critics)

- **Exposition format:** the rendered output parses and lints clean with Prometheus's own
  `expfmt` and `promlint`, including hostile input (NaN, `+Inf`, negative seconds, a key id
  with quotes and newlines). HELP/TYPE ordering, `_total` suffixes, `le` ordering, escaping.
- **Cardinality:** every label set is a compile-time constant; no incident id, pod or
  namespace anywhere.
- **Double counting:** a capture counts `sealed` once even when the status patch after
  packing fails (both adopt paths return without re-counting); refusals don't re-count per
  reconcile.
- **Locks:** no `std::sync::Mutex` held across an `.await`; poisoning degrades to zero.
- **Counter resets vs `increase()`:** a restart contributes 0, so no false page.
- **Scrape plumbing:** annotations and the ServiceMonitor both point at the health port, so
  a webhook NetworkPolicy can't silently kill scraping.

## Verdict

**Time-boxed, not dry.** Both BLOCKERs were caught by the two lenses independently, which
is the strongest signal this round produced: the bucket reuse and the side-effect gauges
were both invisible to the unit test that existed. The fixes are exercised by the exposition
tests and by the E2E on both Kubernetes versions; no second critic round attacked them.
