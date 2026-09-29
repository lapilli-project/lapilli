# Design review — round 31 (trigger coverage: the proposal did not survive)

Constitution: **v0.7.1**. Target: `docs/design-trigger-coverage.md` at **`b3f27f2`** — a proposal, written
to be attacked, for the first v0.2 item. Five lenses in one wave: **identity** (does this describe
Lapilli in its own words?) · **the frozen format and compatibility** · **the cluster and its measured
load** · **privacy and data handling** · **the incumbent's user** (an on-call SRE who already has
Prometheus, a log store and Argo CD).

Stakes: the first feature change on top of a **published** v0.1.0 — tag, GitHub release, GHCR image,
OCI chart and five crates.io crates all exist — touching a frozen format and a user-data path. Sized
5 × 3 accordingly. Mandatory floors met: public → format/compatibility and the incumbent's user;
user data → privacy; a product-defining change → the identity lens first.

**Independence: not achieved.** All five lenses are the author's own model family. Gemini answered
404 on every model this session and no non-Claude path was available, so this wave is calibration: it
finds contradictions between what the tree says and what it does, and between the proposal and the
records it cites. It cannot say the proposal is right. Logged as a limit, not solved.

## Verdict: RETURN TO PREMISE

**Proposal D is dead. The hole it addresses is not.** Nine findings at BLOCKER survived the steelman
test; three of them are not patchable details but attacks on the premise, and together they say the
proposal chose its scope by which collector already existed rather than by where evidence perishes.
Nobody refuted round 29 F4 — Lapilli still fires on the class `kubectl logs --previous` and a git
diff mostly answer, and still refuses the class an SRE is paged for. What is refuted is D as the
answer to it.

The replacement is a **fork**, escalated to the owner rather than decided here, because the three
surviving directions are mutually exclusive in sequence and the evidence does not pick a winner
without a measurement none of us has: §"The fork" below.

## 1. The three that killed it

**K1 · D rejects B for a reason that applies to D.** The proposal refuses option B — resolve the
alert down to pods — because "a replicas mismatch is frequently an *absence* of pods, and resolving
to pods yields the empty bundle round 16 already refused". D inherits exactly that emptiness and
states no refusal floor: §3 says logs are "none when every pod is Ready, which is itself the
evidence". So in D's own headline case the bundle carries the workload YAML, its events and a diff —
and DESIGN.md §3, rewritten by round 29 F3, concedes precisely that layer to Sloop and to the
`RequestResponse` audit log, keeping as Lapilli's edge "the object *and* its status, **the previous
container's log** and the rollout diff, sealed together at the alert". A signed `kubectl get -o yaml`
is not a flight recorder.

**K2 · "One incident, one artifact" is not Lapilli's sentence.** Its only prior occurrence in the
repository is inside `docs/design-trigger-and-load.md` §5's *open question*, which calls the choice
"a product decision that is also a storage decision". The proposal cites the phrase back as doctrine
and says it "settles" the question, while its own §4 item 3 lists the same collapse as an undecided
trade. It also silently reverses settled policy — `docs/design-notify.md`: *"One capture per pod is
right; one message per pod is not"* — and its log-selection rule, "the pods that are not Ready,
newest first, up to a bound", is verbatim the silent sample round 16 killed as E7: *"a silent 6%
sample of an incident is not a defensible artifact for this product."*

**K3 · The v0.2 cut is inverted, and web-verified against upstream.** Every rule D accepts is
upstream `severity: warning`, `for: 15m` (`kubernetes-mixin` `apps_alerts.libsonnet`):
`KubeDeploymentReplicasMismatch`, `KubeStatefulSetReplicasMismatch`, `KubeDaemonSetRolloutStuck`,
`KubeJobFailed`. Nothing in the cut pages anyone, and `for: 15m` means the capture arrives fifteen
minutes after the condition began — which voids, for the whole class being added, DESIGN.md §3's
promise of capture "at alert-time-plus-seconds, before the volatile evidence finishes rotating away".
Events are already coalesced by then, and any pod the ReplicaSet replaced in those fifteen minutes is
unreadable from the API.

