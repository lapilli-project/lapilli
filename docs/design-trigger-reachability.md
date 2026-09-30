# Design — what an alert-triggered recorder can reach, measured

Status: **measurement, and the boundary it establishes.** Written after three proposals
(`docs/design-review-round31.md`, `round32`, `round33`) tried to widen trigger coverage and each was
returned to premise. This one was measured first and written second, which is the order those three
rounds earned.

## 1. The question nobody had asked

Round 29 F4 said Lapilli refuses the alerts an SRE is paged for. Round 31 said the one rule where
evidence is *genuinely destroyed* is `KubeJobFailed`, and buried it among three where
`kubectl describe` answers in twenty seconds. Every round since has argued about which rules to
accept.

None of them asked whether the evidence is still there when the alert arrives.

## 2. Measured, on kind 1.37

A CronJob at `*/1`, every default except the schedule: `failedJobsHistoryLimit: 1`,
`backoffLimit: 0`, `restartPolicy: Never`. Its container prints one line and exits 1. The probe
tracks the first failed pod for sixteen minutes, because `KubeJobFailed` is `for: 15m`.

| t | the failed pod | its logs | objects in the namespace |
|---|---|---|---|
| +0 min | present | readable | jobs=1 pods=1 |
| +1 min | **gone** | **gone** | jobs=1 pods=1 |
| +2 … +14 min | gone | gone | jobs=1 pods=1 |

**The evidence is destroyed fourteen minutes before the alert that is supposed to capture it.**

### What the number actually depends on

The pod does not die on a timer. `failedJobsHistoryLimit: 1` keeps one failed Job, so the previous
failure is deleted **when the next failure is created** — one schedule interval later. The probe ran
at `*/1`, so one minute. The general statement is therefore:

> A failed CronJob pod survives until that CronJob's next run. With the default
> `failedJobsHistoryLimit: 1` and an alert at `for: 15m`, **every CronJob scheduled more often than
> every fifteen minutes destroys its own evidence before the alert fires.** An hourly CronJob does
> not, and Lapilli would capture it.

The interval dependence is read off the mechanism rather than measured separately; the `*/1` row is
the measurement.

### And a correction to round 33

Round 33 left a finding unproven: that `KubeJobFailed` might carry the *exporter's* pod label
through prometheus-operator's relabeling, and so be accepted with the wrong target. Measured here:
kube-state-metrics exposes

```
kube_job_failed{namespace="probe", job_name="failing-29845745", condition="true"} 1
```

— `namespace`, `job_name`, `condition`, and **no `pod` label at all** on any `kube_job_*` series.
Prometheus still attaches the scrape target's `pod` to those samples, so the alert is not necessarily
`pod`-less at Alertmanager; what is settled is that the label cannot come from kube-state-metrics
itself. The half that remains unproven is what an end-to-end install actually delivers, and it needs
a cluster with prometheus-operator on it.

## 3. The boundary this establishes

Lapilli is triggered by operational signals. An operational signal has a `for:` delay — fifteen
minutes on the standard rules — and everything that dies inside that window is unreachable **by
construction, not by omission**. No target shape, no collector and no measurement changes it.

So the honest statement of what Lapilli records is narrower than "the incident window", and it is
this:

> **Evidence that outlives the alert, and dies before the postmortem.**

That window is real and it is most of what matters. A CrashLoopBackOff pod is not deleted, so its
object survives; the kubelet keeps one dead instance, so `--previous` is there at alert time and
gone after the next GC or the next rollout. Events survive an hour. A ReplicaSet survives
`revisionHistoryLimit` rollouts. All of those are hours-to-days evidence that an alert at t+15m
reaches and a postmortem three days later does not.

What falls outside it is the fast-perishing class: a sub-15-minute CronJob's failed pod, a pod
deleted by a scale-down before the alert clears its `for:`, a node that goes away with its pods. For
those, an alert-triggered recorder is the wrong instrument, and saying so is worth more than a fourth
proposal.

## 4. What this does not close

**Round 16's candidate 3 is still open, and this measurement is new evidence for it.** That round
returned a Warning-event trigger to premise but its verdict pointed somewhere else: *"the complete
signal is not the event stream but Pod status: a `restartCount` increment with
`lastState.terminated.reason` present. The controller already watches pods for other reasons and
already reads exactly those fields."* A watcher sees the pod's terminal state before deletion, which
is precisely the evidence an alert at t+15m cannot reach.

It is a reversal of `DESIGN.md` §8.3 (*"read at capture time, not a watcher"*), the controller
watches only `IncidentCapture` today, and round 16 measured what it would cost — 40 crashloopers
produced 12 captures in 200 ms and a projected ~1700 bundles a day with the channel silent. That is
an owner's fork, not a next step, and it is recorded in `ROADMAP.md` rather than started here.
