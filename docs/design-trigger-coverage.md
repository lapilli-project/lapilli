# Design — trigger coverage: the alerts an SRE routes carry no `pod` label

Status: **RETURNED TO PREMISE (round 31).** Kept as written, because the record of a refuted
proposal is worth more than its absence — but **nothing below is the plan**. Nine findings survived
at BLOCKER; three attack the premise. Read `docs/design-review-round31.md` first, then this document
as the thing it refutes.

What round 31 left standing: the **hole** is real and round 29 F4 was not refuted. What it killed:
option D, this document's answer to it. Briefly — D rejects option B for an emptiness it inherits
itself and never bounds; "one incident, one artifact" is a phrase invented inside an open question
and cited back as doctrine, reversing `docs/design-notify.md`'s settled "one capture per pod is
right"; and every rule D accepts is upstream `severity: warning`, `for: 15m`, so the cut was chosen by
which collector already existed rather than by where evidence perishes. The measured costs — an
omitted `pod` making a correct bundle FAILED exit 1 on every released verifier, 800 MiB of log bytes
per DaemonSet capture against a 256 MiB limit, a dedup collapse counted as a resend — are in the round
log with their reproductions.

The replacement is a fork, with the owner: measure which rules hold non-reproducible evidence before
choosing any cut, or re-scope to the cases where evidence is destroyed today, or pay D's full bill.

Originally: target of the first v0.2 item in `ROADMAP.md` §4, raised by round 29 F4 and left open as
question 3 of `docs/design-trigger-and-load.md` §5.

## 1. The hole, measured rather than asserted

`webhook.rs` refuses any alert whose `pod` label is absent or empty, counts it as
`lapilli_alerts_dropped_total{reason="no-pod"}`, and logs one line. That refusal is correct for the
code as it stands, and round 16 is why: forcing a capture without a pod produced a signed 1.7 KB
bundle whose `events.json` and `timeline.json` were both `[]` and whose every PromQL result was
empty — *a success report for nothing*, measured on kind with a real `NodeNotReady`.

The problem is what the refusal covers. Here is the standard kube-prometheus-stack rule set, by
whether Lapilli can accept it:

| Rule | Labels it carries | Lapilli today |
|---|---|---|
| `KubePodCrashLooping`, `KubePodNotReady`, `KubeContainerWaiting` | `namespace`, `pod`, `container` | **captures** |
| `KubeDeploymentReplicasMismatch`, `KubeDeploymentGenerationMismatch` | `namespace`, `deployment` | dropped |
| `KubeStatefulSetReplicasMismatch`, `KubeStatefulSetGenerationMismatch` | `namespace`, `statefulset` | dropped |
| `KubeDaemonSetRolloutStuck`, `KubeDaemonSetNotScheduled` | `namespace`, `daemonset` | dropped |
| `KubeJobFailed`, `KubeJobNotCompleted` | `namespace`, `job_name` | dropped |
| `KubeHpaMaxedOut`, `KubeHpaReplicasMismatch` | `namespace`, `horizontalpodautoscaler` | dropped |
| `KubeNodeNotReady`, `KubeNodeUnreachable`, `KubeletTooManyPods` | `node` | dropped |
| `KubePersistentVolumeFillingUp` | `namespace`, `persistentvolumeclaim` | dropped |
| `KubeQuotaExceeded` | `namespace`, `resourcequota` | dropped |
| `TargetDown` | `job`, `namespace` | dropped |
| An SLO burn-rate rule (multi-window) | whatever the team's recording rules carry — usually `service` or `job`, rarely `pod` | dropped |

So Lapilli fires on exactly the class — crash loop, OOM, not-ready — that `kubectl logs --previous`
and a git diff mostly answer already, and refuses the class an SRE is paged for. Round 29 F4 put it
as bluntly as it deserves: this, not silence between incidents, is the adoption killer.

**This is not the identity's fault.** The identity sentence says Lapilli *correlates
Kubernetes-native state across the incident window*. A Deployment, a DaemonSet, a Node and a Job
are Kubernetes-native state. What is pod-shaped is not the product; it is `TargetRef`:

```rust
pub struct TargetRef {
    pub namespace: String,
    /// Pod name to collect logs from (v0.1 collects previous-container tails from this pod).
    pub pod: String,
    pub container: Option<String>,
}
```

Every collector reads that: logs by pod, resources by pod plus its owner chain, events by
`involvedObject` on the pod and its owners, diffs from the owner's revision history, metrics with
`pod="$pod"` in the selector.

