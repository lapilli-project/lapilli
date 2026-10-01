# Design review — round 35 (the target is the product, and three of its headline claims are false)

Constitution: **v0.7.1**. Target: **the claim that Lapilli should exist and be continued** — not a
document. Sized at the strategy floor (§IV): the object's self · the incumbent's user · frequency
and trigger, plus a fourth that reads the project's own 34 rounds as evidence against it.

**Independence: not achieved.** Fifth round running. All four lenses are Claude-family; the web
searches they ran are the only outside input, and the survival-path investigation's own stated
limit is that Reddit was unreachable directly and three practitioner surveys are unproven rather
than disproven.

Goal order pinned before analysis, because the same evidence supports opposite conclusions under
different goals: **first a CNCF Sandbox listing, second adoption that could be sold.**

## Verdict: the pitch is refuted, the product is not — and the gap between those two sentences is this round's whole content

Three things the project leads with are false or unevidenced. One number it has been using to argue
against itself is **partly self-inflicted**, and that is the only open door anyone found.

## 1. What survived a steelman

**The capture is not a differentiator.** Robusta — MIT, **3,104 stars, commits the same day this was
written** — fires on the same Alertmanager webhook and ships `logs_enricher(previous)`,
`pod_events_enricher`, `get_resource_yaml`, `pod_graph_enricher` and `resource_babysitter`: the
IEB's contents, enricher for enricher, plus `dmesg` which Lapilli does not take. `DESIGN.md:136`
already grades it `alert-trigger ✅` and rests the distinction on *"no portable artifact"*. What is
left to differentiate is the **seal**, not the collection.

**`README.md`'s first sentence is false.** *"When you write the postmortem three days later, the
logs, events and 'what changed' you need are gone."* Loki's default retention is **infinite**
(`compactor.retention_enabled` unset) and Prometheus's is 15 days; 82% of production clusters run a
logging solution (CNCF 2025) and Lapilli *requires* Alertmanager — or anything that POSTs the same
shape — so its only reachable shop is the one most likely to have a log store too. (The Prometheus
**server** is not required, and this sentence said it was until the claim was checked:
`metrics.prometheusUrl` defaults to empty, `charts/lapilli/templates/captureprofile.yaml` emits the
`metrics` collector only `with` it, and `collector.rs` degrades the bundle rather than failing the
capture when it is absent. The argument above does not depend on it; the word did.) The honest residue is **events (1 h TTL) and the terminal
object**, not logs and not metrics.

**0 of 41 paging alerts**, measured by the project itself (round 33), and round 31's incumbent lens
priced the bundle at *"minutes saved: zero, minutes added: about one"*.

**Round 29 was sunk cost in loop-engineering costume.** It applied the incumbent's-user and
landscape lenses to the *alternative* and not to Lapilli, while its own F2 and F3 established that
Lapilli's differentiating layer had partly closed (audit log at `RequestResponse`, Sloop). It then
justified continuing with *"only Lapilli has an application event inside the window"* — the owner's
pinned deadline used as the reason to keep the asset that fits the deadline. The sunk-cost check is
a bare sentence in a document that tables a steelman attempt for every other finding.

**The design's load-bearing premise was retracted and the decision resting on it was never re-run.**
Round 23:102 — *"The object body really is unreconstructible … This is the only thing keeping the
design alive."* Round 29:38 retracts it.

**Nothing measures the outcome.** All 41 series in `docs/metrics.md` measure machinery. Not one says
whether a sealed bundle contained the evidence, or whether anyone opened it — the two counters round
29 F4 marked **Applied**.

## 2. What I refuted

**"Zero adopters proves there is no demand."** The repository is **14 days old** and has been public
for **4**. Nobody has been asked. Zero adopters is not yet evidence of anything, and a decision to
stop that leans on it would be the exact error this round exists to catch: deciding on an untested
premise.

