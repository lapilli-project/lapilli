# Measurement — what a workload-level target can actually seal

This is a **measurement record, not a proposal.** Rounds 31, 32 and 33 each wrote a coverage
proposal and each was returned to premise because nobody had measured the premise first
(`docs/design-review-round34.md`). Round 35 §5 then claimed that coverage is *"a product decision,
not a physical limit"* and attached a stop condition to it. This file is the measurement that claim
needs before any design is written.

Two decision rules were **fixed in writing before the results existed**, for the reason
`docs/demand-test.md` gives: a rule invented after the answers arrive is not a rule.

## 0. What `kube-prometheus-stack` actually alerts about

Rendered from chart **91.8.2** (`helm template … --set defaultRules.create=true`), 155 alert rules.
The label set a firing alert carries is not stated in a rule file, but each rule's `description`
annotation interpolates the labels its author expects (`Pod {{ $labels.namespace }}/{{ $labels.pod }}`),
which is a more reliable signal than inferring from PromQL. Classified that way:

| tier | count | what it is about |
|---|---|---|
| **critical** | 41 | node/exporter 13 · control plane 18 · **the monitoring stack's own pods 8** · PVC 2 |
| warning | 105 | |
| info / none | 9 | |

**Rules about a user's own workload: 23 of 155.** Their severity is the finding:

| severity | count |
|---|---|
| warning | 20 |
| info | 1 |
| **critical** | **2** — both `KubePersistentVolumeFillingUp` |

58 rules declare a workload-shaped label, and **35 of those are the monitoring stack describing
itself** (Prometheus, Alertmanager, kube-state-metrics).

**So "0 of 41 paging alerts" never measured Lapilli.** It measured this chart's tier assignment:
the critical tier is infrastructure and self-monitoring, and a user's application is not in it. No
recorder of pod windows could score better there, because there is no user workload to record.
Three rounds quoted that number against this project without decomposing it.

What follows is that the user-workload tier is **`warning`**, with `for:` mostly 15m (15 of 19
sampled; `KubeContainerWaiting` 1h, `KubeDaemonSetNotScheduled` 10m, `KubeJobNotCompleted` none).

## 1. Measurement A — can a declared target reach the evidence?

**Question.** For a workload-level alert, at alert time, can the controller identify the pods that
hold the evidence, starting from the declared target alone (Deployment → ownerReferences →
ReplicaSet → Pod), and is that evidence there?

**Decision rule, fixed first.** A failure mode passes when (a) the owner chain reaches pods, (b) the
NotReady set is non-empty, and (c) something is present that **a log store does not have** — events,
termination state, or the object as it was. A current log alone is *not* a pass: that is Loki's.

kind 1.37, four failure modes all firing `KubeDeploymentReplicasMismatch`, sampled t+1m / t+5m /
t+15m, three replicas each. No Lapilli installed — plain `kubectl`, so this measures the API's offer
rather than this project's code.

| mode | owner chain | NotReady | evidence a log store does not have | verdict |
|---|---|---|---|---|
| `badimage` (ImagePullBackOff) | ✅ | 3/3 | **no container log at all** (`BadRequest`). Events carry it: `Failed: Failed to pull image "registry.invalid/nope:v9": failed to pull and unpack image` | **pass** |
| `crashloop` | ✅ | 3/3 | `--previous` present, **termination state `reason=Error exit=1 finishedAt=…`**, `BackOff` | **pass** |
| `notready` (readiness fails) | ✅ | 3/3 | current log is Loki's; no termination state; **event `Unhealthy`** is the readiness failure | **pass** |
| `unsched` | ✅ | 3/3 | **no log, no termination state.** Event `FailedScheduling: 0/1 nodes are available: 1 Insufficient cpu, 1 Insufficient memory` is the whole explanation | **pass** |

**4 of 4.** And the shape of the result matters more than the count:

**In three of the four modes the decisive evidence is an event, not a log.** `badimage` and `unsched`
never ran a container, so no log exists anywhere — and the entire explanation of the incident is one
event message. That is the residue round 35 conceded (events, 1 h TTL, absent from a log store),
and at workload level it is not a residue but the whole answer. The assumption that logs are the
headline was wrong.

**The old ReplicaSet keeps the object as it was.** For a stuck rollout both revisions are live:

```
rollout-6dd496f4b8   rev 1   WARMUP=lazy    ← as it was
rollout-7798576894   rev 2   WARMUP=eager   ← now
```

Recorded as a pass but **not as a differentiator**, decided before the run: Kubernetes already
preserves this, which is round 29's objection about the audit log in another form.

**`--previous` was readable on 3 of 3 pods at every sample**, where round 35 measured 7 of 10. The
two are not in conflict and neither replaces the other: round 35 sampled **one** pod repeatedly,
this sampled the **union over three replicas**, and a workload target only needs one readable
instance. Whether replica count actually lifts availability is **untested** — one scenario, three
samples, is not a distribution.

## 2. Measurement B — and when there is no pod to point at?

Measurement A's four modes all had NotReady 3/3, which is the easy case. B attacks the one condition
that breaks (b): a workload alert firing with no failing pod to name.

