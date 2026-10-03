# Design review — round 36 (the warning-tier positioning, killed; and the measurement under it, corrected)

Constitution: **v0.7.1**. Target: a positioning claim that had been stated in conversation and never
written down —

> "kube-prometheus-stack's critical tier contains no user workload, so the **warning** tier is this
> product's defensible place: a warning nobody looks at is where evidence dies unobserved, because
> nobody was woken to run must-gather."

Snapshot: **5bd155f** (`docs/design-workload-target.md` and the fixtures beside it).

Sized at the strategy floor (§IV): **the object's self · the incumbent's user · frequency and
trigger**, plus a null-result lens aimed at round 35 §4 and a measurement-audit lens aimed at the
numbers rather than the strategy. Five lenses, because the claim would have changed a published
pitch — `cheap-first` does not apply to strategy.

**Independence: not achieved.** Sixth round running. All five lenses are Claude-family; the web
searches and source fetches they ran are the only outside input. This is calibration, not
independent review.

Goal order pinned before analysis: **first a CNCF Sandbox listing, second adoption that could be
sold.** The identity sentence is a constraint above both, and no finding here required rewriting it.

## Verdict: KILL

The positioning did not survive. What survives is the measurement under it, with five corrections —
two of them false statements I had already committed and opened a pull request with.

## 1. The strongest signal: three lenses independently found the claim was not written down

The self, frequency and incumbent lenses each grepped the tree for the sentence and each found
nothing. `docs/design-workload-target.md:168` nonetheless pointed at *"the warning-tier reframe in
§0"*, and §0 ends at a measurement — *"the user-workload tier is `warning`"* — and stops.

A positioning claim that exists only in conversation cannot be checked by a reader, and this is the
second round running where the same convergence was the round's best output (round 35 §5b). The
mechanism is worth naming: **a claim spoken and acted on but never written is invisible to every
instrument this project has.** It survives by not being reviewable.

## 2. Why the positioning died

| # | objection | lens | disposition |
|---|---|---|---|
| 1 | The claim is not in the repository while a later section references it | self · frequency · incumbent | **APPLY** — written here, killed here |
| 2 | "The warning tier" is a property of a third party's YAML, not of Lapilli: `severity` appears **once** in the Rust tree, in a demo fixture (`demo.rs:304`), and round 33 already established that *what reaches the webhook is decided by the Alertmanager route tree*. A route tree can page on warnings and silence criticals, so severity is a **correlate of attention, not attention** | self | **APPLY** |
| 3 | The warning tier is not new ground — it is Lapilli's **shipped scope**. `charts/lapilli/templates/NOTES.txt:38` routes `KubePodCrashLooping\|KubeContainerWaiting\|KubePodNotReady`, all three upstream `warning`, and round 31 priced that walk at *"minutes saved: zero. Minutes added: about one"* | frequency · incumbent | **APPLY** |
| 4 | Robusta ships a **named builtin, on by default**, for the exact alert all four modes fired, and it performs measurement A's procedure. Verified against source rather than taken from the lens: `helm/robusta/values.yaml:449` binds `pod_issue_investigator` + `deployment_events_enricher`, and `event_enrichments.py:320` is `list_pods_using_selector(ns, dep.spec.selector, "status.phase!=Running")` then per-pod events | incumbent | **APPLY** |
| 5 | The retrospective trigger has a public base rate of **0 of ~357**: k8s.af's 57 writeups name it zero times, and of ~300 in `danluu/post-mortems` exactly one names evidence as the blocker — application logging, which `demand-test.md` already excludes. Round 29 F5 measured 18 and this verified it on a 20× sample | frequency · null-result | **APPLY** |
| 6 | Read rate is **structurally unmeasurable**: the bundle is offline, credential-free and portable by design, which is precisely what makes an open invisible to the controller that publishes metrics. Owed since round 29 F4, falsely marked *Applied*, re-ordered by round 35 §1, still absent — and it cannot be built | null-result | **APPLY** — stop treating "opened" as a metric |
| 7 | "The events are gone" is **false for a default paid Slack workspace**: Robusta forwards every severity (`severity =~ ".*"`), the enricher copies the event out at t+seconds as inline table blocks, and Slack keeps them for the workspace's lifetime. **The 1 h TTL stops mattering the moment the enricher copies the event out** | incumbent | **APPLY** |
| 8 | Google's canonical postmortem triggers all require that somebody acted or a user was visibly harmed; an ignored warning satisfies none, and the one route in (a stakeholder request) requires user-visible consequence — at which point it was paged, and §0's premise no longer describes it | frequency | **APPLY** |
| 9 | Our own install instruction contradicts the premise: `NOTES.txt:40` makes `continue: true` mandatory *"so the alert goes to both"* Lapilli and the paging route. An operator who genuinely does not look at warnings has no paging route — **the premise and the install describe different operators** | frequency | **APPLY** |
| 10 | Every analogue with the install-before-the-incident property was adopted by **mandate or default inclusion**, never by latent need: flight recorders by law (1941 CAB, 1964 CVR, UK 1965 — *"these legal mandates, rather than industry initiative, drove adoption"*), Replicated because the vendor requires it as a condition of support. `demand-test.md` §2 has **no outcome row** for "the need is confirmed and no purchase trigger exists" | frequency | **APPLY** |

### Refuted, and recorded as refuted

