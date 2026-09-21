# Design — triggering a capture without an alert rule

Status: **returned to its premise by review** (`docs/design-review-round16.md`). The goal survives,
the first design did not. What is below is what was measured, and what a redesign has to start from.
Roadmap: `DESIGN.md` §11, v0.2, "Warning-event trigger (best-effort)".

## The goal

`DESIGN.md` §10.2: *"Trigger on everyday failures (CrashLoopBackOff, OOMKill, failed rollout), so the
evidence folder fills up weekly, not once a year."*

Today a capture starts only when Alertmanager POSTs to the webhook, so the incident had to be one
somebody anticipated well enough to write a `PrometheusRule` for. Everything Kubernetes complains
about that nobody wrote a rule for goes unrecorded.

**This is not "the largest adoption cost in the product"** — an earlier draft of this document claimed
that, and it is contradicted by `kairn demo`, which fires its own alert from inside the controller pod
and shows value in five minutes with no Prometheus at all. §10's own title names the real barrier as
the *deferred-value* trap. The argument for this feature is §10.2's and only §10.2's: without it the
evidence folder fills up once a year.

**It reverses a stated design choice, and that has to be said out loud.** `DESIGN.md` §8.3 specifies
"events snapshot (**read at capture time, not a watcher**)". A trigger that reacts to cluster state is
this product's first always-on watch. §11 records the same discipline being held elsewhere — the
change-diff shipped with "no always-on recorder". Reversing it needs an argument, not a footnote.

## What Kubernetes actually emits (measured, 1.30.0 and 1.37.0)

These are the facts a redesign rests on. All were measured on purpose-built kind clusters during
round 16; none is inferred.

**`BackOff` is real, stable, and identical across both tested versions.** A crashlooping container
produces `reason: BackOff`, message "Back-off restarting failed container `<name>` in pod …",
`involvedObject: {kind: Pod, fieldPath: spec.containers{<name>}}`, `source.component: kubelet`. The
reason string, message prefix, involvedObject shape and the core-v1 `count` / `firstTimestamp` /
`lastTimestamp` fields are **byte-identical between 1.30.0 and 1.37.0**.

**The kubelet aggregates.** Repetitions collapse into one Event object per `(pod, container)` with a
rising `count` — measured at `count=11` over 9m17s for a six-restart crashloop. So the watch sees
updates, not one object per restart.

**Core `v1.Event` is the right API.** The kubelet writes core-v1-shaped events on both versions. The
same `BackOff` object read through `events.k8s.io/v1` has `series: null` and `eventTime: null`, with
everything in `deprecatedCount` / `deprecatedLastTimestamp` — so a design that keys on
`series.lastObservedTime` reads nothing. The converse also matters: an event *written* through
`events.k8s.io/v1` and read back through core v1 has `count: null`, `firstTimestamp: null` and
`lastTimestamp: null`, so any timestamp gate needs a defined branch for null.

**An OOMKilled container emits no OOM event.** This is the finding that reopens the premise. A
container killed by the cgroup OOM killer produces **no `OOMKilling`, no `Killing`, no `Failed`** —
only `BackOff`, and only while it is in restart backoff. The pod carries
`lastState.terminated.reason: OOMKilled, exitCode: 137`, which the bundle already records, but the
*event stream says nothing about OOM*.

Consequence: a container that OOMs every 20–30 minutes never enters restart backoff and therefore
produces **zero Warning events, forever**. An earlier draft claimed "one reason, both flagship cases".
That is true of a *fast* failure loop — which is exactly what `kairn demo` simulates, which is why the
claim survived until somebody measured a slow one.

**Latency favours the event path, not the alert path.** `BackOff` lands in about 10 s. An alert with
`for: 5m` lands in five minutes. An earlier draft — and my own reasoning when reporting this to the
owner — had it that both paths are equally late; that was wrong in the direction that matters.

**Cluster-scoped events are filed in `default`.** A cluster-wide watch will be handed Node events
(`kind=Node`, no pod) with `metadata.namespace: default`, so `involvedObject.kind == Pod` is a hard
filter, not a nicety.

