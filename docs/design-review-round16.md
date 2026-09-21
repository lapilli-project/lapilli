# Design Review — Round 16 (the Warning-event trigger, returned to its premise)

Constitution: **Loop Engineering Constitution v0.5.0**. Artifact: `docs/design-event-trigger.md` — a
**proposal**, reviewed before any code was written.

**Snapshot: the proposal as first written**, preserved in this log by quotation. Two lenses, one
round: a platform/Kubernetes lens told to verify the factual claims on real clusters, and a
product/scope lens told to decide whether this should be built at all.

## Round 0

- **Category:** architecture decision. A second trigger path changes what the product *is* — it
  reverses `DESIGN.md` §8.3's "events … read at capture time, **not a watcher**" — and the proposal
  wanted a new field in the frozen `ieb/v1` manifest plus three new labels on the CR, which become a
  selection surface people depend on.
- **Break-even:** cost ≈ 2 critic runs. Downside is two-sided and asymmetric: without it, incidents
  nobody wrote a rule for are never recorded; with it wrong, **an evidence tool accumulates bundles
  nobody asked for and pages a channel about them**, which for a product whose entire pitch is
  trustworthiness is the more expensive mistake. Reviewing a document is also far cheaper than
  reviewing the controller it would have become.
- **Sizing:** 2 lenses × 1 round, at the design stage rather than after implementation.
- **Independence: not achieved (calibration only).** Both critics were Claude. One built two kind
  clusters (1.30.0 and 1.37.0) and measured what Kubernetes actually emits; the other read the
  project's own documents against the proposal. That is calibration, not independence.

## Findings

Both lenses, independently, produced the same first BLOCKER.

| # | Sev | Finding | Steelman attempt | Disposition |
|---|---|---|---|---|
| E1 | BLOCKER | **The labels the proposal invents cannot be set, and adding them would break the shipped Alertmanager path.** `kairn.dev/bucket` would carry the value `webhook.rs:237` produces — `2026-09-21T02:14` — and a colon is not legal in a label value; `kairn.dev/pod` would exceed the 63-byte cap for any Deployment name over ~45 characters. The create fails 422, `webhook.rs:290` turns it into a 500, and **no capture is made at all**. The proposal's own sentence — "the labels … change no behaviour for the webhook path" — is the exact opposite of true. | None. Reformatting the bucket concedes the finding, and the alternative both lenses found is better anyway. | **APPLY — the labels are deleted.** The question they existed to answer ("has a capture already been made for this target?") is answerable from the controller's own reflector: `Controller::store()` exists in kube-runtime 0.99.0 and the controller is already a ListWatch reflector. Zero API calls, zero new surface, zero compatibility spend. |
| E2 | BLOCKER | **The startup replay gate does not stop the replay it was written for.** An in-progress crashloop's `lastTimestamp` is *always* now, so it sails past a `lastTimestamp >= process_start` gate. Measured against 40 already-crashlooping pods: the initial list delivered **44 `BackOff` events in 0.2 s, 42 of them with `count=3`** — crashloops already several restarts old. And on `events.k8s.io/v1` `lastTimestamp` is `null`, proven by writing an event through that API and reading it back through core v1. | None. And the proposal's claim that this is "the same gate as notify" is wrong in a way that explains the bug: the notify gate compares the **CR's `metadata.creationTimestamp`** (`reconcile.rs:402`), not an event's timestamp. | **APPLY.** Core `v1.Event` named explicitly, and the gate becomes `firstTimestamp >= process_start` — a crashloop that *began* after this process did. A null `firstTimestamp` is dropped with a counted reason rather than guessed at. |
| E3 | MAJOR | **The yielding is backwards, and unimplementable in the direction the proposal claims.** The kubelet's `BackOff` lands in about 10 s; an alert with `for: 5m` lands in five minutes. So the event trigger always wins and it is the *alert* that arrives second, unchecked — the opposite of "the event trigger yields". Separately, the per-event LIST it proposed is a quorum etcd read (`ListParams.resource_version` defaults to `None`, so `Api::list` bypasses the watch cache) answering a question the reflector already holds, and LIST→create is not atomic anyway. | **This refutes my own earlier reasoning, not the critic's.** I had argued to the owner that "both paths are late, so the event path is not worse." The measurement says the event path is *faster*, which raises the feature's value and destroys its dedup mechanism at the same time. | **APPLY.** Any yield needs an explicit grace delay — hold the trigger, drop it if a capture for that target appears in the store meanwhile — and must be documented as best-effort, because the only atomic dedup in this codebase is the deterministic name and its 409 (`webhook.rs:266`). |
| E4 | MAJOR | **There is no bounded-work story.** The token bucket bounds captures per workload and nothing bounds the controller. Walked through concretely with the proposal's own caps, 40 crashloopers in 4 namespaces: **12 captures created in 200 ms**, all reconciling at once because kube-runtime's default is `concurrency: 0`, i.e. unbounded; 4 Slack messages, then a 30-minute cooldown; and the bucket refills — **~72 bundles an hour, ~1700 a day, while the channel stays quiet.** The operator's first signal is the PVC filling. The bucket also degenerates to per-pod for any owner `controller_owner_of` does not recognise: a bare pod, a Job pod, an Argo or Spark pod. | None; the walkthrough is arithmetic over measured delivery rates (29–44 Warning deliveries/minute at 40 pods, sustained over 8 minutes). | **APPLY.** Off by default. A bounded queue that drops and counts, the way the notify dispatcher does; a hard global ceiling that stops rather than refills; `Controller::concurrency(n)`; and a stated fallback key for unrecognised owners. |
| E5 | MAJOR | **`BackOff` does not cover the OOM case the proposal claims it covers.** A container killed by the cgroup OOM killer emits **no OOM event at all** — no `OOMKilling`, no `Killing`, no `Failed` — only `BackOff`, and only while it is *in* restart backoff. So a container that OOMs every 20–30 minutes never enters backoff and produces **zero Warning events, forever**. "One reason, both flagship cases" is true of a *fast* failure loop, which is what `kairn demo` simulates, and false in general. | None. Measured identically on 1.30.0 and 1.37.0, with `lastState.terminated.reason=OOMKilled, exitCode=137` present on the pod while the only Warning event is `BackOff`. | **APPLY,** and it reopens the premise — see the verdict. |
| E6 | MAJOR | The problem statement is unsupported and contradicted by the repository, and the proposal quietly promoted a roadmap item marked "best-effort" twice into an on-by-default primary path. "The largest adoption cost in the product" is asserted; `kairn demo` already fires its own alert from inside the controller pod, so Alertmanager is not required to see value, and §10's own title names the real barrier as the **deferred-value** trap. The sanctioned argument for this feature — §10.2, "Trigger on everyday failures … so the evidence folder fills up weekly, not once a year" — is never quoted. | None; every claim is quotable from the repo. | **APPLY.** The problem statement is replaced with §10.2's, the unsupported claim is deleted, and the reversal of §8.3 is stated as a reversal. |
| E7 | MAJOR | **The token bucket invents a second, weaker evidence policy for a case the project already settled the other way.** `design-notify.md` opens with "One capture per pod is right; one message per pod is not", and `DESIGN.md` §6.1 makes it structural. Dropping 47 of 50 pods' evidence — without recording which 47 — is worse for an evidence tool than 50 bundles, and it contradicts the proposal's own sentence that "only the trigger differs". | Tried: "the alert path's 50 captures are the operator's explicit choice, the event path's are Kairn's, so a cap is justified." The distinction is real, but a silent 6% sample of an incident is not a defensible artifact for this product, and the costs it was meant to control already have homes — retention for bytes, notify grouping for the channel, a global ceiling for the KMS bill. | **APPLY — the per-workload bucket is dropped.** |

