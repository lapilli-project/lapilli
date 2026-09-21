# Design Review — Round 14 (the API-server reachability signal)

Constitution: **Loop Engineering Constitution v0.5.0**. Artifact: the three new `lapilli_apiserver_*`
series and everything that reads them — `crates/lapilli-controller/src/telemetry.rs`,
`docs/metrics.md`'s alert rules, `docs/egress.md`'s verification recipe, `docs/COMPATIBILITY.md` §2,
and the metrics assertions in `test/e2e/run.sh`.

**Snapshot `5e46cc7e02c631a5`** (`git hash-object` of the reviewed diff). Three lenses, one round.

The live-cluster lens ran against that snapshot while the other two lenses' fixes were being
applied, so its evidence describes the **pre-fix** build. That is stated here rather than hidden: it
is why L2 measures a 70 s flip that the bound now prevents, and why its findings arrive as
confirmations of C1 rather than as news.

## Round 0

- **Category:** compatibility surface and behaviour-defining. `COMPATIBILITY.md` §2 promises that
  within a major no series is renamed, retyped or given an extra label — so a name or a label set
  chosen wrongly here cannot be corrected later. The alert rules are the operator's only warning
  for the failure that silently halts the product.
- **Break-even:** cost ≈ 3 critic runs. Downside is two-sided and both sides are expensive: an
  alert that never fires means the product stops and nobody is told, which is the exact hole this
  change exists to close; an alert that fires on every rollout means alert fatigue, and the real
  page is ignored. Downside ≫ cost.
- **Sizing:** 3 lenses × 1 round, with a second round only if a fix looked shaky. Correctness /
  concurrency; SRE / Prometheus semantics; and a live-cluster lens.
- **Independence: not achieved (calibration only).** All three critics were Claude. Two of them
  **ran** the artifact rather than reading it — one wrote failing tests against the real poller and
  a fake API server, one executed the documented alert rules under `promtool test rules` — and the
  third was told to stand up a cluster and break it. That is calibration, not independence; the
  external review remains a release gate.

## Findings

### Correctness / concurrency lens

Verdict as given: **NEW-BLOCKER.**

