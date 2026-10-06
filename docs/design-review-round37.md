# Design review — round 37 (the pivot that was refuted, the identity nobody uses, and a second line the owner opened)

Not a lens round. This is the record of the questions asked on 2026-10-05 and 2026-10-06, each with a
rule fixed before its result, ending in a decision that was the owner's and in code that now sits
beside the recorder. Round 36 earned the rule this page exists for: *a
claim that is spoken and acted on but never written is unreviewable.* Everything below was acted on
before it was written.

Snapshot: **e6c4edb** (`main`), plus the branch this page is committed on.

**Independence: not achieved.** Seventh round running. Every measurement was designed, run and read
by a Claude-family assistant; the outside inputs are primary texts, public repositories, the outputs
of tools, and the owner's decisions. The outcome judge in §3 was a model from the same family as the
agent it graded.

**The goal order moved, and the owner moved it.** Rounds 27–36 pinned *"first a CNCF Sandbox listing,
second adoption that could be sold."* On 2026-10-06 the owner said, of the gate audit in §1b: the
gate's numbers can be met at any time; what matters is that someone uses the thing — a listing for
something nobody uses is worth nothing. That is recorded here as said. `ROADMAP.md` §0 and
`DESIGN.md` §9 still carry the old order, because rewriting them is the owner's (§7).

## Verdict

- The **compliance pivot is refuted** on primary text and on the enforcement record (§1a).
- The **Sandbox gate does not ask for a sector**; it asks for people and organisations, and the
  profile `ROADMAP.md` aims at is below the lowest one observed to pass (§1b).
- **No identity on offer is used by anyone**, including the current one; four candidates were measured
  bottom-up and all four failed the rule (§1c).
- The owner then stopped looking for a gap and **opened a second line**: incidents frozen with their
  answers, replayed for agents that investigate, graded on process. Two experiments did not kill it.
  One of their pre-fixed rules was **missed as written**, and it is reported as a miss (§3).
- It is built, in Go, in this repository, as `lapilli case` (§4–§6). It is not released, and the
  identity sentence does not describe it (§7).

## 1. What closed before the new line

### 1a. The compliance pivot

The proposal: reposition Lapilli as sealed incident evidence for regulated operators (DORA, NIS2).
The morning's assessment of it had rested on sentences quoted from vendor pages as if they were
regulation. They are not in the primary texts. That is the round's first correction, and the
assistant made the error.

Rule, fixed before reading: the pivot survives on an axis only if the primary source shows it.

| axis | survives if | DORA | NIS2 |
|---|---|---|---|
| text | the primary text obliges integrity or verifiability of *incident evidence* | half — RTS (EU) 2024/1774 Art. 22(d), at the level of "secure manner", for detection | no — nothing in the directive; the implementing regulation protects logs |
| recipient | a supervisor or auditor receives the evidence artefact, or asks about its integrity | no — the reporting templates have no evidence field | no — four national templates ask nothing about integrity |
| enforcement | 2025-01 to 2026-10: one sanction or supervisory finding naming log integrity or evidence preservation | no — none found | no — none found |

What the supervisory record does press on is late reports, missing fields, misclassification and the
completeness of the root-cause analysis — the collection half, which is not what a seal adds.

What survives, exactly: *"one way to meet RTS Art. 22(d) for a Kubernetes workload"* is a true mapping
sentence. *"DORA or NIS2 requires sealed evidence"* is false and checkably false. `DESIGN.md` §9 —
*"Audit angle = opportunistic, not promised"* — matched the measurement; the repository was right and
the morning's assessment was the regression.

Limits: two agents collected the record and the decisive citations were re-read from the saved
primary texts at the time. They were not re-verified while writing this page.

### 1b. The Sandbox gate, measured instead of assumed

Source: `cncf/sandbox` — 115 applications created since 2024-07 (57 approved, 50 not, 9 pending),
with the TOC's comments on about 41 substantive non-approvals from 2025-01 to 2026-10 read and
labelled by the assistant.