Walked at 03:00 by the incumbent lens: `kubectl describe deploy` (~20 s) gives desired/available and
the scaling events, `kubectl get rs` (~10 s) says whether a rollout is the cause, `describe pod` on
one unhealthy pod (~30 s) gives `FailedScheduling: Insufficient cpu` or the image pull or the quota,
and Argo holds the revision and git SHA that `diffs/` does not. **Minutes saved by the bundle: zero.
Minutes added: about one, to fetch it off the PVC.**

Of the seventeen dropped rule names in the proposal's own table, D covers eight and leaves nine —
including both examples round 29 F4 leads with, SLO burn rate and node conditions. The one row where
evidence is genuinely destroyed is `KubeJobFailed`: a CronJob at `*/5` with
`failedJobsHistoryLimit: 1` deletes the failed pod, and short-lived pods are commonly missed by log
shippers. The proposal buries it as one of four equals.

## 2. The six that would have to be paid even if the cut were right

Each was measured or computed, not argued.

**F1 · An omitted `pod` makes a correct bundle indistinguishable from a tampered one.** Option A
verbatim, run against the released verifier (HEAD is byte-identical to `v0.1.0` on every path
touched): `FAILED … malformed v1 manifest: missing field 'pod'`, **exit 1**, problem code `manifest`
— the same verdict and exit code as one tampered byte, on every binary an adopter has already
downloaded, while `docs/COMPATIBILITY.md` teaches them `case $? in 0) ok ;; 2) partial ;; *) reject`.
`manifest.rs`'s `Target { namespace: String, pod: String }` has no `Option` and no `serde(default)`.
The only shape that passes is `pod: ""`, which is a false statement inside a signed manifest and is
dropped silently by `find_bundles` either way.

**F2 · The format question is bigger than `incident.target`.** `spec/IEB-SPEC.md` rule 6 makes
`resources/pod.json` **required** whenever `resources` is in `collectors_run`, so a workload-shaped
`resources/` is FAILED on a v1 verifier even with the target question solved — reproduced, exit 1.
`logs/<container>-current.log` has no pod dimension and `logs/index.json`'s frozen schema has no pod
key, so with N pods those names are a collision rather than a location. And `summary.rs`'s single
`read("resources/pod.json")` is the one reader behind the notification, `lapilli postmortem` and
`lapilli mcp summary`, so the legal shape yields a bundle that verifies OK and renders an empty
document.

**F3 · Nothing mechanical would have caught F1 or F2.** Not one of the 43 frozen v0.1.0 fixtures
carries `incident.target`, and `test/spec/build_from_spec.py` writes none — so the conformance suite
and the spec-only-producer gate both go green on a change that breaks the installed base. This is a
coverage hole that exists **today**, independent of any proposal, and it is the cheapest thing on
this page to fix.

**F4 · The bounds do not fit the shipped envelope.** `LOG_LIMIT_BYTES` is 4 MiB *per container
instance*, and its own comment enumerates the multipliers as containers × (current+previous) ×
reconcile concurrency, concluding "16 MiB, about 6% of the 256Mi limit". The pod count is not in that
formula. A 100-node DaemonSet capture is 100 × 1 × 2 × 4 MiB = **800 MiB** of log bytes; two in
flight is **1.6 GiB staged against a 1 GiB default volume**, whose only pre-flight is a one-shot
64 MiB free check before collection starts, and whose own values file says a full volume makes *every*
capture fail. At the payload cap of 50 that is **40 GiB** of log reads authorised by one payload.
Fetch order is load-bearing and unenforced: fan out the 100 reads and memory in flight is 1.6 GiB
against a 256 MiB limit.

**F5 · The dedup identity loses evidence through the one channel the accounting cannot see.** The
identity is `{rule, cluster, target, minute-bucket}`. With a workload target, a second genuinely
different incident in the same minute — new-ReplicaSet bad config at 10:14:03, old-ReplicaSet
OOMKill at 10:14:47 — produces the same name, hits the 409 path, and is counted as
`webhook_duplicate`, indistinguishable from an Alertmanager resend. `captured + dropped = sent` still
balances, so no rule fires. Meanwhile the co-firing case goes the other way: three
`KubePodCrashLooping` plus one `KubeDeploymentReplicasMismatch` is **four** signed bundles and **two**
notifications for one incident, with the same log bytes sealed twice at two collection times and
nothing saying which is authoritative. So D's headline claim is false in both directions.