**"The two classes don't overlap at all."** Partly refuted. Events (1 h) and the terminal object as
it was are genuinely absent from a log store. The residue is real — it is just far smaller than the
pitch.

## 3. What was measured rather than argued

**The headline artifact is present about seventy per cent of the time.** Two documents contradict
each other about it: `DESIGN.md:158-160` says a fast crash loop had its previous-instance logs
*"garbage-collected 2–3 s after the crash"*; `docs/design-trigger-reachability.md:72-73` says *"the
kubelet keeps one dead instance, so `--previous` is there at alert time."*

Probed on kind 1.37 — a fast crash loop (exit immediately) and a slow one (100 s then exit),
sampled from t+0 to t+18 min, then ten consecutive samples at 20 s:

| | result |
|---|---|
| `--previous` returns log content | **7 of 10**, and both pods had content at t+15 min |
| `--previous` returns `unable to retrieve container logs for containerd://…` | **3 of 10** |

So **neither document is right**. The log is not GC'd seconds after the crash, and it is not
reliably there either: at an arbitrary moment there is roughly a 30% chance the previous instance is
unreadable, around the restart. And by `spec/IEB-SPEC.md:278-281` a bundle that captured in that
window is **not** PARTIAL — it verifies `OK` at 100% coverage with its headline evidence missing.

That is the single most actionable finding in this round, and it is a product defect rather than a
market one — **but a smaller one than the lens claimed**. Checked rather than assumed: the producer
already records the reason in `logs/index.json`'s `unavailable`, `Summary` already reads it as
`LastWords::Discarded`, and `lapilli postmortem` already prints *"**not in the bundle** — the kubelet
had already discarded it when Lapilli asked"*. What verifies `OK`/100% without comment is
`lapilli verify`'s one-line verdict, and that line is an integrity check. The documents were wrong;
the code was already honest.

**Found one layer out, when the E2E was run for the first time with `SKIP` unset: the same error was
in the gate.** `test/e2e/run.sh` asserted the previous instance's log *content* unconditionally
(`grep -q "FATAL: cache warmup failed"`), and `test/e2e/notify.sh` asserted only the
`LastWords::Captured` status string. Both are true 7 times in 10. The E2E is one of the **thirteen
required checks** on `main`, so for about three runs in ten a merge failed for a reason that is not
a defect — and the run that exposed it did exactly that: the bundle correctly reported *"kubelet had
already discarded its logs"* and the assertion failed anyway.

Both now gate the property that **is** invariant: the bundle either carries the last words or says
the kubelet discarded them, and **silence is the only forbidden outcome**. That is the same shape as
the finding above — the documents were wrong, then the tests were wrong, and the code was already
honest in both cases. The lesson is narrower than "write better tests": an assertion on a
probabilistic artifact must gate the *reporting*, not the artifact, or it converts a measured 30%
into a flaky required check, which teaches a maintainer to re-run a gate instead of reading it.
`ROADMAP.md` item 0 records four occasions when a red CI went unread; a check that cries wolf three
times in ten is how that habit is trained.

## 4. The survival paths, and which of them are real

Two independent lenses converged on the same shape — *portability is non-optional only where the
evidence must cross a trust boundary* — so it was investigated with a brief that said returning
"both are dead" was a valid answer.

| candidate | verdict |
|---|---|
| **ISV support across an unreachable boundary** | **Real, recurring, paid — and owned.** Replicated Troubleshoot holds the workflow with the same content including `-previous.log`, redaction, `sbctl` replay, a vendor portal, and `schedule --cron` + `--auto-upload` already shipped; Red Hat ships alert-triggered gathering in every OpenShift cluster. Survivable only as a trigger and attestation layer for a format that already has distribution — not as a rival format |
| **An incident in front of an LLM without production credentials** | **Dead.** The thing people refuse is *write* access; a read-only snapshot was already conceded. The remedy exists and was abandoned (`support-bundle-mcp-server`, 0 stars), and `sbctl` has made it possible for four years without ever mentioning AI |
| **SLA credit / regulatory evidence** | **Real burden, dead purchase.** EKS, DORA and the US banking rule do compel cross-boundary reporting — and Atlassian, GKE, Cloudflare and AWS have made the vendor's own telemetry the sole source of truth, so a customer-held bundle has no standing in the only dispute that pays |