- Reasons given: community (maintainers, contributors, adopters, "too early") in 31; form in 5;
  identity in 14. **Overlap with a non-CNCF tool was given as a reason in none.** Two projects that
  declared overlap with CNCF projects were approved anyway (#392 HolmesGPT, #417 KubeElasti).
- What separates outcomes among 57 decided applications: signed from a company domain 24 of 37
  approved, from a personal address 2 of 14; adopters listed 19 of 29, not listed 10 of 28.
- **No project of independent-individual origin was approved** in what was read. The lowest approved
  profile found: three maintainers, two from one start-up, adopters listed, an 18-month-old
  repository.
- The TOC checks what is claimed: *"the 2nd maintainer only has 2 commits compared to 111"*, *"3
  adopters are listed, however, only 1 adopter is verifiable"* (#426).
- Sector is not a substitute. Of thirteen approvals in 2026-09, eleven were AI-adjacent, all with
  large-vendor backing; one- and two-person projects in the same space were deferred for community
  reasons in the same period.

So a change of sector or identity is not what the gate asks for and would not change its answer.
`ROADMAP.md` §2 item 4 — a second maintainer and two or three adopters — is below the lowest profile
observed to pass. The repository was created on 2026-09-17; the application's own pre-check asks for
six months, which is 2027-03-17 at the earliest.

Limits: an e-mail domain approximates an organisation; the labels are the assistant's; the TOC's
closed sessions are not visible.

### 1c. The identity, measured bottom-up

The owner's objection to §1b is what made this measurement happen. Eight earlier searches had gone
top-down — is there a gap, is there a mandate. This one counted what people already do by hand, in
public issue trackers. No one was interviewed: the owner discarded the demand test on 2026-10-05
(§7).

Rule, fixed first — all four needed: the work is frequent (thousands); someone who asks for the
evidence exists structurally and cannot reach the cluster; *"the evidence is already gone"* recurs
unprompted; no default tool covers it. If every candidate fails, the answer is not another pivot.

| candidate | frequent | someone asks | loss recurs | unoccupied | |
|---|---|---|---|---|---|
| today's: a sealed bundle for one's own postmortem | — | no | no | — | fails |
| the help boundary: evidence handed to an upstream maintainer or a vendor's support | **no, at the point that differs** | yes | yes | yes | fails, 3 of 4 |
| CI failure capture | yes | yes | — | no | fails — default practice covers it |
| archiving short-lived objects | not measured | | | | a different product |

The help boundary is the only place where someone *asks*: issue templates in Cilium, Longhorn, Istio
and Velero demand a bundle, and handing one over is daily work (1,990 issues with a Cilium sysdump
attached; 1,149, 816, 767 and 724 for four other projects). The loss is real and unprompted —
*"this cluster has no log aggregation … so the original crash/hang logs are unfortunately gone"*
(longhorn#13717). But the case Lapilli alone covers, evidence gone *before* a person could collect
it, is rare: in a random 40 of the issues labelled as lacking information, loss was the cause in
none to two. And where it was lost, the operator's own restart or reinstall did it in nine or more of
fifteen, not a TTL.

So the thing only Lapilli does is real and small wherever it is measured, and changing sector does
not change that.

## 2. The second line

The owner, after §1c: this is not about finding a gap; a new world has to be opened. And a direction:
merge the logic of a talk published the day before, in which the Toss Securities SRE team describes
root-cause analysis as *notice → collect → find the specificity → hypothesise → verify*, with the
hypothesis written as a proposition plus the evidence it needs, and the first hypothesis usually
wrong.

Three ways to merge it were weighed against four conditions (the work is frequent; no open-source
project has it at source level; an existing tool cannot absorb it as one feature; it is a product
and not a component):

| candidate | verdict |
|---|---|
| automating the specificity — failing population against healthy | a good feature, not a line: an enricher in Robusta, one command for an agent |
| threshold-triggered deep collection | already someone's: Robusta's profilers, HolmesGPT with Inspektor Gadget, Pyroscope, Parca |
| **the investigation as a replayable, gradable case** | the only one that passed |

Evidence that the work exists: HolmesGPT maintains 275 hand-written scenarios, each deployed to a live
cluster before every run, and a third of its tracker mentions evaluation. AIOpsLab, ITBench and
coroot/rca-lab evaluate on live environments. No shared format exists.

Six ways it could die were written down before any experiment: round 35 §4 had already ruled *a
snapshot in front of an LLM* dead (the consumer is different here — whoever builds the agent, not
whoever runs the cluster — but the warning stands); a frozen incident cannot grade a fix; a narrow
snapshot may not answer an arbitrary question; nobody may share real incidents; the world may be a
few dozen projects; and this was the first candidate in ten the assistant rated well, on the day
after it had rated the compliance pivot well and been wrong.

## 3. Two experiments, and the rule that was missed

One author, one laptop, kind clusters. The harness, the cases, the rubric and the write-up share that
author. kind v0.33.0 (Kubernetes v1.37.0), crust-gather v0.17.1, Prometheus v3.5.0.

### 3a. Easy cases: frozen equals live, and the cases are saturated

HolmesGPT 0.42.0 with `gpt-5.5`, on ten of its own scenarios chosen by a mechanical rule from 275
(Kubernetes-only, a diagnosis question, directory order, 4 easy / 5 medium / 1 hard), run as
published and not redistributed. Set up live, ask twice, freeze, tear down, serve the snapshot, ask
twice.

Rule, fixed first: of the scenarios that pass live, at least 80% pass frozen.

| | live | frozen |
|---|---|---|
| scenarios passed | 10 of 10 | 10 of 10 |
| runs passed (blind judge; an independent keyword check agreed) | 20 of 20 | 20 of 20 |
| tool calls per run | 15.5 | 18.1 |

The rule is met and the experiment has almost no power: nothing failed anywhere, so it shows that
freezing loses nothing *at this difficulty* and, as a side effect, that these scenarios no longer
separate anyone. Cost: US$8.30.

### 3b. Hard cases: the evidence freezes, the agent fails, and the failures differ in kind

Three cases written for the purpose — the ones under `cases/` — each with a wrong first hypothesis
planted and the decisive evidence somewhere different. `expected`, `must_not` and `evidence` were
written before any run. Agent: Claude Code 2.1.289, headless, `--model haiku` (resolved then to
`claude-haiku-4-5-20251001`), read-only tools. **Not the planned agent**: the `gpt-5.5` runs stopped
after three when the provider's credit ran out, and a second provider's free tier allowed one run.
Those four runs are not used.

Rules, fixed first. *Freezability:* the decisive evidence of every case is in the frozen copy, and of
the cases that pass live (one of three runs), at least 80% pass frozen. *Process:* does a
deterministic check over the transcript say anything the final answer does not.

| case | live | frozen | retrieved all decisive evidence (live / frozen) |
|---|---|---|---|
| s1-shared-cache-exhaustion | 1 of 3 | 1 of 3 | 2 / 2 |
| s2-periodic-saturation | 1 of 3 | 2 of 3 | 1 / 3 |
| s3-node-local-drift | 1 of 3 | 0 of 3 | 1 / 0 |
| **all** | **3 of 9** | **3 of 9** | |

**The freezability rule was missed as written.** Two of the three live-passing cases pass frozen: 67%,
under the 80% that was fixed. Checked afterwards, in this order: the evidence is reachable from the
frozen copy with read-only commands (8 of 8 items); none of the three frozen s3 runs read the node
agent's log, and neither did two of the three live ones; five more runs per condition on s3 gave
0 of 5 and 0 of 5. So the miss is a case this agent fails almost always in both conditions, measured
with three runs. The rule was written for cases an agent passes and is the wrong instrument for one
it fails — which is a finding about the rule. It is not rewritten after the fact.

**What the process check adds**, across the 18 judged runs:

| retrieved all decisive evidence | runs | passed |
|---|---|---|
| yes | 9 | 6 |
| no | 9 | 0 |

Necessary, not sufficient. And the failures sort into kinds the transcript distinguishes and the
answer alone does not: *never looked* (s3, five of six runs); *looked and did not conclude* (s1, one
live run with all three items retrieved); *did not look and supplied a mechanism* (s3, four of six:
"version 1.4.2 introduced a new validation", which appears in no tool output); *right number, wrong
source* (s2, two live runs computing the caller's share from configuration). No run cited a pod or an
address it had not seen, 0 in 28 — the entity check caught nothing here, and the invented mechanism
was caught by the judge's `must_not`.

**Metrics freeze, with a clock.** The same per-caller query returned 5,902 / 728 / 435 / 171 live
and 5,902 / 729 / 435 / 171 frozen, and still 5,902 ninety minutes later.

Cost: about US$2.00 for the 18 judged runs and US$0.93 for the ten extra.

A correction made the same day: the first sealed copy of s2 carried a lock file, a WAL and a head
beside the frozen block, left by the Prometheus that had served it. No query result changed. It was
found by opening the store with Prometheus's own libraries, which is also how §5 began.

**Established**, on three synthetic cases and one small model: decisive evidence of three kinds can
be frozen and reached; the run-level pass rate was the same live and frozen; the cases have headroom;
a model-free check over the transcript predicts failure and separates kinds of failure. **Not
established**: any of it for a strong agent, for real incidents, for a judge that is a person, or for
a case written by someone unconnected to the grader.

## 4. What building it measured

The prototype was 797 lines of Python. The port is where the following were found; each is a tool
output, and the tests named are in the tree.

- **The engine agrees with Prometheus to the last digit.** Six queries over the frozen samples
  against Prometheus 3.5.0's own TSDB reader on the block they came from — for example
  `5902.174954327807`, `729.0744049169664`, `434.6331352594811`, `171.45300673552757` per caller —
  and the same digits again after moving the engine ten minor versions, v0.305.0 to v0.315.0.
  `TestTheSealedCaseAnswersAsItsPrometheusDid` pins them, and
  `internal/metrics/testdata/reference/` keeps the block and the reader so that the comparison can be
  run again.
- **A millisecond matters.** Evaluated one millisecond later, one of those answers moves in its eighth
  significant digit (171.45300673… becomes 171.45301784…). Prometheus rounds a request's time to the
  millisecond; a first version here truncated it, and now rounds, with a test on the rounding.
- **Remote read returns what is stored.** An export from a Prometheus 3.5.0 serving the pristine block
  gave 15 series, 2,064 samples and 3 staleness markers, identical byte for byte, once decompressed,
  to the dump taken through the TSDB reader. An earlier JSON dump had lost every series containing a
  marker, because JSON has no NaN.
- **"The same bytes every time" was false as first written.** The same store compresses to 3,151
  bytes under Go 1.25.1 and 3,077 under Go 1.27.1. The claim in the code now says one build, and
  `case-format.md` says what identifies a case.
- **The prototype's process check had a hole**, and a mutation found it: fourteen deliberate defects
  were introduced one at a time, twelve failed a test, two did not. One survivor was that searching
  the agent's *inputs* as well as its outputs changed no test — and it should not have been searching
  them, because typing a guess is not observing it (`case-grading.md`). The other was an untested
  boundary of the pod-name pattern. Both are pinned now. All 28 recorded runs grade identically under
  the old reading and the new one.
- **The port grades as the prototype did.** `TestCheckReproducesEveryRecordedGrade` recomputes every
  process check from the 28 stored transcripts and requires the stored result — which is also the test
  of whether Go's regular expressions differ from Python's on this data. They do not.
- **The guard had a way round.** The prototype's wrapper compared `KUBECONFIG` and nothing else, so
  `kubectl get pods --kubeconfig ~/.kube/config` would have passed an allow-list of `kubectl get *`.
  Nothing in the recorded runs did that. The wrapper now refuses the flags, and the scenario scripts,
  which called bare `kubectl`, are pinned to a named kind cluster and refuse anything else.
- **The dependency was already vulnerable.** The Prometheus module the spike had chosen, v0.305.0,
  carried four advisories and `golang.org/x/text` a fifth (`govulncheck`: GO-2026-5264, -5381, -5662,
  -5710, -5970). At v0.315.0 the scan is clean. The price is §5's last row.
- **End to end, once each.** `freeze` against a live kind cluster rebuilt from `scenarios/s1…`: the
  scenario came up in 60 s, all three evidence items were found, one Secret was redacted, the result
  verified. `serve` with crust-gather: `kubectl` through the guard, the guard's refusals, `promq`
  returning 5902.17 for the caller. And one agent run through the Go harness on frozen s2 — 29 steps,
  112 s, US$0.15, no refusal by the guard, the metrics evidence *not* retrieved because the agent
  asked three times for two metric names that do not exist. One run: a check that the harness works,
  not a result.

Not exercised: `freeze --metrics-url` against a Prometheus inside a cluster (the export and the
packing were each tested, not the two together on a live cluster); the `holmes` adapter in Go against
a real HolmesGPT; anything on Linux, which is where CI will run it for the first time.

## 5. The language, and a recommendation reversed

The prototype was Python because it was fastest to write in a session. Asked whether that was right,
the assistant recommended **Rust**, inside the existing workspace, on two premises: the integration
points are language-neutral, and Rust is the author's language. Asked to measure, it found both
false.

| | measured |
|---|---|
| peers | CNCF projects: Go 69%, Rust 7%, Python 6% of 229. Sandbox approval by language shows no effect (Go 37 of 62, Rust 7 of 10, Python 3 of 6). **The stores a case freezes are Go, 9 of 9** |
| integration | a 35-line Go program opens a frozen TSDB and evaluates PromQL in process, with the freeze as the evaluation time — no container, no clock proxy. For Rust a PromQL parser was found and no engine over a TSDB reader. crust-gather has a library target, so Rust could embed the Kubernetes half instead. Each language embeds one half |
| one maintainer | the author's Go is 261,360 lines against 30,713 of Rust. What the tool would reuse from the Rust workspace is about 7.5% of it |
| speed | the harness is 1–4% of a run. An agent's median run is 58 s |
| the lean path, v0.305.0 → v0.315.0 | importing `promql`: no cloud SDK at either version. Importing the top-level `tsdb`: 160 cloud-SDK packages at v0.315.0. From v0.311 `promql` compiles 79 packages of the Kubernetes client through a logging helper — 412 packages against 298 |

Recommendation, and what the owner took: **the case tool in Go, the recorder left in Rust**, in one
repository. The cost was named with it: the recorder's redaction engine is Rust, so a case does not
get it (`design-case.md` §7), and writing it twice would put security code in two languages.

## 6. Dispositions

| | |
|---|---|
| `cmd/lapilli-case`, `internal/…` | new: the case tool, in Go |
| `crates/lapilli-cli` | `lapilli case …` hands over to `lapilli-case`; nothing else in the CLI changes |
| `cases/`, `scenarios/`, `test/fixtures/case-runs/` | new: three sealed cases, what rebuilds them, and the 28 recorded runs |
| `.github/workflows/ci.yml` | a fourteenth job, `case-tool`: format, tidy, vet, test, shipped cases verify, no cloud SDK, `govulncheck`. **It is not a required check until the owner adds it to the `main` ruleset** |
| `.github/dependabot.yml` | `gomod` |
| `docs/design-case.md`, `case-format.md`, `case-grading.md` | new |
| `ROADMAP.md` | §7 added; §3 item 8 annotated with the owner's decision on the demand test |
| `DESIGN.md` | **not edited** |
| `release.yml`, `THIRD-PARTY-LICENSES.md`, `RELEASE.md` | not edited: `lapilli-case` is not released |

## 7. For the owner — left undone on purpose

1. **The identity sentence.** `DESIGN.md`'s identity block says Lapilli is *triggered by operational
   signals* and depends on *no AI tool*, and that a round which would redefine the sentence is out of
   scope for that round. The case tool is triggered by a person and exists for agents. Three drafts,
   none applied:
   - *Two tools, one sentence each.* Keep the block as it is, scoped to "the recorder", and add
     beside it: "**Lapilli cases** freeze an incident together with its answer key, replay it with no
     cluster, and grade how an agent investigated — shipping no agent, no model and no judge."
   - *One sentence above both.* "Lapilli turns an incident into a file that can be checked later
     without the cluster it happened on: sealed as evidence for the people who review it, and frozen
     as a case for the agents that are asked to explain it."
   - *The case first.* "Lapilli is where an incident is kept so that it can be asked about again —
     by a reviewer, or by an agent under test." with the recorder described as the in-cluster source
     of such incidents. This one promises a path — recorder output becoming a case — that does not
     exist: a bundle is one pod's window and a case is a whole cluster.
   "No AI tool" survives all three in the sense that matters: Lapilli ships none and grades any.
2. **The goal order** in `ROADMAP.md` §0 and `DESIGN.md` §9, which the owner has restated in
   conversation and not on the page.
3. **`docs/demand-test.md`.** The owner discarded it on 2026-10-05: no interviews, judgement from what
   can be measured without asking. The page now says so at its top and is otherwise as it was.
4. **Governance.** A neutral grader needs one rule most projects do not: a maintainer of a graded
   agent does not decide alone how that agent is graded. `case-grading.md` states that changes are
   versioned and public; whether `GOVERNANCE.md` should say more is a governance change.
5. **The README's opening.** A short section near the end now points at the case tool. The lockup, the
   pitch and *What Lapilli is not* are untouched.
6. **The `main` ruleset**, if `case-tool` is to be required.

## 8. What this round taught the method

**A quotation is a measurement of the page it was copied from.** The compliance assessment rested on
vendor pages quoting a regulation, and the regulation does not say it. The rule this earned is already
in the assistant's instructions: a regulation is quoted after its primary text has been searched for
the words.

**The answer to a question about a gate is not an answer about a product.** §1b was accurate and
beside the point, and it took the owner to say so.

**A rule that is missed is reported as missed, and then examined.** §3b's 67% against 80% is the most
useful number in the experiment, because working out why it happened showed what three runs cannot
distinguish.

**A recommendation is a premise list.** The Rust recommendation fell with two premises nobody had
measured, one of them about the person being advised.

**Mutation finds what review reads past.** The grading hole in §4 survived the prototype, its eleven
tests, a port and a test that compared 28 recorded grades. It fell to changing one line and watching
nothing fail.

**A dependency pinned for an experiment is not a dependency chosen for a product.** v0.305.0 was
picked because a container image of the same version was on the laptop.
