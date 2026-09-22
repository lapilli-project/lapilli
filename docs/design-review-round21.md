# Design review — round 21: what triggers a capture, and what a storm costs

Constitution: Loop Engineering v0.5.0. Tier: **full** (a public-repo design document making
measurement claims that defaults are now set from).

Snapshot attacked: `docs/design-trigger-and-load.md` as written on top of `cd7b562`, together with
`test/e2e/storm.sh`, `webhook.rs`, `telemetry.rs`, `charts/lapilli/values.yaml` and
`charts/lapilli/templates/deployment.yaml` at that commit.

Lenses, five, in parallel, one agent each: **measurement validity** · **claims vs evidence** ·
**doc-vs-code drift** · **adoption (an SRE at 200 nodes looking for a reason to say no)** ·
**devil's advocate on the new framing**. Each was told a clean report was legal and that a
fabricated objection was worse than one.

Independence, honestly: all five are the same model family. That calibrates; it does not achieve
independence. Correlated blind spots are not covered by this round and an outside reviewer is still
a release gate.

## Result

**25 findings. Every one I put through a steelman test survived it.** Not one was refuted, which is
itself a signal about the snapshot rather than about the critics: the document was written fast,
immediately after its framing had been overturned once, and it overclaimed in the predictable
direction.

Round verdict: **not dry.** Applied below; §5 and the open questions grew rather than shrank, which
is the honest outcome.

### The findings that changed the product, not just the prose