| objection | why it died |
|---|---|
| "The counts 58 / 35 / 23 are not reproducible from the stated method" (audit lens recomputed 48 / 28 / 20) | **REFUTED by recomputation.** They reproduce exactly; the lens used a different label set — it added `replicaset`, `cronjob`, `owner_name` and dropped `controller`. A different classification, not an error in the document. `test/fixtures/workload-target/classify-rules.py` is now committed so this is settled by running it rather than by argument |
| "The reframe contradicts the published *'nobody is awake at 02:14'* urgency" | **PARTLY REFUTED.** A warning is a *different* scenario — nobody logs in at all — so both can be true. What stands is that the published text never describes the warning case |
| "Round 31 K3 stands whole" | **PARTLY REFUTED.** K3 claimed *"events are already coalesced by then, and any pod the ReplicaSet replaced in those fifteen minutes is unreadable from the API"*. Events were present with readable reasons at t+15m in all four modes, so the **reachability half is refuted by measurement**. K3's **value** half — a tier nobody is paged for is worth near zero — is untouched, and the killed positioning needed exactly that half to be wrong |
| "The reframe requires rewriting the identity sentence" | **REFUTED, and deliberately not escalated.** A warning-severity alert *is* an operational signal, so the claim was a subset of the trigger clause, not a contradiction of it. Unlike round 35 §5b's `correlates`, there was no clause to defend. The self lens declined to manufacture an owner escalation, which is the correct call |

## 3. Five corrections to the measurement, two of them false claims already pushed

The audit lens recomputed rather than read, and found the document I had already opened a pull
request with contained two statements that are not true:

1. **Decision rule (a) was never measured.** The script resolves Deployment → ReplicaSet and then
   discards it — `$rs` is echoed and never used — and takes pods from `-l app=<name>`, a label its own
   manifests plant (`measure-failure-modes.sh:135,138`). A controller holding a declared workload
   target has no such label. Worse, for `rollout` that selector **merged both ReplicaSets into one pod
   set**, which is exactly the disambiguation the open question turns on.
2. **`--previous` was not 3 of 3 at every sample.** Recounted from every `prev=` line: `crashloop`
   **0/3** at t+1m then 3/3, `rollout` **0/3** then 2/2 — **10 of 16 = 62%**, which *reproduces* round
   35's 7 of 10 instead of needing the reconciliation paragraph I wrote. That paragraph explained away
   a disagreement that did not exist and buried the finding the data produced: **at t+1m the union over
   three replicas was zero**, because the replicas crash-loop in lockstep within seven seconds. So
   **replica count does not lift availability under correlated restarts** — a question §3 listed as
   untested and which the run had already answered.
3. `KubePersistentVolumeFillingUp` and `KubePersistentVolumeInodesFillingUp` are **two different
   rules**; the table called them "both" one, and missed `KubePersistentVolumeErrors`, so storage is 3.
4. The headline *"there is no user workload to record"* contradicts its own table — **a PVC filling up
   is a user workload's data volume**. The honest form is a statement about labels: none of the 41
   criticals carries a `pod` or workload-controller label.
5. The method **undercounts its own best number**: 15 rules interpolate no labels at all, 8 of them
   critical, and four of those (`KubeStateMetrics*`) are self-monitoring. "The monitoring stack 8" is a
   floor, not a count.

And one measurement gap that is not an error but matters more than any of them: **`KubeContainerWaiting`
is `for: 1h` and the default event TTL is 1 h.** It is one of the three rules Lapilli actually ships a
route for, §1 found that the decisive evidence in the two waiting-container modes is an *event*, and
measurement A sampled t+1m/5m/15m only. **The one shipped rule whose firing time coincides with the TTL
of its own evidence was never sampled at its firing time.**

## 4. What still stands

- The critical tier carries no `pod` or workload-controller label, so **"0 of 41" measured the chart's
  tier assignment and not Lapilli**. Three rounds quoted it without decomposing it. This survives as a
  correction to a self-criticism, not as a reason to exist.
- Evidence a log store does not hold is **reachable at t+15m** in all four failure modes, and when no
  pod can be named the reason is on the workload object (`status.conditions` ahead of events).
- **Events, not logs, are the decisive evidence** in three of four modes — `badimage` and `unsched`
  never ran a container.
- Collection is **both/and**: conditions alone miss `terming`, pods alone miss `quota` and `paused`.
- One narrow gap in the incumbent's **default**: the `quota` shape (no failing pod) gets
  `pod_issue_investigator` returning early, `deployment_events_enricher(Warning)` finding only
  `ScalingReplicaSet`, and `dependent_pod_mode` matching nothing — so Robusta posts the bare alert with
  no reason. It closes with four lines of `customPlaybooks` (`status_enricher` with `show_details`), so
  it is **not a differentiator**.

## 5. What this round taught the method

**A claim that is spoken but never written is unreviewable, and acting on it is the failure.** Three
lenses spent their first probe discovering the target did not exist in the repository. The rule this
earns: before a positioning claim is used to justify work, it is written where a reader meets it, with
its falsifier attached.

**A measurement is an artifact with its own premises, and mine failed three times.** A Korean particle
bound into a shell variable name; a missing `chmod`; and the expensive one — a namespace-wide
`ResourceQuota` with three experiments in one namespace, which collapsed all three modes into one case
and reported a plausible **3 of 3 pass**. Experiment design, not code.

**Writing a reconciliation is a warning sign.** When two measurements disagreed I wrote a paragraph
explaining why both could be true. The numbers actually agreed (62% vs 70%), and the paragraph's real
function was to let me keep a sentence — "3 of 3 at every sample" — that I had read off one sample and
generalised. Prefer recomputing to reconciling.

**The incumbent's defaults are part of the landscape, and reading its marketing is not reading its
source.** `DESIGN.md`'s Robusta row has now been corrected twice: round 35 at pod level, round 36 at
workload level, where it had omitted a named builtin that performs this project's newest measurement
procedure. Both corrections came from reading the chart and the Python, not the docs site.