**Delivery volume, for sizing.** 40 crashlooping pods sustained **29–44 Warning deliveries per
minute** over 8 minutes, and a watch attaching to that cluster received **44 `BackOff` in the initial
list within 0.2 s**, 42 of them already at `count=3`.

## What the first design got wrong

Recorded so a redesign does not repeat it. The full review is in `docs/design-review-round16.md`.

**Labels on the CR are not available as an index.** The proposal wanted
`kairn.dev/{namespace,pod,bucket}` to answer "has a capture been made for this target?". Two of the
three are illegal: the bucket value is `YYYY-MM-DDTHH:MM` (`webhook.rs:237`) and a colon is not a
legal label value, and a pod name can exceed the 63-byte cap. Setting them makes **every**
`IncidentCapture` create fail 422 → 500 → no capture at all, including on the shipped Alertmanager
path. The question is answerable for free from `Controller::store()`, which kube-runtime 0.99.0
exposes and which is already populated.

**A `lastTimestamp` startup gate does not gate.** An in-progress crashloop's `lastTimestamp` is always
now, so the whole initial list passes it. `firstTimestamp >= process_start` is the condition that
means "this crashloop began after this process did". And the analogy to the notify process-start gate
was false: that one compares the **CR's `creationTimestamp`** (`reconcile.rs:402`), which is why it
works.

**"Yield to Alertmanager" runs backwards.** Since the event arrives first, there is nothing to yield
to; it is the late alert that would create the second capture, unchecked. Any yield needs an explicit
grace delay and must be documented as best-effort — the only atomic dedup in this codebase is the
deterministic name and its 409 (`webhook.rs:266`).

**A per-workload token bucket contradicts settled policy.** `design-notify.md` opens with "One capture
per pod is right; one message per pod is not", and `DESIGN.md` §6.1 makes the target part of the key
on purpose. A silent 6% sample of an incident is not a defensible artifact for an evidence tool. The
costs the bucket was meant to control already have homes: retention for bytes, notify grouping for the
channel, a global ceiling for the KMS bill.

**Nothing bounded the work.** With the proposal's own caps, 40 crashloopers in 4 namespaces produce
**12 captures in 200 ms**, all reconciling at once because kube-runtime's default is `concurrency: 0`
(unbounded), and the bucket refills: **~72 bundles an hour, ~1700 a day, while Slack sits in its
30-minute cooldown**. The operator's first signal is the PVC filling — the chart's default is 1 GiB
and nothing deletes a bundle (`DESIGN.md` §11).

## Where a redesign should start

1. **Pod status, not events, as the primary signal.** A `restartCount` increment with
   `lastState.terminated.reason` present is *complete* where the event stream is not — it catches the
   slow OOM that emits no event at all. The controller already reads exactly those fields
   (`kairn-bundle/src/summary.rs`). Events become the secondary signal, for the failures that have no
   pod-status footprint (`FailedScheduling`, `FailedMount`).
2. **Off by default**, like every other cost-bearing capability in this product (`DESIGN.md` §10.3:
   "Signing / cluster-wide / S3 export are opt-in upgrades").
3. **Bounded work, stated as numbers**: a queue that drops and counts when full, a hard global ceiling
   per hour that stops rather than refills, and `Controller::concurrency(n)`.
4. **One capture per pod**, as the alert path does.
5. **Retention first.** Anything that raises the capture rate before bundles can expire turns a
   1 GiB default into the thing that breaks the *alert* path too.
6. **`trigger.source` in the manifest** is still worth having — a reader should be able to tell "an
   operator's rule fired" from "Kubernetes complained and nobody had a rule" — but it sits inside the
   signed identity tuple (`manifest.rs:43`), so it changes what `kairn verify` checks fail-closed, it
   needs a `COMPATIBILITY.md` §1 reader-rule classification, and it obliges new fixtures. That is a
   real spend to argue for, not an aside.
