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
`DESIGN.md` §9 carried the old order until the owner had it written down the next day (§10).

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
  identity sentence does not describe it (§7) — did not, until §10.

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

Source: `cncf/sandbox` — the applications created since 2024-07, counted as 57 approved, 50 not and
9 pending (116 issues; one is a test, and which of the three counts holds it was not recorded), with
the TOC's comments on about 41 substantive non-approvals from 2025-01 to 2026-10 read and labelled by
the assistant.

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

The owner's objection to §1b is what made this measurement happen. Eight earlier searches — the seven
recorded in `docs/demand-test.md`, and §1a — had gone top-down: is there a gap, is there a mandate.
This one counted what people already do by hand, in the public issue trackers of twenty projects,
about 126,000 issues. No one was interviewed: the owner discarded the demand test on 2026-10-05
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

HolmesGPT 0.42.0 with `gpt-5.5`, on ten of its own scenarios chosen by a mechanical rule from the 275
under `tests/llm/fixtures/test_ask_holmes` at commit `5e6f345`: tagged `kubernetes`, no external
backend, a diagnosis question, taken in directory order with a quota of 4 easy / 5 medium / 1 hard
(only one eligible scenario was tagged hard). They were `10_image_pull_backoff`,
`13a_pending_node_selector_basic`, `14_pending_resources`, `15_failed_readiness_probe`,
`76_service_discovery_issue`, `77_liveness_probe_misconfiguration`, `78a_missing_cpu_limits`,
`81_service_account_permission_denied`, `82_pod_anti_affinity_conflict` and
`84_network_policy_blocking_traffic`. One, `23_app_error_in_current_logs`, failed its own setup check
and was replaced by the next in line, as decided beforehand. Run as published and not redistributed.
Set up live, ask twice, freeze, tear down, serve the snapshot, ask twice; a condition passes a
scenario if at least one of its two runs passes.

Rule, fixed first: of the scenarios that pass live, at least 80% pass frozen.

| | live | frozen |
|---|---|---|
| scenarios passed | 10 of 10 | 10 of 10 |
| runs passed (blind judge; an independent keyword check agreed) | 20 of 20 | 20 of 20 |
| tool calls per run | 15.5 | 18.1 |
| tool calls that errored | 10 of 340 | 12 of 398 |

A snapshot took 1.1 s and 3.4 MB on average. The rule is met and the experiment has almost no power:
nothing failed anywhere, so it shows that freezing loses nothing *at this difficulty* and, as a side
effect, that these scenarios no longer separate anyone. HolmesGPT's own published results agree
(`docs/development/evaluations/history/results_20260914_200444.md` in its repository: 92%, 90%, 89%,
83% and 81% for five models over 63 tests). Cost: US$8.30.

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

Expected statements conveyed, by the judge's count: 14 of 24 live, 17 of 24 frozen. A decoy was blamed
in `s3` only, in two runs of each condition. Commands the agent's harness refused: 15 live, 18 frozen.
`kubectl top` failed in both conditions; the kind clusters had no metrics API.

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
and 5,902 / 729 / 435 / 171 frozen, and still 5,902 ninety minutes later. In the frozen runs the
agent's metric queries returned data in 3 of 3 runs; in the live runs in 1 of 3 — one run guessed
metric names that do not exist, and one never queried.

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
  `5902.174954327808`, `729.0744049169665`, `434.6331407640582`, `171.4530178466593` per caller —
  and the same digits again after moving the engine ten minor versions, v0.305.0 to v0.315.0. (These
  are the digits at the instant a replay uses. The first pinning was one millisecond off; §9.)
  `TestTheSealedCaseAnswersAsItsPrometheusDid` pins them, and
  `internal/metrics/testdata/reference/` keeps the block and the reader so that the comparison can be
  run again.
- **A millisecond matters.** Evaluated one millisecond apart, seven of ten pinned values differ, the
  furthest in its sixth significant digit (7683.171… against 7683.182… for a thirty-minute increase).
  Prometheus rounds a request's time to the millisecond; a first version here truncated it, and now
  rounds, with a test on the rounding.
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