## 2. What was considered, and what is proposed

**A · Generalise the target to an object reference.** `{namespace?, kind, name, container?}` with
per-kind collectors. Covers every row above. It is also a change to `incident.target` in a **frozen**
format, and a large one to make in one step.

**B · Resolve the alert down to pods and keep everything pod-scoped.** For
`KubeDeploymentReplicasMismatch`, list the Deployment's pods and capture those. Rejected on its own
headline case: a replicas mismatch is frequently an *absence* of pods, and a node failure's pods are
gone or unreachable. Resolving to pods yields the empty bundle round 16 already refused, with extra
machinery in front of it.

**C · Keep refusing, document a mapping, make the metric actionable.** Rejected as the answer,
though part of it is kept: asking a team to rewrite the alert rules their paging depends on so a
recorder can read them inverts the relationship, and the rules above are *upstream defaults*, not
local choices.

**D · Proposed: A's shape, narrowed to namespaced workloads first.** Support `Deployment`,
`StatefulSet`, `DaemonSet` and `Job` in v0.2; leave `Node`, `HPA`, `PVC`, `ResourceQuota` and
label-only SLO rules to a later step, behind the same target shape so they are additive rather than
a second rewrite.

Why that cut. The owner-chain collector already walks Pod → ReplicaSet → Deployment; a workload
capture walks the same chain from the other end, so the *collection* story is one we have measured.
A node capture raises questions a workload capture does not — which pods, how many, whose logs, what
a bundle should say when the node holding the recorder's own volume is the thing that failed — and
each of those deserves the loop's attention rather than a decision taken in passing.

## 3. What a workload-scoped capture contains

| Collector | Pod-scoped capture (today) | Workload-scoped capture (proposed) |
|---|---|---|
| `resources/` | the pod, its ReplicaSet, its Deployment | the workload, its current and previous ReplicaSet, and **every pod it owns**, up to a bound |
| `logs/` | current and previous tails of the pod's containers | tails from the pods that are **not Ready**, newest first, up to a bound; none when every pod is Ready, which is itself the evidence |
| `events.json` | events on the pod and its owners | events on the workload, its ReplicaSets and its pods |
| `diffs/` | the owner's rollout diff | unchanged — this collector was always workload-shaped |
| `metrics/` | `pod="$pod"` | the workload's pods by `owner` relabelling, or the sum, with the selector recorded |
| `changes.json` | the owner's `managedFields` actors | unchanged |

Two things this settles that §5 left open. A workload capture is **one incident, one artifact** for
the 20-pods-of-one-Deployment row, instead of the 20 bundles that fell out of a pod-scoped trigger.
And the "no `namespace` label" row stops mattering for these rules, because every one of them
carries a namespace; what has no namespace is the cluster-scoped set, which this step defers.

## 4. What must not be decided in passing

1. **The frozen format.** `incident.target` is `{namespace, pod}` in `lapilli.dev/ieb/v1`, and
   `spec/IEB-SPEC.md` says it exists so a lookup never has to unpack a bundle. Options: an additive
   optional `target.object = {kind, name}` with `pod` kept for pod captures; or `ieb/v2`.
   `docs/COMPATIBILITY.md` governs, and the answer has to survive a verifier that knows only v1.
2. **The bounds.** "Every pod it owns" is a DaemonSet on 500 nodes. Both the object count and the
   log bytes need a stated, measured ceiling, and the memory envelope in
   `docs/design-trigger-and-load.md` §3 was measured for one pod's logs.
3. **The dedup identity.** It is a hash of `{rule, cluster, target, minute-bucket}`. If the target
   becomes a workload, twenty pod alerts for one Deployment collapse into one capture — wanted — but
   so do two genuinely different pod incidents in the same minute on the same workload. That trade
   needs stating, not discovering.
4. **Which pods' logs.** "Not Ready, newest first" is a guess. A capture that takes the wrong three
   pods out of fifty is worse than one that says it took none.
5. **The two metrics round 29 asked for**, which are how this is judged rather than believed: the
   share of a team's firing rules the webhook accepts, and bundles opened ÷ bundles sealed.

## 5. What this proposal is not

Not a new trigger source: still Alertmanager, still an operational signal, still no polling and no
querying of a vendor's store. Round 16 returned an event-driven trigger to premise and round 24
returned the backfill; neither is reopened here.