The null result is worth as much as the two kills: **what stalls an incident review is culture,
incentives and time — not evidence availability or portability.**

## 5. The one open door, and it is inside the product

The number that argues hardest against the project is **0 of 41**. Decomposed, it is
**0 accept / 17 drop / 24 unknown**. The unknown mass exists because Lapilli *infers* its target
from a `pod` label: `webhook.rs:312` drops any alert without one. Red Hat's four-year-old answer to
the same problem is to let the rule author **declare** the target instead.

Lapilli already reads a project-specific label from the alert (`lapilli.dev/export`,
`webhook.rs:421`), so the mechanism is in the codebase and the change is where the target comes
from, not what the target is. **It does not touch the identity sentence**: still triggered by an
operational signal, still correlating Kubernetes-native state, still sealing one portable file.
`incident.target` is frozen in `ieb/v1` and stays frozen — filling it from a declared label rather
than an inferred one is not a format change.

**Coverage is therefore a product decision, not a physical limit** — which is the opposite of what
`ROADMAP.md` concluded when it wrote that round 29 F4 is *"not fixed and not going to be, on this
path."*

## 5b. Found twice, by two paths that did not know about each other

The self lens, reading the identity sentence clause by clause, reported that *"correlates … across
the incident window"* is a word `DESIGN.md:108-110` already declines to defend — *"This is **timing +
completeness**, not a claim of deep 'correlation'"*. It escalated the wording as an owner's question
rather than applying anything.

Independently, the owner asked a question from the opposite end: if analysis is what Mimir, Loki and
Tempo exist for — label sets, `trace_id`, `span_id` — does sealing a file not make analysis *harder*?

Checked against the format rather than argued: **`trace`, `span`, `correlation` and `request_id`
appear nowhere in `spec/IEB-SPEC.md`.** A real bundle unpacks to `manifest.json`, `redaction.json`,
`logs/index.json` and a plain stdout tail. There are no stream labels and no join keys. The bundle
is one pod's window, and cross-service analysis is not something it can do.

So the same gap was reached twice: once by auditing the project's own sentence, once by asking what
the artifact is shaped for. The two readings agree, which is the strongest evidence this round
produced for anything.

**Applied, in scope:** `DESIGN.md` §4 now states it where a reader of the format meets it — no
correlation identifiers, one pod's window, that work belongs to the observability stack — together
with the two consequences. One is useful: a `trace_id` the workload printed *does* survive into the
bundle, so a bundle can be an entry point into a trace store (at the cost of the credential-free
property, since the store must be alive). The other is a real cost: **sealing gets easier and
cross-service analysis gets harder**, because the evidence has been cut out of the system that made
it joinable. A bundle answers *what this pod's state was*; it does not answer *why that request
failed*, and most postmortem questions are the second kind.

**Escalated, out of scope:** the identity sentence's own use of "correlates". Changing it is the
owner's, not a round's.

**The owner's answer, recorded here because the escalation was recorded here:** change the word.
The identity sentence now reads *gathers Kubernetes-native state across the incident window*, which
is what §3 already said it meant — "timing + completeness". The same substitution was made in all
four places the sentence is published (`DESIGN.md`, `README.md`, `site/index.md`, `ROADMAP.md`),
because three of them carried the word with none of §3's caveat. Two nearby uses went with it:
§2's *"time-window-correlated snapshot"* is now *"time-window snapshot"*, and the landscape row
that used "no window correlation" as a differentiator against troubleshoot.sh now says what it
actually means — collects on demand rather than across an incident window. The word was a claim the
document declined to defend two sections after making it; that is a defect whether or not anyone
had complained.