**F6 · Privacy: the unit of everything becomes the workload.** Per-pod erasure becomes structurally
impossible — rewriting a bundle to drop one pod's logs invalidates the hash tree and the signature,
and deleting it destroys evidence about the others, which Object Lock forbids anyway — while the
pod→bundle index that would locate the data is exactly what the additive-target shape removes.
`mcp.allowLogs` is one boolean over a whole bundle, so one allowed read hands a model provider N
pods' never-redacted logs. And the deferral of cluster-scoped *kinds* does not defer cluster-scoped
*data*: every pod body carries `spec.nodeName` and `status.hostIP`, which no mode redacts, so a
DaemonSet capture is a namespaced read that yields a cluster-wide node inventory. "Every pod it owns"
also names no resolution mechanism, and the obvious one — the workload's label selector — is not a
containment boundary, so in a shared namespace it reaches another tenant's pods.

## 3. Dispositions

| # | Finding | Disposition |
|---|---|---|
| K1 | D inherits B's emptiness with no refusal floor | **RETURN TO PREMISE** — and the refusal predicate it asks for is a requirement on whatever replaces D |
| K2 | "One incident, one artifact" is invented doctrine; log selection is round 16's killed silent sample | **RETURN TO PREMISE** |
| K3 | The cut is by collector convenience, not by where evidence perishes | **RETURN TO PREMISE** |
| F1 | Omitted `pod` → FAILED exit 1 on the released verifier | **APPLY to the replacement**: `target.pod` always a real pod name, or `ieb/v2` with the counted bill |
| F2 | Rule 6, the log path namespace, and `summary.rs`'s single reader | **APPLY**: a new collector name is genuinely additive per COMPATIBILITY.md; the readers come first |
| F3 | No fixture carries `incident.target`; the spec producer writes none | **APPLY NOW**, independent of this design — a gate that cannot see the field it governs |
| F4 | Per-instance bound × pod count blows the memory limit and the volume | **APPLY**: per-capture byte budget, max pods-with-logs, fetch order enforced, re-measured |
| F5 | Dedup collapse counted as a resend; co-firing makes 4 bundles and 2 notifications | **APPLY**: a correlation key independent of `rule`, and the collapse counted as its own outcome |
| F6 | Erasure unit, index loss, `allowLogs` blast radius, node inventory, selector containment | **APPLY**: pods resolved by `ownerReferences` controller uid only; captured pod set recorded in the manifest; `docs/data-handling.md` and `docs/egress.md` updated in the same change |
| M1 | Option C was rejected against a cost it does not incur — it is one Alertmanager `matchers` line, which is what `docs/metrics.md` already tells the operator | **APPLY**: C's real cost restated; D's successor must beat C on the sub-case where pods exist and are unhealthy |
| M2 | `target.object = {kind, name}` cannot express a cluster-scoped target, so "additive rather than a rewrite" is unsupported by the proposal's own shape | **APPLY**: `{kind, name, namespace?}` decided now if the shape is taken at all |
| M3 | The only measurement in the document is a `NodeNotReady` — the case it defers — while `ROADMAP.md` §4 and `DESIGN.md` both schedule node-level captures for v0.2 | **APPLY**: either Node comes into the step or the deferral is a recorded change of plan |
| M4 | ~305 API requests and up to 800 MiB per capture, versus ~6 and ≤8 MiB today; 102 of them unindexed quorum event LISTs | **APPLY**: one event LIST per namespace, the pod list served from the informer, and the cost measured — open question 7 already admits nothing measures it |

Nothing was REFUTED, so no independent re-check of a dismissal was owed. Nothing was OVERRIDDEN.

## 4. The fork

Three directions survive, they are mutually exclusive in sequence, and the evidence does not choose
between them without a measurement nobody has. Escalated to the owner.