**Attacks that failed, which is what makes the rest worth anything.** The load-bearing factual claim
**holds**: `BackOff`'s reason string, message prefix, `involvedObject{kind: Pod, fieldPath:
spec.containers{…}}`, `source.component: kubelet` and the core-v1 `count`/`firstTimestamp`/
`lastTimestamp` shape are **byte-identical between 1.30.0 and 1.37.0**, and the kubelet does
aggregate repetitions into one object with a rising count (measured: `count=11` over 9m17s for a
six-restart crashloop). The API question is settled in favour of **core `v1.Event`**: the kubelet
writes core-v1-shaped events on both versions, and the same object read through `events.k8s.io/v1`
has `series: null` and `eventTime: null`, so a gate on `series.lastObservedTime` would read nothing.
The chart's existing `apiGroups: [""], resources: ["events"], verbs: ["get","list","watch"]` does
cover the watch — but only in cluster-wide mode; with `watchNamespaces` set the chart emits a
namespaced **Role** per namespace, so one cluster-scoped watch would 403 and the trigger needs N
namespaced watchers. The product lens also read all seven of `DESIGN.md` §2's non-goals literally and
found that **none** of them prohibits this feature; what it found instead was the "best-effort"
downgrade and the §8.3 reversal.

## A process failure that this round exposed

**Round 11 concluded that the postmortem draft is the stronger feature and recorded it as "the
roadmap's next item". It was never put in the roadmap.** `grep -c postmortem DESIGN.md` returns 0.
So when I went looking for the next substantial thing to build, the roadmap offered me the weaker
item — the one marked "best-effort" twice — and I wrote a design document for it.

A review conclusion that does not reach the plan has no force. The roadmap is now corrected, and the
lesson is narrower than "write things down": **a round's verdict has to land in the artifact that
decides what happens next**, which for a feature is `DESIGN.md` §11 and not only the round log.

## Verdict

**RETURN-TO-PREMISE.** Not "apply the fixes": every central mechanism the proposal invented is wrong.
The labels break the shipped path, the startup gate does not gate, the yield runs backwards, the rate
cap contradicts a settled policy, and nothing bounds the work. What survives is the *goal* — capture
incidents nobody wrote a rule for — and a set of measurements that are worth more than the document
was.

The measurements also point somewhere the proposal did not look. Because a slow OOM produces **no
event at all**, the complete signal is not the event stream but **Pod status**: a `restartCount`
increment with `lastState.terminated.reason` present. The controller already watches pods for other
reasons and already reads exactly those fields (`summary.rs`). A redesign should start there and
treat events as the secondary signal, not the primary one.

**Sequencing is the owner's call, and it is a real fork.** Three candidates, no evidence-based winner:

1. **Bundle lifecycle (retention and deletion)** — already in `DESIGN.md` §11 with its constraints
   written. It is a gap *today*, not hypothetically: nothing deletes a sealed bundle and the chart's
   default PVC is 1 GiB. It is also the precondition that makes any higher capture rate safe.
2. **The postmortem draft** — round 11's product lens called it the stronger feature, and this round
   found that verdict never reached the roadmap.
3. **The trigger, redesigned around Pod status** — highest value if the goal is "install and it
   records", and now the best-understood of the three thanks to this round's measurements.

My recommendation is 1, then 2, then 3: retention is the only one of the three that is a defect
rather than an absence, and it is the one the other two both benefit from.