Not exercised when this section was written: `freeze --metrics-url` against a Prometheus inside a
cluster (the export and the packing were each tested, not the two together on a live cluster); the
`holmes` adapter in Go against a real HolmesGPT; anything on Linux. All three were run the next day,
and running them is how half of §9 was found.

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
| `cases/`, `scenarios/`, `test/fixtures/case-runs/` | new: three sealed cases, what rebuilds them, the 28 recorded runs, and what their judge was told |
| `.github/workflows/ci.yml` | a fourteenth job, `case-tool`: format, tidy, vet, test, shipped cases verify, no cloud SDK, `govulncheck`. Not a required check when it was added; required since §10 |
| `.github/dependabot.yml`, `.gitignore` | `gomod`; the tool's default outputs |
| `docs/design-case.md`, `case-format.md`, `case-grading.md` | new |
| `ROADMAP.md` | the header; a paragraph at the end of §0; a bullet in §1; §3 item 8 annotated with the owner's decision on the demand test; §7 added |
| `README.md`, `CHANGELOG.md`, `CONTRIBUTING.md`, `SECURITY.md` | a section near the end; *Added* under Unreleased; the Go checks and the row on required checks; one scope bullet |
| `docs/demand-test.md` | a note at its top that the owner discarded it; otherwise unchanged |
| `DESIGN.md` | **not edited** (then; the owner had its identity block and §9 rewritten in §10) |
| `release.yml`, `THIRD-PARTY-LICENSES.md`, `RELEASE.md` | not edited: `lapilli-case` is not released |

## 7. For the owner — left undone on purpose

*All six were decided the next day, 3 having been already; §10 says how.*

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

## 9. Second pass, 2026-10-07: what was verified, and what the first pass had wrong

The owner asked for three things the day after: verify everything §4 lists as not exercised, re-check
every document against the code, and re-check the design. So the three paths were run for real, and
the code and the documents were given — with no conclusions attached — to two reviewers that had not
written them: one to check every sentence against the tree, one to attack the design. Both are
Claude-family, so this is still calibration and not independent review. What makes it worth a section
is the count: building it, with its tests and a mutation pass, had found the half-dozen things in §4.
This found twenty-two more, in a day.

### Verified

| | result |
|---|---|
| Linux | the `case-tool` job green on `ubuntu-latest`, and the suite in a `golang:1.27` container on arm64 |
| `freeze --metrics-url` against a Prometheus inside a cluster | `s2` rebuilt on kind, frozen through a port-forward: 15 series, 1,238 samples. Five queries at the freeze instant, asked of the live Prometheus and of the frozen copy: the same value strings, five of five |
| the scenarios, after they were pinned to a named cluster | all three rebuilt and frozen — 60 s, 509 s, 180 s — every decisive evidence item found, `s3`'s through the node agent's log |
| the `holmes` adapter against a real HolmesGPT | 0.42.0 with `gpt-5-mini` on frozen `s2`: 24 steps, 29 model calls, US$0.055, 161 s, its Prometheus toolset working against the frozen store. And again after the fixes below, with an empty home directory and only its model key: 9 steps, US$0.033, the metrics evidence credited |
| the `claude-code` adapter after the fixes | one run on frozen `s2`: 25 steps, US$0.24, nothing refused by the new guard |

One run each: checks that the paths work, not results.

### What was wrong

Most consequential first. *Found by* says which instrument.

**The frozen condition was not the live one, in ways §3 did not list.**

1. **Field selectors are ignored by the snapshot server**, so `kubectl describe` lists every event in
   the namespace: 10.5 KB a `describe pod` in the recorded frozen runs against 2.7 KB live, and the
   four frozen steps the agent's harness truncated were all `describe` — the design page had put
   them under `logs`. Asked
   directly, `--field-selector spec.nodeName=…` returns every pod. *Found by the document audit, then
   measured.* Closed a step later, here and not at its source: a front before the snapshot server
   does the selecting. On a kind cluster, live against frozen, six selector questions gave the same
   lines and `describe pod` the same 2,738 bytes and five events. The same front now honours
   `logs --tail`, which the snapshot server also ignores and 3 of the 49 recorded frozen steps that
   asked for a tail were answered past.