## 6. Dispositions

| # | Finding | Disposition |
|---|---|---|
| 1 | `README.md` claims logs/metrics are gone; they are not | **APPLY** — claim only events and the terminal object |
| 2 | Two documents contradict each other on `--previous` at alert time | **APPLY** — both replaced by the measurement above |
| 3 | A bundle missing its headline evidence verifies `OK`/100% | **PARTLY REFUTED on checking.** `lapilli postmortem` already says it, in bold: *"**not in the bundle** — the kubelet had already discarded it when Lapilli asked"* (`postmortem.rs:714`), fed by `LastWords::Discarded`, which `summary.rs:236` sets from the `unavailable` field the producer already writes. The remaining gap is `lapilli verify`'s one-line verdict, which is an integrity check rather than an evidence inventory and is the one surface carrying a compatibility commitment. Logged, not applied |
| 4 | Nothing measures evidence-present rate or read rate | **APPLY** — the two counters round 29 F4 already owed |
| 5 | Coverage is self-inflicted: declare the target instead of inferring it | **APPLY, and it is the test** — re-measure against the 155 rules; if the number does not move, that is the pause criterion |
| 6 | The capture is not a differentiator; Robusta owns it | **ACCEPTED** — stop selling the capture; `docs/landscape.md` rather than the first screen |
| 7 | ISV / LLM / regulatory survival paths | **REFUTED or OWNED** — none is a direction |
| 8 | Round 29 was sunk cost | **ACCEPTED, recorded** — the stop decision is re-opened by this round rather than deferred to 2027-Q1 |
| 9 | Zero adopters means no demand | **REFUTED** — 4 days public, nobody asked |
| 10 | 34 rounds are evidence of quality | **REFUTED** — independence never achieved; the project's own case should not cite the count |
| 11 | The bundle carries no correlation identifiers, so sealing trades away cross-service analysis | **APPLIED** to `DESIGN.md` §4 (§5b). Reached twice independently — by auditing the identity sentence, and by asking what the artifact is shaped for. The identity sentence's own "correlates" is **escalated**, not applied |

## 7. The fork, and what the owner chose

Three directions were put up, because the surviving objections are mutually exclusive and one of
them would have rewritten the identity sentence, which is out of a round's scope:

- **A — stop now**, moving round 29's pause criterion from 2027-Q1 to today.
- **B — fix the false claims, then test demand**, because the demand test has never been run.
- **C — reposition onto the trust-boundary use**, which needs the identity sentence rewritten.

The owner chose **B**, and C is closed anyway by §4. The order inside B is load-bearing: **the false
claims are fixed before anyone is asked**, because an installer who arrives expecting their logs to
be gone and finds Loki holding them is a worse outcome than not asking at all.

Both halves are now written down. The claim fixes are this branch; the demand test is
[`docs/demand-test.md`](demand-test.md), whose decision rule was fixed **before any answer existed**
for the reason this round exists — a rule invented after the answers arrive is not a rule. It also
makes §4's null result a selectable outcome, so "it is culture and time, not evidence" can come back
as a finding rather than being argued away.

**The stop condition is written down before the work starts**, which is the point of putting it
here: if §5's declared target does not move the coverage number against the same 155 rules, then the
gap was a physical limit after all, and A is the answer rather than another round.

## 8. What this round taught the method

**Point the lens at the product, not at the proposal.** Rounds 31–33 killed three proposals; round
34 inverted the order and measured premises instead, finding four shipped defects. This round
pointed the same inversion at the product itself and found that three of its headline sentences do
not survive. The method works in proportion to how uncomfortable the target is.

**A number that argues against you deserves the same scrutiny as one that argues for you.** The
project had been quoting "0 of 41" against itself for three rounds without decomposing it. Half of
it was its own inference method.

**"Nobody has adopted it" is not a finding on day four.** The strongest case for stopping leaned on
a fact that had not had time to become evidence, and the round is better for having refused it.