1. **Measure first.** Build the unique-evidence yield the incumbent lens proposed — at T+24 h, re-run
   each collector's queries against the live cluster, Prometheus and the team's log store, and report
   per rule the fraction of sealed bundles holding at least one file that can no longer be
   reproduced — ship it as `lapilli diff-live <bundle>`, publish the per-rule table, and let the table
   pick the cut. It is also the only artifact on this page that an adopter can run on their own
   cluster in week one, which is what round 29 F5 says the install trigger needs.
2. **Re-scope to where evidence perishes, and stay pod-shaped.** `KubeJobFailed` is the one accepted
   row where the evidence is destroyed, and a Job's failed pod *is* a pod — so resolving `job_name` to
   it may need no target generalisation at all. Pair it with `KubeNodeNotReady`, the trigger the
   product advertises, designed properly rather than deferred.
3. **Pay D's full bill.** Representative pod plus `target.object{kind,name,namespace?}`, a new
   collector name, per-capture byte budgets, the refusal predicate, the recorded member set, the
   correlation key, uid-based pod resolution. Large, and the incumbent lens says its value for the
   four warnings it buys is near zero.

Recommendation: **1, then 2** — measure before choosing, then cover what the measurement says
perishes. F3 is done immediately either way, because it is a gate that cannot see the field it
governs.

**Owner's decision (2026-09-29): direction 1.** Measure first. No format change, no new collector and
no change to what the webhook accepts until the per-rule table exists. The next design target is
`lapilli diff-live`, and it carries a tension this round must hand forward rather than bury: round 24
returned *backfill* to premise because it would have made Lapilli a client of other stores, and a tool
that re-runs queries against Prometheus and a team's log store is close enough to that line to need
the loop pointed at it before it is built, not after.

## 4b. Where Lapilli now exists, and what that surface had wrong

Asked after the fork, and worth recording because two of the six places were nobody's decision.
`v0.1.0` exists on: the GitHub org and its one public repository, two GitHub releases, two signed
tags, two public GHCR packages (the image and the OCI chart), five crates.io crates — and
**docs.rs**, which builds every crate the moment it is published. Sigstore's public transparency log
holds the CLI attestations permanently. `lapilli.dev` is paid for and serves nothing; the format
identifier `lapilli.dev/ieb/v1` is a URI namespace, so that is valid but a reader who types it sees
nothing. The chart is not on Artifact Hub, which is where someone looking for a Helm chart looks.

The doc comments were therefore a published surface with no gate on them, and they had two defects.
Thirty-six references to repository paths — `spec/IEB-SPEC.md`, `DESIGN.md`, `docs/design-kms.md` —
resolve in a clone and are dead on docs.rs; they are links now. And three placeholders in the CLI's
own `--help` text (`<n>`, and `<b>`/`<a>` in a test) are read by rustdoc as HTML, which renders the
rest of the line bold and swallows the next word: `rustdoc::invalid_html_tags` is a warning by
default, so the published page for the crate people `cargo install` had mangled help text and nothing
said so. A `ci` job now runs `cargo doc` with that lint and `broken_intra_doc_links` as errors,
mutation-proven at the site rustdoc actually flagged — and the first attempt at that proof used the
`<b>` in a `#[cfg(test)]` module, which rustdoc never documents, so the mutation passed and meant
nothing. The same lesson as the `lapilli-` prefix filter and the `.dep-v0` grep, for the third time
this week: a check has to be proven at a site it governs.

## 5. What this round taught the method

**A phrase invented inside an open question is not a finding, and citing it back is how a proposal
settles its own hardest trade without noticing.** "One incident, one artifact" appeared nowhere in
this project except the question it purports to answer. The author — this arbiter — wrote it, then
read it as doctrine one section later.

**A scope cut justified by what is already built will always defer the valuable half.** Every rule D
accepted was reachable with the collector that existed; every rule it deferred was one where the
recorder would earn its place. The cut was honest about *why* it was cheap and silent about whether
cheap was the same as first.

**The strongest lens was the one told to be hostile and given the web.** It checked the upstream rule
definitions rather than accepting the proposal's table, and `severity: warning, for: 15m` — five
words from someone else's repository — did more damage than any reading of ours.