2. **A case had two clocks.** Metrics answers were moved forward to the caller's time; logs and events
   kept theirs. The recorded frozen runs were made within minutes of each freeze, where the gap is too
   small to show. A day later, HolmesGPT read pod logs stamped the 5th, asked `kubectl` about that
   evening, and reported a metric timestamp on the 6th at which nothing in the incident existed. *Found by the first real HolmesGPT run and,
   independently, by the design review.* Fixed: the question is mapped and the data never is
   (`design-case.md` §3).
3. **`freeze` was not a read.** crust-gather's defaults start a pod with host access on every node to
   read the kubelet journal; the three shipped cases carry those journals, the pods and their events,
   and so did every snapshot the frozen runs were served. *Found by the document audit, reading the
   archives; measured on kind.* Fixed for new freezes — off unless `--node-logs`. The three cases are
   as they were, and say so.

**An agent could get out, or was handed more than the case.**

4. **The kubectl guard had two more ways through**: `-s` grouped behind another short flag
   (`kubectl get pods -As https://…`), and `kubectl config set` / `use-context`, which rewrite the one
   kubeconfig the guard trusted. *One each from the two reviewers, both executed against a stand-in
   kubectl.* Fixed by replacing the shell script with a decision in Go: read-only verbs only, and the
   case's destination appended to every invocation, so that a spelling the refusals miss is overridden
   rather than obeyed.
5. **The agent inherited the operator's whole environment** — cloud credentials, tokens for
   observability backends — and its home directory. HolmesGPT switches toolsets on when it finds such
   things. *Found by the design review.* Fixed: the environment is built from a short list plus what
   the operator names, and the home directory is empty.
6. **Secret redaction went by directory name.** A snapshot from another collector, a `List`, or the
   second document of a file would have been packed with its values and `secrets_redacted: 0`. *Found
   by the design review.* Fixed: by content.
7. **Unpacking a case and reading its metrics were unbounded.** *Design review.* Fixed: entry, byte and
   line limits.

**The grading did not mean what the pages said.**

8. **The same query result was graded differently by agent.** HolmesGPT's adapter stored each tool
   result inside its harness's envelope, JSON inside JSON; the metrics evidence of `s2` matched what
   every other agent's tools print and not that. *Found by the first real HolmesGPT run.* Fixed, with
   the shapes of that run as the test.
9. **The judge's rule in the code was not the rule the judge had been given.** The 18 verdicts were
   given under a longer text and came back as one boolean per statement; the code printed a shorter
   one and asked for indexes. *Document audit, from the shape of the verdicts.* Fixed by making the
   code's rule the recorded one, word for word, and keeping what that judge was told beside its
   verdicts.
10. **A partly judged batch was reported out of all its runs**, and a run that died on a provider's
    outage was indistinguishable from a wrong answer. *Design review; an outage happened the same
    day.* Fixed: the outcome is out of the verdicts there are, and such runs have a column.
11. **Packet ids came from a constant seed**, so the key could be rebuilt by anyone with the tool.
    **No record stated its rule version**, which the grading page said every result does. *Document
    audit.* Both fixed.
12. **Metrics evidence was not checked for solvability** — known, and first on the roadmap as it then
    stood. Done: at freeze and in CI, with an optional witness query per item.

**Sentences that were false.**

13. **"The same digits to the last one" was pinned one millisecond from the instant a replay uses.**
    The engine agrees at either instant; seven of the ten pinned values do not. *Document audit, which
    evaluated both.* Re-pinned, and the test now reads the instant from the case.
14. **"Byte for byte what the recorded runs were made against."** The archives were packed after the
    runs, which is when their one Secret was blanked; and they had been packed by the prototype with
    owner names and times in their headers. *Document audit, from the archives' own timestamps.* They
    were packed again with the tool, which writes neither, and `case-format.md` says what was compared
    and what was not.
15. "Secret values never leave the cluster" — they reach the freezing machine and are blanked there.
16. "Label and field selectors work" — see 1.
17. "Three clouds' KMS" — two.
18. "Every published number is recomputed in CI" — five numbers and the 28 process grades are.
19. `CONTRIBUTING.md` still said every `ci` job is required; `case-tool` is not.
20. A file name that does not exist (`redaction_report.json`), a sum that did not add up (§1b), a
    scenario comment promising three bursts where two are guaranteed.

**Smaller.**

21. `promq` printed six significant digits with an exponent, turning two counters a few requests apart
    into one number. Ten digits, plain, now.