**1 — The fix I shipped authorised an OOM. (BLOCKER, devil's advocate + measurement validity)**

Raising `MAX_BODY` from 256 KiB to 1 MiB admitted payloads of about 990 alerts. Every measurement
in the document stopped at 20. Measured during the round: 20 alerts → 124.0 MiB peak, 50 alerts →
146.8 MiB, against a 256 MiB limit. Cost grows with storm size and nothing bounded it.

So the change that closed a silent-loss cliff opened an unmeasured one in the other direction. The
critic's objection — "the document declares memory a non-issue at 2% of the ingest its own change
authorised" — is exactly right.

APPLIED: `webhook.maxCapturesPerPayload`, default **50** — the largest storm measured end to end,
not a computed ceiling. Excess alerts are counted (`reason="payload-cap"`), logged with the knob
that raises them, and returned in the webhook's `dropped` field so the harness's accounting identity
checks them. Open question 2 is answered by measurement rather than left open.

**2 — `payload-too-large` was counted in the wrong unit. (BLOCKER, measurement validity)**

`lapilli_alerts_dropped_total{reason="payload-too-large"}` incremented once per refused *payload*,
not per alert — the body is never parsed, so the alert count is unknowable. The document then
claimed the identity `captured + dropped = sent` had "no place for a cliff of any cause to hide",
which is arithmetically false for the one cliff it was added to close: `0 + 1 ≠ 2000`.

This is the same unit-mixing defect I had just finished documenting in
`lapilli_webhook_requests_total`, reproduced in the series I added to fix it.

APPLIED: split into `lapilli_payloads_dropped_total{reason="too-large"}`, with both series' help
text naming their unit, and the document now states what the identity does and does not cover.

**3 — The counters nobody watched. (MAJOR, adoption)**

Both silent-loss fixes landed in a counter that no shipped alert rule referenced. "The same
blindness with a different shape."

APPLIED: `LapilliPayloadRefused` (critical), `LapilliPayloadCapped` (warning),
`LapilliAlertsWithoutPod` (warning). `scripts/alert-rules-check.sh`: 24 rules, valid.

**4 — The cause of the probe stall, found by a probe a critic named. (MAJOR → resolved)**

§2.3 shipped a relaxed liveness timeout over an explicitly unexplained 1.7 s stall. Two critics
objected to changing a shipped default on an unknown cause, and both pointed at the same missing
measurement: host-level scheduling delay, invisible to everything sampled inside the container.

Measured: `/proc/<pid>/schedstat` showed **355 ms of CPU time against 1,439 ms of cumulative
runqueue wait**, with the node's load average at **92 on 8 cores**. The controller waited to be
scheduled four times longer than it ran. The three causes the document had "ruled out" were all
sampled inside the container at ~3.5 Hz and were structurally blind to this.

APPLIED: the cause is now stated in the document and in the chart comment; the claim that the
cluster was "idle" is withdrawn (the harness puts N crash-looping pods on that node by
construction, which is what drove the load); and the trade the document argued one side of is now
argued on both — `/healthz` returns a constant, so relaxing the timeout buys tolerance for a busy
node and **nothing** against a wedged reconcile loop, while removing the only thing accidentally
catching one. That is open question 6, not a solved problem.

**5 — The measurement instruments were lying in two ways. (BLOCKER + MAJOR, measurement validity)**

- `memory.peak` was never read *before* the storm. Every "peak" was the high-water mark since pod
  start, including the informer's first list of the cluster. The proof it mattered: the
  concurrency-1 row (124.0 MiB) came out **above** the concurrency-2 row (121.3 MiB), which is a
  noise floor larger than the 62 MiB difference being read off it.
- The "wall clock" column measured iterations of a completion loop whose quantum was seven to eight
  seconds — a `scrape()` the script itself prices at "over a second", plus a `kubectl get`, plus
  `sleep 5`. 10 s, 12 s and 13 s were one to two iterations of the same instrument, and "bounding
  to 2 costs no wall clock" was read off a ruler with no such gradations. Taken literally the table
  said bounding *cost* 3 seconds.

APPLIED: the harness reads a baseline peak and reports growth over it, warns when the baseline is
≥90% of the final peak, and the completion loop is a one-second poll with `+-1s` printed. Wall
clock is removed from both tables in the document, because §3's own spread (1 s to 200 s for the
same storm, tracking the metrics collector's work) makes it a property of the cluster rather than of
any setting here.

### The findings that were drift

Confirmed against source, all mine:

- **A `container` label row I invented.** §1 claimed the webhook reads `container` and targets that
  container. It hardcodes `container: None` and never reads the label — which `storm.sh` sends and
  which is silently discarded. Removed; "six labels" → five.
- **The identity tuple was incomplete.** `lapilli.dev/export: local` forks the target to
  `namespace/pod#local`, deliberately, so a forged "local" alert cannot claim the real capture and
  keep it off remote storage. The document dropped that entirely while asserting "a resend collapses
  onto one capture".
- **The discredited n=1 table lived in three places** — the design doc, `values.yaml`, and a doc
  comment in `main.rs` — with a fourth row (`4 → 157.2 MiB`) the doc had already abandoned. Three
  contradictory measurements of one experiment in one repo means none is citable. All replaced with
  §3.2's numbers *and its caveats*, and `values.yaml` now says the old table is gone rather than
  corrected.
- **The metrics selector is `container!="",container!="POD"` in `metrics.rs:43`**, not
  `container!=""`, so open question 5's premise was wrong: relaxing the first exclusion does not pull
  in the pod-level series. Restated as "which of the two exclusions".
- **Numbers disagreed across three files** for what was presented as one measurement (16 CPU s /
  112 s / 11.6% versus 18 / 121 / 16%). An argument whose whole weight is "measured, not argued"
  cannot ship two versions of the measurement.

### Valid, and logged as gaps rather than fixed

- **The recorder's own node can be the one that dies.** One replica, `strategy: Recreate`, a
  ReadWriteOnce volume — so the canonical trigger the product advertises can take out the recorder,
  and no Lapilli series can fire when Lapilli is what is gone. Now in §5. HA is a dependency, not
  this round's fix.
- **What Lapilli costs the cluster is unmeasured.** Every number here is what a storm costs
  Lapilli; nothing measures what Lapilli costs the API server and Prometheus *during* the incident,
  which is the usual reason observability tooling gets uninstalled after a bad night. Open question
  7. The critic is right that this may outrank everything above, and it was ranked last only because
  it was the thing not yet looked at.
- **A node-level alert now records nothing at all.** Dropping the pod-less capture was an
  improvement over a signed bundle with nothing in it, but it is a stopgap and the document read
  like a resolution. Now stated plainly and carried as open question 3, the biggest functional hole.
- **The body limit and the cap are not composed.** A 2,000-alert payload is refused whole by the
  limit and never reaches the counted cap — so the counted-refusal design does not apply to the
  largest storms, the case it was written for. Open question 2.
- **The 100-alert row did not complete.** The client got `EAGAIN` through `kubectl exec` while the
  node sat at load 92, so it says nothing about the controller. Left in the table as an absence,
  because "nobody has measured a completed storm above 50" is the fact that sets the cap.

### Nothing was overridden

No finding was accepted-with-risk. Two things were re-scoped to dependencies (HA topology,
cluster-side cost) and are logged above; the rest were applied.

## Carried forward

1. The §3 tables were **not re-run** after the harness's baseline and poll-quantum fixes. They are
   labelled as carrying an unsubtracted baseline. Re-running them is the first item of round 22, and
   it is now one command each: `test/e2e/storm-sweep.sh size 20 50 100` and
   `test/e2e/storm-sweep.sh conc 0 1 2 8`. That tool is new this round — the measurements it
   automates had lived in throwaway scripts, which is part of why three contradictory copies of one
   table ended up in the repo. Run it with `REPEATS=3`: every published row so far is n=1.
2. 100 and 200 alerts on hardware that is not a laptop.
3. §2.3's probe latencies are quoted from one-off node probes, not produced by the harness. A
   `LapilliProbeLatency`-style measurement belongs in `storm.sh` or the claim stays unreproducible.
4. Open questions 3 (node-level capture), 6 (a `/healthz` that can fail) and 7 (cost to the
   cluster) are all larger than anything this round fixed.

## After the round: the largest thing it pointed at

Open question 9 asked what reclaims a finished capture. Measuring it turned it into the most
serious finding of the round, and it was reached by following the round's own gaps rather than by
another critic.

**Nothing reclaims a capture, and it costs 19.4 KB of controller memory to hold one** (§3.3). At the
corrected capture rate, a single alert rule takes the controller past the point where it can survive
its own 20-alert storm in **about two months**, and to an idle OOM in under four. Not probabilistic
— a clock, with nothing in the product watching it.

Shipped: `LapilliCapturesAccumulating`, so the clock is at least visible. Not shipped: the fix,
because the two candidates differ on something this project has a position about — whether to delete
the record that a capture happened. §5's open question 9 carries both and recommends the one that
deletes nothing.

**The measurement of it failed first, in this round's own signature way.** The first run reported a
flat line — 4.6 MiB at 0 captures, 4.7 MiB at 2,000 — which reads as "accumulation is free". Every
`kubectl apply` had been rejected for a missing `spec.profile`, and the script sent those errors to
`/dev/null`. Zero captures existed. The corrected script refuses to print a row when the count does
not reach the target, which is the guard the first one lacked.

## What this round says about the method

The previous round's lesson was *generation and verification that share a blind spot agree with each
other*. This round adds its sibling: **a framing corrected once is corrected too far.** The document
was rewritten immediately after "memory is the risk" was disproven, and it arrived at "memory was
never the problem" — a universal drawn from one payload size, while the change shipped in the same
breath quadrupled the payload size the code would accept. The devil's-advocate lens existed to catch
exactly that, and it did.

Second: **most of the highest-value findings were about instruments, not about the system.** The
baseline-free peak, the poll-quantum wall clock, the in-container-only cause elimination, a latency
sampler whose value was never printed, and a probe script that measured zero objects and reported a
flat line — all cases of measuring confidently with a tool that could not see the thing being
claimed. That is the same failure as a guard that cannot fail, moved from the test suite into the
measurement, and it kept happening *after* the round had named it. Naming a failure mode does not
inoculate you against it; the only thing that caught each one was printing the instrument's own
inputs and reading them.