| # | Sev | Finding | Steelman attempt | Disposition |
|---|---|---|---|---|
| C1 | BLOCKER | A poll that **hangs** instead of failing never calls `apiserver_poll` at all, so the gauge freezes at `1` for the client's 295 s read timeout — and a hang is exactly what a NetworkPolicy that DROPs looks like from inside the pod, which is the case `docs/egress.md` is written about. The doc's claim that the gauge "goes to 0 within one poll interval" was false. | Tried "surely the client has a shorter default": it does not. `kube-client-0.99.0/src/config/mod.rs:392` — `DEFAULT_READ_TIMEOUT = 295s`. A probe against a blackholing fake API server missed **250 consecutive intervals** while `/metrics` still said `lapilli_apiserver_reachable 1`, `failed 0`. | **APPLY.** `tokio::time::timeout(budget, api.list(…))`, the budget being the poll interval, and the elapsed case counts as `unreachable`. Measured: reverting the bound makes the new test sit for **295.77 s** — the read timeout, to two decimal places. |
| C2 | MAJOR | Every test drove `Metrics::apiserver_poll` directly and **nothing drove its only caller**, so the wiring — the whole behaviour under review — had no coverage. Two mutations of the loop, one that never records a failure and one that swaps success for failure and therefore inverts the metric's meaning, both left all 75 tests green. | None. This is the project's recurring failure for the fourth time, and the critic proved it rather than asserting it. | **APPLY.** `poll_once(api, m, budget)` extracted and tested against a real HTTP server: success, a 403, and a blackhole. Both of the critic's mutations now fail, as does misclassifying a 403, as does dropping the timeout. |
| C3 | MAJOR | The clock failure is swallowed. If `duration_since(UNIX_EPOCH)` errors, `polls_ok` still increments and the verdict still becomes OK, but the timestamp stays `0` and `render` suppresses it — so the series is absent *forever* while the gauge says the poll worked, and `COMPATIBILITY.md` §2 tells consumers to read absence as "not known yet". One state carrying two meanings. | None; the `if let Ok(since)` is quoted and the consequence follows. | **APPLY,** and root-cause-merged with S2: the alert no longer reads this series at all, which downgrades the consequence from "the staleness alert is permanently unevaluable" to cosmetic. The swallow is now a `tracing::error!`. |
| C4 | MAJOR | "A lost RBAC binding counts as unreachable" is true of exactly one of the chart's three bindings — the poller's own `list` — and the claim was made in the help string, the CHANGELOG and two docs. Sharper: a 403 means the API server **answered**, so calling that `reachable 0` is itself a false statement. | Half survives. The gauge does measure what it measures; the overclaim and the *name* are the defects. The sub-claim that "a 403 on `pods/log` surfaces nowhere" is **REFUTED**: `reconcile.rs:671` increments `collector_failure()` once per collector that did not run, so it surfaces as `lapilli_collector_failures_total` and a PARTIAL bundle, both documented with alerts. What survives of it is narrower — those only appear once a capture runs, so a broken collector binding is found during an incident rather than before one. | **APPLY.** Renamed to `lapilli_apiserver_poll_ok`, claims narrowed everywhere, and `result` now says whether the server answered. The collector-binding gap is logged as a dependency below. |
| C5 | MINOR | `apiserver_last_ok`'s store and the verdict's `swap` are independent relaxed writes, so a scraper on another core can pair a fresh verdict with a stale timestamp; and `render` reads `state` before the verdict while the poller writes them in that same order, so a scrape straddling a recovery can emit a healthy verdict beside the outage's gauges — breaking the invariant the code states in its own comment. | None, and the fix is two words. | **APPLY.** `Ordering::Release` on the publishing swap, `Acquire` in `render`, and the verdict load hoisted above the `state` lock so the stale-looking direction is the one a straddling scrape gets. |

### SRE / Prometheus lens

Verdict as given: **should not ship in this form.** Every finding was produced by running the real
rules under `promtool test rules` (Prometheus 3.1.0), not by reading them.

