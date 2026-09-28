# Design — what triggers a capture, and what a storm of them costs

Status: **measured and implemented.** Everything in §4 is shipped: `MAX_BODY` is 1 MiB in
`webhook.rs`, the chart defaults are `webhook.maxCapturesPerPayload: 50` and
`reconcileConcurrency: 2`, both probes carry explicit timeouts in `deployment.yaml`, and the
storm suite runs last in `test/e2e/run.sh`. §5's open questions stay open, except Q9, which
`docs/design-capture-retirement.md` resolved. Round logs: `docs/design-review-round21.md` (this
document) and `docs/design-review-round22.md` (§3.3's fix).

§3's tables are reproduced by `test/e2e/storm-sweep.sh size 20 50 100` and
`test/e2e/storm-sweep.sh conc 0 1 2 8` — the tool gives each row a fresh controller pod and reads
back the concurrency the process reports rather than the one it was asked for. It mutates the
release, so it is not the gate's harness; `test/e2e/storm.sh` is, and it changes nothing.

§2.3 is **not** reproducible that way. Its probe latencies, thread states and the schedstat reading
came from one-off probes on the node, and they are quoted here rather than produced by any command.
That is a gap, carried in the round log.

This exists because of one question: *if it is heavy, nobody will run it.* An evidence recorder that
falls over during a large incident records the small ones and misses the big ones, which is the
worst shape a failure can have here.

The short answer: **at the sizes anyone has measured — up to 50 alerts in one payload — memory was
not what failed.** What failed was a liveness probe that killed the controller mid-capture, and a
body limit that discarded whole payloads with nothing able to see it. Neither is *solved*: both are
now **bounded and visible**, which is a weaker and more accurate word. And the envelope is small —
the largest completed storm is 50 alerts, while the body limit admits about 990 — so §4's new
per-payload cap exists to keep the product inside the range where these numbers mean anything.

## 1. What triggers a capture today — all of it

```rust
for alert in payload.alerts.iter().filter(|a| a.status != "resolved")
```

One `IncidentCapture` per non-resolved alert in an Alertmanager payload (`webhook.rs`). Five fields
are read and nothing else (four labels and `startsAt`):

| Label | Used as | Missing → |
|---|---|---|
| `alertname` | `spec.trigger.rule` | `"unknown"` |
| `namespace` | target namespace | **the controller's own namespace** |
| `pod` | target pod | **the alert is dropped and counted** (`reason="no-pod"`) |
| `startsAt` | firing timestamp | now |
| `startsAt`, present but unparseable | — | **the alert is dropped and counted** (`reason="bad-firing-ts"`) |
| `lapilli.dev/export: local` | skip remote export | remote export runs |

Identity is a deterministic hash of `{rule, cluster, target, minute-bucket}`, so a resend collapses
onto one capture and the same rule on two pods stays two. `target` is `namespace/pod`, or
`namespace/pod#local` when the alert carries `lapilli.dev/export: local` — the export mode is part
of the identity on purpose, so a forged "local" alert cannot claim the real alert's capture (409)
and thereby keep it off remote storage. The same alert with and without that label is therefore two
captures, not one.

A bad `startsAt` is refused rather than replaced, and that is a deliberate asymmetry with the missing
case. A *missing* `startsAt` asserts nothing, so the receive time is an honest stand-in. A *present
and unparseable* one is a claim, and substituting `now()` for it would put a firing time nobody
asserted inside a signed manifest — and, because the minute-bucket comes from it, would give every
resend of that alert a new identity and a new capture.

**There is no filter beyond that.** No severity, no alertname allowlist, no cap on captures per
payload. The implicit design is *"Alertmanager's routing does the filtering"*, which is defensible —
that is what routing is for — but it is written down nowhere, so an operator cannot tell the
contract from an omission. §5 keeps that open.

## 2. What was actually wrong

Both of these were found by running a storm against a live cluster, not by reading the code, and
neither was on the list of things this document was originally written to investigate.

### 2.1 The body limit nothing could see

`MAX_BODY` was 256 KiB, set on the belief that "Alertmanager payloads are a few KiB". A real
kube-prometheus-stack `KubePodCrashLooping` alert — labels, annotations, `generatorURL`,
`fingerprint`, timestamps — measures **1,059 bytes**. So the limit rejected a whole payload at about
**247 alerts**, rejected it *entirely* rather than the excess, and did it with a 413 produced by a
`tower` layer **outside** the handler. None of the four `webhook_requests_total` outcomes could
observe it.

A 250-node zone failure therefore produced **zero captures and zero telemetry**. The recorder was
blind at exactly the scale that matters most, silently.

The limit is now 1 MiB, and the limit is not the fix — the counting is. Exceeding it increments
`lapilli_payloads_dropped_total{reason="too-large"}` and fires `LapilliPayloadRefused`.

That series counts **payloads, not alerts**, and it is deliberately separate from
`lapilli_alerts_dropped_total` for a reason worth stating: a refused body is never parsed, so the
number of alerts inside it is not knowable. Putting the count in the alert series would have made
the harness's `captured + dropped = sent` identity quietly false for exactly the cliff it was added
to close. That identity covers what the handler sees — a cap, a filter, a future refusal — and it
does **not** cover a payload refused before parsing, or alerts that never arrive because readiness
took the pod out of the Service. Those need the two counters and the alert rules, not the identity.

Raising the limit also made things worse before it made them better: 1 MiB admits about 990 alerts,
and §3.1 shows cost rising with storm size well before that. §4's cap is the other half of this
change and should not be separated from it.

### 2.2 A signed bundle with nothing in it

An alert with no `pod` label fell through with `pod = ""` and `namespace` = the controller's own.
The capture reached `Exported`, incremented `captures_total{result="sealed"}`, and produced a signed
1.7 KB bundle whose `events.json` and `timeline.json` were both `[]`. `lapilli verify` called it
`PARTIAL coverage=40%`, which is not nothing — but the capture was reported as a success by every
signal an operator watches.

`KubeNodeNotReady` produces exactly this, and node- and cluster-level rules are the common case for
a missing `pod` label. The webhook now drops and counts those (`reason="no-pod"`, alerting via
`LapilliAlertsWithoutPod`), and `refuse_capture` refuses the same shape as a belt, since a capture
can also be created by hand.

**Say the consequence plainly: Lapilli now records nothing at all for node- and cluster-level
alerts.** An empty signed bundle was worse than nothing, so this is an improvement, but "drop and
count" is a stopgap and not a resolution — node-scoped capture is simply unimplemented. §5 carries
it as an open question rather than leaving it as a fix that reads like completion.

### 2.3 The liveness probe

The chart set no `timeoutSeconds` on either probe, so both inherited Kubernetes' default of
**1 second**. During a 20-alert storm, `/healthz` — a handler that returns a constant — took up to
**1.7 seconds**. Across five different controller pods the kubelet recorded `Unhealthy`, and once
escalated to a liveness kill that took the in-flight captures with it.

**The cause is host CPU scheduling.** `/proc/<pid>/schedstat` on the node showed 355 ms of CPU time
against **1,439 ms of cumulative runqueue wait** — the controller waited to be scheduled four times
longer than it ran — with the node's load average at **92 on 8 cores**. Three cheaper explanations
were measured and eliminated first:

| Suspected | Eliminated by |
|---|---|
| cgroup memory reclaim | the same stall with the limit raised to 1 GiB and the peak at 11.6% of it |
| the process's own CPU | 8 runtime workers, at most 3 ever in state `R` across 388 samples |
| blocking disk I/O | no thread in uninterruptible sleep in any of those 388 samples |

Two honest qualifications. First, an earlier draft of this section called the cluster "idle": it is
not, and cannot be — the harness by construction puts N continuously crash-looping pods on the same
single node, which is what drove the load to 92. A node that has simply *died* does not generate
that. Second, the three eliminations above were all sampled **inside** the container at about
3.5 Hz, which is blind to host-level runqueue wait; it took the schedstat probe, from outside, to
find the answer. The table is a record of what was excluded, not of how the answer was reached.

The probes now use timeout 5s / threshold 6 (liveness) and 3s / 6 (readiness). The asymmetry sets
the numbers: a false kill loses the incident the product exists to record, while a slow kill delays
recovery from a genuinely wedged controller by about two minutes. Readiness is treated the same way
for a reason that is easy to miss — failing readiness removes the pod from the webhook Service, so
Alertmanager has nowhere to deliver the rest of the storm.

**This trade has a cost, and it is not symmetric in the way the paragraph above implies.**
`/healthz` returns a constant, so it cannot fail for a controller whose reconcile loop has stopped;
relaxing the timeout buys tolerance for a busy node and buys nothing at all against a wedge, while
removing the only thing that was accidentally catching one. Nothing else watches for a stopped
loop. A liveness signal derived from the age of the last reconcile tick is the fix, and it is not
built — §5 carries it.

The harness now asserts on the kubelet's own `Unhealthy` events rather than on the restarts they
sometimes produce. `restarts = 0` passed every run in which the kubelet was already calling the
controller unhealthy, which is exactly how this stayed invisible.

## 3. What a storm costs — measured

Two experiments, both with `test/e2e/storm.sh`, payloads of realistic 1,070-byte alerts against a
Deployment whose containers all exit.

### 3.1 How cost grows with the size of the storm

The question an operator actually has. Concurrency fixed at the default of 2, a fresh pod per row:

| alerts | payload | **peak** | % of 256 MiB | CPU | finished |
|---|---|---|---|---|---|
| 20 | 20.9 KiB | 124.0 MiB | 48.4% | 1.58s | 20/20 |
| 50 | 52.3 KiB | 146.8 MiB | 57.4% | 11.82s | 50/50 |
| 100 | 104 KiB | — | — | — | **did not complete** |

The 100 row is not a controller failure and is not reported as one: the client got `EAGAIN` posting
the payload through `kubectl exec` while the node sat at load 92, so the run says nothing about
what the controller would have done. It is left in because the absence is the point — **nobody has
measured a completed storm above 50 alerts.**

From two points the growth is about **0.76 MiB per alert** over a ~110 MiB floor. Two points do not
establish a slope, and the peaks below carry a baseline this run did not subtract (see *What these
numbers are not*). But the direction is not in doubt, and the body limit admits about 990 alerts:
whatever the true curve, the configuration permitted payloads far outside anything measured. That
is what §4's cap exists for, and the cap's default is this table's largest completed row.

### 3.2 What the concurrency bound buys

One payload of 20 alerts, a fresh pod per row:

| `reconcileConcurrency` | **peak** | % of 256 MiB | CPU | finished |
|---|---|---|---|---|
| 1 | 124.0 MiB | 48.4% | 2.18s | 20/20 |
| **2** (default) | 121.3 MiB | 47.4% | 3.19s | 20/20 |
| 8 | 162.0 MiB | 63.3% | 8.32s | 20/20 |
| 0 (kube-rs default, unbounded) | 183.8 MiB | 71.8% | 5.56s | 20/20 |

Reading it, carefully:

- **Only the ends carry weight.** Unbounded peaked about 60 MiB higher than a bound of 2. That is
  the case for the default — not the exact 121.3.
- **Adjacent rows are not distinguishable.** Concurrency 1 (124.0 MiB) came out *above* concurrency
  2 (121.3 MiB), which is a noise floor larger than the difference being read off it. Every row is
  one run.
- **CPU does not rise monotonically** (2.18 → 3.19 → 8.32 → 5.56): bounded values used 2–3 CPU
  seconds and the unbounded pair used 6–8, but 8 landing above unbounded is unexplained. An earlier
  draft presented this as a clean trend by quoting the two endpoints that made one.
- **The concurrency-8 run is not trusted.** Its wall clock was three times its neighbours'. Its CPU
  figure comes from the same run and is not quoted as the end of a trend either — a run cannot be
  discarded in one column and kept as evidence in another.

Wall clock is not in either table. The same 20-alert storm has measured anywhere from **1 second to
200 seconds**, and the spread tracks whether the metrics collector had anything to fetch — a
property of the cluster's Prometheus and the alert's age, not of any setting here. Bundle count on
the volume made no difference (448 versus 0). Earlier drafts quoted 10s versus 13s as though the
difference meant something; the harness's completion loop had a seven-to-eight second quantum at
the time, so those were one to two iterations of the same instrument. The loop is now a one-second
poll and the output says `+-1s`.

### 3.3 What accumulated captures cost — the one that has a date on it

> *Update (2026-09-25):* the memory half of this section is superseded by
> `docs/design-capture-retirement.md` (`77ae8ce`, round 22). The controller now labels a finished
> capture `lapilli.dev/retired=true` and watches with `!lapilli.dev/retired`, so the watch cache
> holds only live captures and the clock below no longer runs against controller memory. The
> etcd half remains true: nothing deletes an `IncidentCapture`, and every one ever made still
> accumulates there. The measurement is kept as measured.

Nothing ever deletes an `IncidentCapture`. Retention reclaims *bundles*; the `Policy.reclaim_orphans`
doc comment in `retention.rs` says it
in its own words — "nothing in this controller ever deletes one" — and there is no `ownerReference`,
no finalizer and no TTL, so Kubernetes' garbage collector has no handle either. Every capture ever
made stays in etcd and in the controller's watch cache.

Measured on a fresh pod, probe captures refused immediately so they cost nothing to *run*:

| captures held | controller memory |
|---|---|
| 0 | 5.3 MiB |
| 500 | 15.3 MiB |
| 1,000 | 19.9 MiB |
| 1,500 | 38.6 MiB |
| 2,000 | 43.1 MiB |

**19.4 KB per capture.** Deleting all 2,000 took memory from 46.2 MiB back to 29.0 MiB, so the cache
is genuinely holding them — this is not a leak — though the allocator keeps an arena and the floor
ratchets up. And 19.4 KB is a *floor* in the other direction too: these probe objects carry no
status, no `exports` map and almost no `managedFields`, all of which a real capture accumulates.

Against the chart's 256 MiB limit:

| | captures | at ~120/day (one rule, 20-pod Deployment) | at ~300/day (DaemonSet, 50 nodes) |
|---|---|---|---|
| can no longer absorb a 20-alert storm (~110 MiB) | ~7,400 | **62 days** | **25 days** |
| OOMs sitting idle | ~13,300 | 111 days | 44 days |

This is not a risk, it is a clock. A single alert rule takes the controller past the point where it
can survive its own storm in about two months, and nothing in the product notices or acts. The only
remedy today is `kubectl delete incidentcapture` by hand.

It is also the reason `retention.reclaimOrphans` must default off: "no live CR" is a statement about
whether a human ran that command, not about whether the evidence is still wanted.

An alert on the held population fires at 5,000, which is about a month of headroom at the single-rule
rate. That makes the clock visible; it does not stop it. The fix was open question 9. *As shipped:*
the rule is `LapilliWatchCacheFilling`, `lapilli_captures_watched > 5000` for 1h (`docs/metrics.md`),
on the watched population rather than `sum(lapilli_captures)`, which grows forever by design.

### What these numbers are not

They are kind on one laptop. The absolute values are a shape, not a budget.

**The peak is the kernel's `memory.peak`** — the high-water mark of `memory.current`, which includes
reclaimable page cache, so it is an upper bound on the anonymous working set rather than the working
set. It is used because it is the accounting the OOM killer is built on, and because sampling was
worse: the first version of these tables sampled `/metrics` in a loop that broke as soon as every
capture was terminal, *before* its `sleep`, while each sample cost over a second and the storm
lasted two. Every cell was n=1 and none was taken during the storm.

**The rows above still carry an unsubtracted baseline.** `memory.peak` cannot be reset on this
kernel (6.5; the write landed in 6.8), so a fresh pod bounds it — but the mark still includes
process start and the informer's first list of the cluster, neither of which is the storm. The
harness now reads the baseline before the storm and reports growth over it, and warns when the
baseline is ≥90% of the final peak; the tables above predate that and were not re-run.

Two harness defects invalidated earlier numbers outright, both caught only because the script prints
its inputs instead of trusting them: it generated `{"alerts": []}` — 14 bytes — because a command
cannot carry both a heredoc and a herestring; and it hardcoded a past `startsAt`, which put the
metrics window before the target namespace existed.

## 4. What is implemented

| Change | Default | Why that default |
|---|---|---|
| `reconcileConcurrency` (`LAPILLI_RECONCILE_CONCURRENCY`) | **2** | §3.2: unbounded peaked ~60 MiB higher. `0` restores the kube-rs default. |
| `webhook.maxCapturesPerPayload` (`LAPILLI_MAX_CAPTURES_PER_PAYLOAD`) | **50** | §3.1's largest completed row. Not a computed ceiling — the largest storm anyone has measured. `0` = unlimited. |
| `MAX_BODY` | 1 MiB | ~990 realistic alerts; a limit, not a capacity. |
| Probe timeouts | liveness 5s/6, readiness 3s/6 | §2.3. |

Alerts past the cap are counted in `lapilli_alerts_dropped_total{reason="payload-cap"}`, logged with
the knob that raises them, and returned in the webhook's `dropped` field, where the harness's
accounting identity checks them. A cap that lost evidence silently would be the failure this product
exists to remove, so the counting is the feature and the number is the parameter.

Three alert rules now watch the three ways evidence goes missing — `LapilliPayloadRefused`,
`LapilliPayloadCapped`, `LapilliAlertsWithoutPod`. None existed when the counters were added, which
made the counters a place for the problem to sit unread rather than a fix.

`test/e2e/storm.sh` is wired into `test/e2e/run.sh` as the `storm` suite, last, because its cleanup
deletes every capture in the namespace. It asserts: the kubelet never called the controller
unhealthy during the storm; `captured + dropped = sent`; every accepted capture reached a terminal
phase; and (pre-existing) zero restarts.

## 5. The decisions nobody has made

| Case | What happens now | The question |
|---|---|---|
| A node- or cluster-level alert | **Nothing is captured**, counted as `no-pod` | Lapilli records a pod's incident window and has no node-scoped capture at all. What *should* a node failure produce — node objects, kubelet events, the pods that were on it? |
| No `namespace` label | Targets **the controller's own namespace** | An alert that carries a pod label but no namespace captures from `lapilli-system`. Refuse, or target nothing? |
| A payload of 2,000 alerts | Refused whole: 2,000 × 1,059 B is over the 1 MiB body limit, so **zero** captures | The cap never engages here; the body limit fires first and the two bounds are not composed. Should the limit refuse, or should the handler read what fits and cap the rest? |
| 20 pods of one Deployment, one cause | **20 bundles** | Notification already groups these into one message and has a `GroupKey` to reuse. Twenty bundles is neither "one incident, one artifact" nor a deliberate per-pod choice — it is the shape that fell out. At 50 nodes it is 50 bundles. |
| `resolved` alerts | Ignored, and **not** counted in `dropped` | So the identity balances against the *firing* count, not the payload's. Deliberate, but it means the assertion is narrower than it reads. |
| Two firings 61s apart | Two captures (minute bucket) | Interacts with `group_interval` (5m) and `repeat_interval` (4h), which nothing documents. |
| A crash-looping pod's metrics | **Empty** | Measured: for a pod whose container is not running at scrape time, cAdvisor emits only `container=""` series (the pod slice and the container scope), and the selector at `metrics.rs:43` — `namespace="$namespace",pod="$pod",container!="",container!="POD"` — excludes both. The workload the product most needs to observe is the one whose metrics are reliably absent. `lapilli verify` now says so; the query is unchanged. |
| A wedged reconcile loop | **Nothing notices** | `/healthz` returns a constant, and §2.3 just widened the window in which a stall goes unremarked. |
| The controller's own node dies | Evidence for that incident is not recorded | One replica, `strategy: Recreate`, a ReadWriteOnce volume. The canonical trigger this product advertises can take out the recorder, and no Lapilli series can fire when Lapilli is what is gone. |
| An `IncidentCapture` that finished | **Kept forever in etcd, and it is now measured**; retired from the controller's watch since `77ae8ce` | See §3.3 and `docs/design-capture-retirement.md`. |

## Open questions for review

1. **Is the cap's default of 50 right, or is it just the largest thing measured?** It is honestly
   the latter. The way to move it is to measure 100 and 200 on hardware that is not a laptop.
2. **Should the body limit and the cap be composed?** Today a 2,000-alert payload is refused whole
   by the limit and never reaches the cap, so the counted-refusal design does not apply to the
   largest storms — the case it was written for.
3. **What should a node-level alert capture?** §5 row 1. This is the biggest functional hole.
4. **20 bundles or 1?** A product decision that is also a storage decision.
5. **Which of the metrics selector's two exclusions should be relaxed?** Dropping `container!=""`
   alone still excludes `container="POD"`, so the choice is narrower than "widen it": it is whether
   a capture should carry the pod-level cgroup series when the container's own are absent, and how
   a reader would tell the two apart.
6. **Should `/healthz` be able to fail?** A reconcile-tick age would make liveness mean something
   again; it would also introduce a way to kill a controller that is merely slow, which is the
   failure §2.3 just fixed.
7. **What does Lapilli cost the cluster?** Every number here measures what a storm costs Lapilli.
   Nothing measures what Lapilli costs the API server and Prometheus *during* the incident — which
   is the usual reason observability tooling gets uninstalled after a bad night.
8. **Can a 50-node storm fill the default 1 GiB volume?** Measured bundles are small — n=201 demo
   bundles of one thin workload, p50 6.2 KB, max 6.7 KB, which `charts/lapilli/values.yaml`
   (the `persistence.size` comment) calls what it is: *"Only the last of those is measured, and it
   is a floor — a real capture carries a real log window."* A median of *demo* bundles is a floor
   for real workloads, not a median of them — and retention is off by default.
9. **What reclaims a finished `IncidentCapture`?** *Resolved (2026-09-23, `77ae8ce`): the second
   candidate below shipped as `docs/design-capture-retirement.md`, with the predicate narrowed to
   `Exported` only (never `Failed`) and the failure mode named in the last paragraph — a retired
   capture that must be re-reconciled — handled by a `resourceVersion` precondition on the label
   patch and an un-retire command. The reasoning is kept as written.* Now measured (§3.3), and the answer is that a
   single alert rule ends the controller in about two months. Two candidate fixes, and they are not
   variations on each other:

   - **A TTL on terminal captures.** Bounds etcd and memory together, and is what most operators
     would expect. But it *deletes the record that a capture happened*, and once its bundle has
     been reclaimed the CR is the only thing left saying so. This project has refused that shape
     everywhere else: `reclaimOrphans` defaults off, `allowUnexported` must be set on purpose.
     Defaulting a TTL on would contradict its own posture about deleting evidence.
   - **Scope the watch instead of deleting anything.** The controller only needs *non-terminal*
     captures; a capture that reached `Exported` needs no further reconciliation. Label terminal
     captures and watch with a selector that excludes them, and the cache holds only live ones
     while every CR stays in etcd for the operator. This bounds the memory — the failure with a
     date on it — and bounds nothing in etcd.

   Recommendation: the second, because it fixes the measured failure without the project having to
   change its mind about deleting evidence, and because it composes with a TTL later rather than
   replacing it. It needs its own round: a capture that must be re-reconciled after being labelled
   terminal would become invisible to the controller, and that failure mode has not been explored.