**Decision rule, fixed first.** Pass when the reason sits on the Deployment or its ReplicaSet, in
**events or `status.conditions`**, where "reason" means *a string an operator can read and know what
to fix*. `ScalingReplicaSet` ("I scaled it") is explicitly **not** a reason — it never says why the
scaling did not take. The log-store comparison was dropped for this half: with no pod there is no
log, so "absent from Loki" would be vacuous.

| mode | did the condition hold? | reason on the target | verdict |
|---|---|---|---|
| `quota` (ResourceQuota blocks creation) | ✅ 1/1 Ready, nothing to point at | `status.conditions`: `ReplicaFailure=True reason=FailedCreate` → *"exceeded quota: only-one-pod, requested: pods=1, used: pods=1"*; same on the RS event | **pass** |
| `paused` (template changed, rollout paused) | ✅ 2/2 Ready | `status.conditions`: `Progressing=Unknown reason=DeploymentPaused`. **No event carries it** | **pass**, and low value — a human did this deliberately; it is not an incident. Recorded before the run so it cannot be used to inflate a coverage number |
| `terming` (pod held in Terminating by a finalizer) | ❌ **condition did not hold** — NotReady 1, Terminating 1: there *was* a pod to point at | — | **out of scope** |

**2 of 2 modes that produced the condition passed → the reason is on the target.**

Three details that a design has to carry, and none of them was guessed:

1. **`status.conditions` is the first place the reason lives, ahead of events.** Events expire after
   an hour; a condition sits in the object. The next rollout overwrites the object, so sealing it
   still buys something.
2. **A Deployment's own events are nearly worthless.** In all three modes the only Deployment event
   was `ScalingReplicaSet`. The reason was on the **ReplicaSet's** events and in the Deployment's
   **conditions** — nowhere else. Without the rule fixed in advance, `ScalingReplicaSet` could have
   been counted as "events present, pass".
3. **`terming` broke its own premise and produced the more important finding.** Its conditions read
   `Progressing=True NewReplicaSetAvailable "has successfully progressed"` and
   `Available=True MinimumReplicasAvailable` — **the workload object says nothing is wrong** while a
   pod sits `phase=Failed` in Terminating. The evidence exists only on that pod.

So collection is **both/and**, not either/or: conditions alone miss `terming`, pods alone miss
`quota` and `paused`.

## 3. What this does and does not settle

**Settled.** A workload-level declared target can reach evidence that a log store does not hold, in
every failure mode measured, and when no pod can be named the reason is on the workload object.

That removes the **physical-limit** objection, and only that. Round 35 §7's stop condition is worded
on a different quantity — *"if §5's declared target does not move the coverage number against the
same 155 rules"* — and this measurement did not move any coverage number, because the declared target
does not exist yet. Reachability is a precondition for that test, not the test. Saying the stop
condition has been cleared would be the same substitution this project has already made twice:
asserting a proxy in place of the property (`docs/design-review-round34.md`).

**Not settled, and a design must not assume these:**

- **Whether anyone wants the resulting bundle.** `docs/demand-test.md` is **0 of 5**. This file
  measures that the evidence is reachable, not that it is wanted — and the warning-tier reframe in §0
  is a claim about *positioning*, which is exactly the class of claim round 35 found three of to be
  false. It has not been through a round.
- **Which pod is the *right* one.** Measurement A asked only whether the failing set is identifiable.
  With 3 of 3 failing the choice is trivial; a mixed set (1 of 3) was never run, and a bundle that
  seals one pod out of three has to say which and why.
- **Frequency.** How often a real cluster's `KubeDeploymentReplicasMismatch` arrives by each of these
  paths is unmeasured. The modes were constructed, not sampled from production.
- **Whether replica count lifts `--previous` availability** (§1's last paragraph).

## 4. How the measurements themselves failed, since that is also evidence

Three defects, all in the measuring code, none in what was measured:

- **`"$ev건"`** — bash read the Korean particle as part of the variable name, looked up `ev건`, and
  died under `set -u`. Writing shell with Korean suffixes attached to variables needs `${ev}건`; the
  English habit of a trailing space hides this.
- **A missing `chmod`**, which cost one run and nothing else.
- **A namespace-wide `ResourceQuota` with three experiments in one namespace.** The quota blocked
  `paused` and `terming` from creating pods, so all three modes collapsed into the `quota` case and
  all three reported `FailedCreate`. It looked like **3 of 3 pass**. Re-run with one namespace per
  mode, the real answer was 2 of 2 valid and one out of scope. The expensive defect was not code but
  **experiment design**, and it failed by producing a plausible result.

The last one is the reason this file states each decision rule before its table: a measurement that
can be read two ways will be read the convenient way.

## 5. Reproducing this

The scripts and the raw samples are in the repository rather than described, because a measurement
nobody can re-run is an assertion:

| file | what it is |
|---|---|
| `test/fixtures/workload-target/measure-failure-modes.sh` | measurement A — four failure modes, samples at t+1m/5m/15m |
| `test/fixtures/workload-target/measure-no-pod.sh` | measurement B — one namespace per mode, which is the fix for §4's contamination |
| `test/fixtures/workload-target/samples-*.txt` | the raw output both tables above were read from |

Each needs `kind`, `kubectl` and `jq`, creates and deletes its own cluster, and installs no Lapilli.
The rule inventory in §0 is `helm template prometheus-community/kube-prometheus-stack --version
91.8.2 --set defaultRules.create=true`, classified on each rule's `description` annotation.