| # | Sev | Finding | Steelman attempt | Disposition |
|---|---|---|---|---|
| S1 | BLOCKER | Neither alert fires when the controller is not scraped **at all** — a rule that selects on a series cannot fire once that series has disappeared; it does not even go pending. So a stuck rollout, a Pending pod, a CrashLoop, a scale to 0 or a policy on port 8081 leaves the cluster with no evidence recorder and total alert silence, which is the failure class this change claims to close. The chart makes it worse: `replicas: 1` with `strategy: Recreate`, so no second pod keeps reporting. | Tried "operators already have a platform-wide `up == 0` rule": that does not help, because a target that has gone away has no `up` series either — only `absent()` sees it. And `grep` finds no `absent(` or `up` anywhere in the repo. | **APPLY.** `LapilliNotReporting` on `absent(sum(lapilli_apiserver_polls_total{result="ok"}))`, which works because the counter — unlike both gauges — is emitted from process start. |
| S2 | MAJOR | `LapilliApiServerBlindTooLong` is dead in the one scenario it was advertised for: a pod that restarted straight into blindness never emits a last-success timestamp, so there is nothing to subtract. And with no `for:` it subtracts a *pod's* clock from Prometheus's, so 16 minutes of skew pages a perfectly healthy controller immediately. | Tried "in-cluster clock skew is rare": maybe, but the blind-from-start hole has nothing to do with skew, and that hole is the whole purpose of the rule. | **APPLY.** The input is now `sum without(result) (increase(lapilli_apiserver_polls_total{result="ok"}[5m])) == 0` with `for: 15m` — only Prometheus's clock, no prior success needed, and it catches a frozen poller too. |
| S3 | MAJOR | One outage pages three times; one of those pages is a lie produced by this file's own design; and the promised escalation cannot be routed because not one rule in `docs/metrics.md` carries a `severity` label. The lie: the state gauges deliberately hold their last value through an outage, so `LapilliCapturesStuck` fires saying captures are stuck when in truth the *picture* is stale. Also `description: "Blind for {{ $value }}s"` — `$value` on a filter comparison is the left-hand sample, i.e. `0`, so every page read "Blind for 0s". | None on any of the three; the `$value` result was reproduced verbatim by a test that matched the rendered annotation. | **APPLY.** `unless on(instance) lapilli_apiserver_poll_ok == 0` on `LapilliCapturesStuck` and `LapilliExportsUnsettled`; the `$value` text replaced with the `result` breakdown, which is the actionable thing; `severity` on the escalation pair, and a note that the rest of the file is examples for the operator to label. |
| S4 | MAJOR | Every verification path added here proves only `== 1`, so a hardcoded "the poll succeeded" would pass the entire E2E. Worse, `docs/egress.md`'s recipe is framed as "the fastest confirmation you did not lock the controller out" — but the gauge holds its pre-policy `1` for up to 30 s, so running it right after `kubectl apply` prints a green answer for a controller you have just locked out. The recipe was also wrong mechanically: an unanchored `grep` also matches the `# HELP`/`# TYPE` lines, and the backgrounded port-forward is read before it is ready. | None. It is the same blind spot as C2 seen from the operator's side. | **APPLY.** Two negative E2E steps (revoke the poller's Role → assert `0`, counted as `forbidden` and **not** `unreachable`, timestamp preserved, pod still Ready and unrestarted → restore → assert recovery); `AGE` now read from the node's clock rather than the host's; the recipe waits an interval, anchors the grep, and says plainly why. |
| S5 | MINOR | This patch's own edit to `COMPATIBILITY.md` states a guarantee the code does not honour — "absent until the first poll *succeeds*", while the code and its test emit `0` on a first poll that fails — and freezes the metric in the least useful shape, with no way to tell RBAC-denied from network-unreachable, which §2 then forbids adding later. | None on the contradiction. On the label: it is genuinely now-or-never, and the alert's own remediation text ("check RBAC and any egress policy") was the confession. | **APPLY,** by a different route than proposed: rather than a second label, `result` carries the outcome — `ok`, `forbidden`, `unauthorized`, `not-found`, `api-error`, `unreachable`. One label, no inconsistent label sets across samples of the same metric, and §2 explicitly permits **new label values**. The split is drawn where it is reliable: `kube::Error::Api` means the server answered, anything else means no answer came. |

### Live-cluster lens

Method: a kind cluster of its own, the chart installed from source, and then the controller
actually broken — four different ways. Verdict as given: **MAJOR**, with the premise confirmed.

| # | Sev | Finding | Steelman attempt | Disposition |
|---|---|---|---|---|
| L1 | MAJOR | **A new hole, the twin of the one this change closes.** Removing only the `create` verb leaves the gauge at `1` while every capture is impossible: the webhook answers 500 and *no series moves at all* — `rejected` stays 0, because the 500 was in no bucket. Read from a live cluster: `captures_total{sealed,refused,failed} 0`, `collector_failures 0`, `reconcile_errors 0`, `poll_ok 1`, `webhook_requests_total{rejected} 0`, and `No resources found`. | Tried "Alertmanager's own `notifications_failed_total` covers it, and `docs/metrics.md` already tells operators to alert on Alertmanager's delivery": that is the sender's metric and it is real coverage, but Lapilli returning a 500 it generated and counting nothing is still wrong — the operator looking at Lapilli's own endpoint sees a healthy install. | **APPLY.** `lapilli_webhook_requests_total{result="error"}`, a `LapilliWebhookErroring` alert, and an E2E assertion — the negative step has the rules revoked already, so firing one alert there exercises exactly this path. |
| L2 | MAJOR | The headline number was wrong for the very failure `docs/egress.md` is about. With egress to the API server black-holed at the node's `iptables -t raw`, the `list` **hung for 60.0 s** before erroring and the gauge took **70 s** to flip — not "within one poll interval (30 s)" — after which `failed` accrued every ~46–62 s rather than every 30 s. | None. This is C1 measured on a real cluster instead of against a fake server, arriving at the same place independently. | **APPLY,** the same fix as C1, and every number in the docs corrected: an *error* flips it within 30 s (measured: a revoked RoleBinding, 30 s), a *hang* takes up to two intervals, about 60 s. |
| L3 | MINOR | A deleted CRD was reported as "the API server cannot be reached" while `kubectl get --raw /version` answered perfectly — and the alert then sent the operator to check RBAC and egress. | None. | **APPLY,** already covered by S5's classification: that case is now `result="not-found"`. |
| L4 | MINOR | "Absent until the first poll returns (within 30 s of startup)" is off by three orders of magnitude. The poller polls *before* its first sleep: measured **6.2 ms** after the listener came up, and the very first obtainable scrape, 162 ms in, already carried all three series. So the documented absence describes a state no scrape will see, and the `for: 3m` rationale of "rides out a rollout's first interval" was reasoning about nothing. | None; two timestamps settle it. | **APPLY.** Both docs corrected, and the `for: 3m` justification now rests on what it actually needs to ride out — one lost poll and the ~60 s a hung poll takes to be noticed. |
| L5 | MINOR | The E2E only ever exercised the `1` path, so CI would pass with the gauge hard-coded. Separately: a `deny-all-egress` NetworkPolicy was applied and polls kept succeeding for 76 s — **kindnet does not enforce NetworkPolicy**, so no conclusion may be drawn from one in kind. | None; it is S4 from the operator's side, and the kindnet point was proved rather than assumed. | **APPLY** (the negative steps), and the kindnet limit is recorded below as a coverage gap rather than papered over. |

**The premise held, which was the thing most worth checking.** Through all three real outages the
pod stayed `1/1 Running` with `RESTARTS 0`, `Ready=True`, and `/healthz` answering `ok` — so the
metric is necessary rather than a restatement of pod status, and nothing here restarts a controller
that a restart could not help. Counters reset on a pod delete and the gauge was back at `1` in the
first scrape obtainable. The transition was logged exactly once per change (3 warnings for 3
outages, 4 recoveries counting startup) — but the recovery line was byte-identical to the startup
line, so a log search could not tell a recovery from a restart. Fixed: `apiserver_poll` now returns
a `Transition`, and a recovery says "usable **again**".

**Attacks that failed, which is what makes the rest worth anything.** The doc's claim that the
timestamp alert "catches a poller that stopped polling without failing" was checked against the
code and **holds**. `AtomicU64::default()` is confirmed to be the unknown state, and no path
records a poll with neither branch taken. There is exactly one `spawn_state_poller` call site, the
chart pins `replicas: 1`, and there is no leader election anywhere, so the transition detector
cannot lose or duplicate an edge — and `swap` would be atomic even if there were two pollers. The
metric-name suffixes were checked against Prometheus convention and are correct (`_total`,
`_timestamp_seconds`). The poller does not interfere with the SIGTERM path. The `sum`-free forms of
two rules were the only naming problem found, and only because the rules were executed.

## A gate that was missing, added here

The root cause behind both C2 and S4 is the same: **nothing executed the thing being reviewed.**
The unit tests exercised a primitive instead of its caller, and the alert rules had never been run
at all — they are documentation, since the chart deliberately ships no `PrometheusRule`.

`scripts/alert-rules-check.sh` now extracts the YAML block from `docs/metrics.md`, checks every
expression is valid PromQL, and runs `promtool test rules` over the five timelines that matter: a
target that disappears, a pod blind from its first poll, a healthy pod with a badly skewed clock,
an outage that must not also page about stuck captures, and a genuinely stuck capture that still
must. It is wired into `scripts/release-check.sh`. Verified to bite: reverting
`LapilliApiServerBlindTooLong` to the pod-clock expression fails it, and so does dropping the
`unless` guard.

Running it immediately found one more defect in my own fixes — `absent(…)` and `increase(…)`
inherit the selector, so both new pages would have arrived labelled `result="ok"` while announcing
that the controller was broken. Fixed with `absent(sum(…))` and `sum without(result) (…)`.

## A process failure of my own, in the fix for S4

The negative E2E step I added to close S4 **failed the gate on its first real run**, and it failed
for the reason this project keeps relearning: I put an error-swallowing construct inside a
verification path.

The step revoked the poller's Role by patching `rules` to `[]`, having first saved the object with
`kubectl get role -o yaml`, and restored it with `kubectl apply -f` — wrapped in `|| true`. A
`get -o yaml` carries `metadata.resourceVersion`, which the API server treats as an
optimistic-concurrency precondition, so re-applying after the revoke fails with
`Operation cannot be fulfilled … the object has been modified`. Reproduced on a scratch cluster
rather than guessed:

```console
$ kubectl -n t patch role r --type merge -p '{"rules":[]}'
$ kubectl apply -f /tmp/r.yaml
Error from server (Conflict): … the object has been modified; please apply your changes to the
latest version and try again
```

The `|| true` meant the restore failed silently, and the next step then reported the *controller*
as having failed to recover — a true assertion about a false premise. The two real findings
underneath, `poll_ok=0` counted as `forbidden` and the refused capture counted as
`webhook result=error`, had both already passed on the live cluster.

Fixed by capturing `-o jsonpath='{.rules}'` and restoring with a merge patch, which involves no
resourceVersion; and the restore is no longer allowed to fail quietly — the recovery step now fails
loudly if the rules did not come back, because a recovery assertion against an unrestored Role
proves nothing. The lesson is the narrow, repeatable one: **`|| true` in a check turns a broken
check into a passing one**, and it belongs in cleanup paths, never in the setup a later assertion
depends on.

## Verdict

**Applied, and not declared dry.** Two lenses returned a BLOCKER each against code I had just
written and believed correct, and one of the blockers (C1) meant the feature did not work in the
scenario it was built for. Four mutations that previously survived the whole unit suite now fail,
and the alert rules are executed rather than asserted.

**Still open, logged as dependencies rather than fixed here:**

- A broken **collector** RBAC binding leaves `lapilli_apiserver_poll_ok` at `1`. It is not invisible
  — `lapilli_collector_failures_total` and a PARTIAL bundle report it — but only once a capture runs,
  so it is discovered during an incident rather than before one. A startup permission self-check
  (`SelfSubjectAccessReview` over the verbs the collectors need) would close it and is the next
  thing worth building here.
- The negative E2E steps revoke RBAC, which is a 403. **Nothing in CI produces the `unreachable`
  case on a live cluster**: kindnet does not enforce NetworkPolicy — proved in this round, not
  assumed — and the live lens had to reach for `iptables -t raw` on the node to make a blackhole.
  That path is covered by the blackhole unit test and by one hand-run experiment, not by the gate.
- A 295 s stall was never reproduced; the longest real hang measured was 60 s. The bound makes the
  distinction moot, but the claim "up to 295 s" rests on the client's constant, not on a measurement.
- Round 13's R14 targets were not attacked, because this round's artifact displaced them: the
  `JoinSet` lifetime after `drain` returns, `RECONCILE_GRACE` against a KMS-`Sealing` capture, and
  whether `ATTEMPTS_DRAINING = 1` leaves the rate-cap notice unsendable at shutdown.