22. The label endpoints ignored `match[]`; endpoints with nothing frozen answered with a page a client
    cannot parse; everything after `--` on the tool's own command line but the first word was parsed as
    flags; a failed run was recorded with `"steps": null`.

One claim of the audit did not hold when tested: that the `claude-code` adapter's text filters can
read any local file. Claude Code 2.1.291 refused `head` and `grep` on a file outside its working
directory. The page says what was observed and that it is not this tool's guarantee.

### What this does to §3

The 18 judged runs stand as records: what the agent did and how it was graded are in the fixtures, and
re-grading them gives the stored results. What is weaker than §3 says is the sentence built on them.
"The run-level pass rate was the same live and frozen" compared a live cluster with a copy that
answered `describe` four times as long, stamped its metrics with another clock, and carried pods the
live cluster did not have. Each of those could have moved the frozen number either way, and three runs
per condition cannot say. The experiment has not been re-run with the corrected instrument. It is
first on the roadmap.

### What the second pass taught the method

**Run the real thing once before reasoning about it.** One HolmesGPT run — three minutes, six cents —
found two design defects that the port, its tests, a mutation pass and a design document had not.

**An audit of the documents is an audit of the code.** The sentence "to the last digit" was checked by
evaluating it, and was off by a millisecond. The sentence "objects, events, pod logs" was checked by
opening an archive, and the archive held a privileged pod.

**The author's instruments find the author's kind of mistake.** Mutation found a hole in a check the
author had written. It could not find that the guard needed to exist in a different form, or that
`freeze` did something nobody had asked it to do.

**"Verified" names what was run.** §4 said end to end, once each, and listed what was not. The list
was the honest part and the place the defects were.

## 10. What the owner decided, 2026-10-07

§7 left six things undone on purpose. Asked, the owner said to go ahead, and chose where there was a
choice.

1. **The identity.** Of the three drafts in §7, the second, with the first beneath it: one sentence
   above both tools, and each tool's own. `DESIGN.md` now opens its identity block with

   > Lapilli turns a Kubernetes incident into a file that can be checked later without the cluster it
   > happened on — sealed as evidence for the people who review it, and frozen as a case for the
   > agents asked to explain it.

   followed by the recorder's sentence, word for word as rounds 27–36 checked against it, and by
   *cases freeze an incident together with its answer key, replay it with no cluster, and grade how
   an agent investigated — shipping no agent, no model and no judge.* The third draft was not taken:
   it described the recorder as the source of cases, and nothing turns a bundle into a case.
2. **The goal order** is on the page as the owner said it: first that someone actually uses it,
   second a Sandbox listing, third adoption that could be sold (`DESIGN.md` §9, `ROADMAP.md` §0).
3. **`case-tool` is a required check.** The `main` ruleset lists fourteen.
4. **The prototype is gone.** The directory the Python prototype lived in was never committed
   anywhere. Before it was removed, its run records were compared file by file with
   `test/fixtures/case-runs/` (31 files, identical), its answer keys with `cases/` (identical), and
   the specifics of its experiment record that §3 had left out were carried into §3 — the ten
   scenario names, the counts of tool errors and refused commands, the statements conveyed. Four of
   those were recomputed from the fixtures on the way and agreed.

5. **The pitch**, which had been left open: the identity leads it. `README.md`, the site and
   `DESIGN.md` open with the sentence above both tools, and then name each with the state it is in —
   the recorder released, cases pre-alpha and in no release. Neither tool is put in front of the
   other, and the unreleased one is not made to look like the released one.
6. **Governance.** `GOVERNANCE.md` has a section, *Grading neutrality*: Lapilli ships no agent, no
   model and no judge; the grading rule is public and versioned; and nobody decides alone how their
   own agent is graded. The third needs a second maintainer to mean what it says, and says what
   holds until there is one.
7. **Passed over**: an issue upstream for the field selectors crust-gather ignores. Asked whether it
   matters for developing Lapilli now, the answer was that it does not — the front in §9 made the
   difference ours to carry — and the owner let it be. It is parked in `ROADMAP.md` §7, with what
   would bring it back.

Nothing of §7 is open. What is next is not a decision but a measurement: the comparison §9 weakened,
run again. Its rule is `docs/design-review-round38.md`, fixed before its runs.
