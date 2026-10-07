# Design review — round 38 (live against frozen, measured again with the corrected instrument)

Not a lens round: one measurement. Round 37 §3 compared an agent investigating a live cluster with the
same agent investigating a frozen copy, found 3 of 9 runs passing in each condition, and said the two
were the same. Round 37 §9 then found that the frozen condition had not been the live one — a
`describe` four times as long, a second clock, pods the live cluster did not have — and that the
sentence was weaker than it had been written. `ROADMAP.md` §7 put running it again first. This is
that.

**Part 1 was committed before any run existed**, in the commit that adds this file. Part 2 is
appended when the runs are in, and says so. A rule written after the answers is not a rule
(`docs/demand-test.md`, round 35 §7).

**Independence: not achieved.** Eighth round running. The cases, the grader, the harness, this rule
and the reading of the result share one author; both judges are models.

## Part 1 — fixed before any run

### What is being asked

With the instrument as it now is — the field-selector front, one clock, a read-only `freeze`, a built
environment, unwrapped tool output — **does an agent do on the frozen copy of an incident what it
does on the live one?**

### What is run

| | |
|---|---|
| cases | `s1-shared-cache-exhaustion`, `s2-periodic-saturation`, `s3-node-local-drift` — the answer keys under `cases/`, unchanged since before any agent ran |
| agents | **A.** Claude Code, headless, `--model haiku`, the `claude-code` adapter as it is on `main`. **B.** HolmesGPT 0.42.0 with `gpt-5-mini` (`--temperature 1`, `--pass-env OPENAI_API_KEY`), the `holmes` adapter as it is on `main` |
| conditions | **live** — `lapilli case run --live` against the kind cluster the scenario was just built on. **frozen** — `lapilli case run` against a case frozen from that same cluster by `lapilli case freeze`, after the live runs and before teardown |
| runs | **6** per agent, case and condition: 72 in all, 18 per agent and condition |
| tool | `lapilli-case` built from `main` at the commit Part 2 names; crust-gather v0.17.1; kind v0.33.0 |

Per case, in order: build the scenario (`scenarios/<id>/setup.sh`); the live runs, both agents at
once, each agent's six one after another; freeze (no node logs; for `s2`, the Prometheus inside the
cluster, thirty minutes); tear down; the frozen runs. The frozen copy is of the cluster the live runs
saw, not the sealed case under `cases/`: the question is fidelity, and that needs the same incident.

Each run is one invocation, so that a provider failure can be seen and acted on before the next.

### What is measured, per run

- **Process**, by `lapilli case` itself, no model: whether every decisive evidence item was retrieved
  (`evidence_all`), each item separately, steps, failed steps, entities cited and not observed.
- **Outcome**, by two judges who see blind packets and nothing else, under rule version 0
  (`docs/case-grading.md`), the instructions as `lapilli case packets` prints them: **judge 1**, a
  Claude model started with no other context; **judge 2**, `gpt-5.5` over the API. Each agent thus
  has one judge from another family than its own model. **A run passes if both judges pass it.**
  Each judge's own count and Cohen's κ between them are reported.

### The rules

**R1 — the same, at the resolution this has.** For each agent, pooled over the three cases, and for
the two agents together: the difference between live and frozen in (a) the share of runs that pass
and (b) the share with `evidence_all`, each with a 95% interval for a difference of proportions
(Newcombe's method on Wilson score intervals).

- An interval that **excludes zero** is a difference, and is reported as one: the frozen copy is not
  a stand-in for the live cluster for that agent on that measure.
- An interval that **contains zero** is reported as *not distinguished*, together with its
  half-width. **Nothing narrower than the half-width is claimed.** With 18 runs a side that is
  roughly thirty points; with 36, twenty-odd. "The same" will mean "not shown to differ by more than
  that", and Part 2 will use those words.
- No claim is made per case. Six runs a side cannot carry one, and round 37 §3b is what happens when
  it is tried.

**R2 — the differences round 37 §9 found are gone.** From the transcripts, no judge:

- `kubectl describe pod`: for each case and agent with at least three such steps in each condition,
  the mean output length frozen over live lies between 0.8 and 1.25. It was 3.9.
- No frozen step fails with an error that only the replay can produce: a refusal by the front, a 5xx
  from it, a malformed table. Counted and listed; the rule is zero.
- No answer in the frozen condition gives a date later than the day of its freeze as the time of
  something in the incident. Counted by search and read by eye; the rule is zero.

**R3 — retrieval is still necessary.** Among all judged runs: how many pass without `evidence_all`.
Round 37 found 0 of 9. The rule is zero; every counterexample is read and reported, and one is
enough to say that the process check is not a necessary condition.

**What would count against the instrument**, stated now: R1 excluding zero in the same direction for
both agents; any failure of R2; a guard refusal of something a read-only investigation needs, in
either condition.

### Exclusions, fixed now

- A run that ends without an answer **because the provider failed** — an HTTP 5xx, a 429, a quota
  message in the run's error — is kept, marked, left out of every count, and run once more. Two in a
  row for one agent stop that agent; what it has is reported as it stands.
- A run that ends without an answer for **any other reason** — its time limit, its budget, a refusal
  to answer — stays in every count and fails. No answer is not a right answer.
- Nothing else is excluded, and no run is repeated because of what it said.

### Limits on spending

Agent A, at most US$12 as its own harness reports cost; agent B, US$6; judge 2, US$3. Reaching a
limit stops that part, and Part 2 reports what there is.

### What this cannot show, whatever it finds

That the cases are good — they share an author with the grader. That a strong agent behaves like
these two. That a person would judge as the two models do. That the frozen copy is faithful where
nobody thought to look: R2 checks the differences that are known.

## Part 2 — the result

**Written 2026-10-07, after the runs.** Part 1 above is as it was committed in `a44095c`, before any
run existed; not a word of it was changed. The instruments under `test/round38/` are as committed in
`600cfdd`, which is also the commit the tool was built from. `posthoc.py` beside them was written
afterwards, and everything taken from it below is marked *post hoc*. The records are in
`test/fixtures/case-runs/2026-10-07-round38/`; `internal/grade` recomputes the counts from them.

### In short

- **R1 — not distinguished, at a resolution of 28 points.** The agent that can pass passed 5 of 18
  live and 5 of 18 frozen. The other agent passed nothing in either condition, so it says nothing
  about fidelity.
- **R2 — missed.** By the rule as written, in one cell of four: `describe pod`, 0.76 against a band
  of 0.8 to 1.25. And beyond what the committed search could see: it looked for *errors* only the
  replay produces and found none, while the transcripts hold **answers only the replay gives that are
  not errors** — a sorted event list that comes back empty (13 frozen steps, 0 live), `-o wide`
  without its wide columns (10 of 10 frozen pod listings, 0 of 9 live), a named object listed as
  `NAME AGE`. 33 frozen steps in 19 of the 36 frozen runs; none live. Found by reading, afterwards,
  while this was being written.
- **R3 — held.** No run passed without all the decisive evidence: 0 of 57. Of the 15 that had it, 10
  passed.
- Part 1 said beforehand that any failure of R2 counts against the instrument. It does. **The frozen
  copy is not yet a faithful stand-in for the cluster — and the comparison of outcomes could not see
  that**: what it measured did not move.

### What was run

| | |
|---|---|
| when | 2026-10-07, 09:03:48 to 10:10:21 UTC, one laptop |
| tool | `lapilli-case` `main-600cfdd`; crust-gather v0.17.1; kind v0.33.0, three nodes, Kubernetes v1.37.0 |
| agents | Claude Code 2.1.292 with `--model haiku`; HolmesGPT 0.42.0 with `gpt-5-mini` |
| runs | 72, six per agent, case and condition. All 72 ended with an answer |
| excluded | none: no provider failure, no agent stopped, no spending limit reached |
| judged | all 72 by both judges. Packets drawn with seed `7692343065575330885` |
| spent | agent A US$3.77 of 12, agent B US$1.22 of 6, judge 2 US$1.38 of 3 |

Three things about how it went that Part 1 does not spell out. `run.sh` started a case's frozen runs
and went on to build the next case, so a case's frozen runs overlapped the next case's live ones: up
to four agents at once, not two. One kind cluster carried all three cases, each scenario torn down
before the next was built; a frozen copy holds its own scenario's namespaces and no other's. And the
live runs of a case are spread over the 12 to 16 minutes before its freeze, while the frozen runs
all see the one instant of it and ran within 18 minutes after: the two conditions looked at the same
incident, not at the same moment of it.

Judge 1 was a Claude Code subagent given its instructions and nothing else, in three fresh contexts
of 24 packets each (the call pinned no model; the session ran Claude Opus 5.5). Judge 2 was
`gpt-5.5`, one call a packet; three of its replies were not valid JSON and were asked for again.
What each was told is kept with the verdicts.

### Counts

| agent | condition | runs | pass (both judges) | judge 1 | judge 2 | all decisive evidence retrieved | mean steps | failed steps | cited, not observed |
|---|---|---|---|---|---|---|---|---|---|
| A, Claude Code | live | 18 | 5 | 5 | 6 | 7 | 20.2 | 45 | 0 |
| A, Claude Code | frozen | 18 | 5 | 5 | 7 | 8 | 20.9 | 51 | 0 |
| B, HolmesGPT | live | 18 | 0 | 0 | 0 | 0 | 12.6 | 11 | 0 |
| B, HolmesGPT | frozen | 18 | 0 | 0 | 0 | 0 | 12.4 | 8 | 0 |
| both | live | 36 | 5 | 5 | 6 | 7 | 16.4 | 56 | 0 |
| both | frozen | 36 | 5 | 5 | 7 | 8 | 16.7 | 59 | 0 |

### R1 — the same, at the resolution this has

Live minus frozen, in points, with Newcombe's 95% interval.

| agent | measure | live | frozen | difference | interval | half-width | reading |
|---|---|---|---|---|---|---|---|
| A | pass | 5/18 | 5/18 | 0 | −28 to +28 | 28 | not distinguished |
| A | all decisive evidence | 7/18 | 8/18 | −6 | −34 to +24 | 29 | not distinguished |
| B | pass | 0/18 | 0/18 | 0 | −18 to +18 | 18 | not distinguished |
| B | all decisive evidence | 0/18 | 0/18 | 0 | −18 to +18 | 18 | not distinguished |
| both | pass | 5/36 | 5/36 | 0 | −17 to +17 | 17 | not distinguished |
| both | all decisive evidence | 7/36 | 8/36 | −3 | −21 to +16 | 19 | not distinguished |

No interval excludes zero. In the words fixed beforehand: for agent A, the frozen copy was **not
shown to differ from the live cluster by more than 28 points in the share of runs that pass, or by
more than 29 in the share that retrieve all the decisive evidence**. Nothing narrower is claimed.

Two of those six rows should not be leaned on. Agent B's intervals are what the method returns for
zero of eighteen on both sides; an agent that never passes cannot pass less often on a frozen copy,
and its rows carry no information about fidelity. The pooled rows are narrower than A's only because
half of their runs are B's zeros. **The number to carry out of R1 is A's, 28 points**, from one
agent and one small model.

### R2 — the differences round 37 §9 found are gone

**`kubectl describe pod`, as the rule was written.** Mean output length, frozen over live, for each
case and agent with at least three such steps in each condition:

| case | agent | live: steps, mean bytes | frozen: steps, mean bytes | frozen / live | within 0.8–1.25 |
|---|---|---|---|---|---|
| s1 | A | 6, 2,284 | 10, 2,803 | 1.23 | yes |
| s1 | B | 7, 2,795 | 2, 2,790 | — | too few steps to rule |
| s2 | A | 4, 2,609 | 11, 2,214 | 0.85 | yes |
| s3 | A | 7, 3,073 | 16, 2,350 | 0.76 | **no** |
| s3 | B | 8, 3,485 | 16, 3,446 | 0.99 | yes |

Four cells could be ruled; one is outside the band. **The rule is missed in that cell.** The 3.9 of
round 37 is gone — the largest ratio here is 1.23 — but the rule was a band, and 0.76 is outside it.

*Post hoc*, and unable to turn that miss into a pass: of the 16 frozen steps in that cell, 5 piped
`describe` through `grep` or `head` and average 81 bytes; of the 7 live steps, one did. The steps
that describe one named pod and print all of it average 3,405 bytes live and 3,381 frozen. Across
all cells, 13 commands were typed character for character the same in a live and in a frozen run of
the same case and agent; frozen over live lies between 0.996 and 1.004 for every one, the remainder
being the ages kubectl counts from the wall clock. So what moved that mean was what the runs chose
to type, not what the replay returned. That makes it the wrong rule to have written: a mean over
whatever each condition happened to describe measures the agent's choices together with the
replay's answers. The right one — the same command, both conditions — would have had to be written
down beforehand to count, and was not.

**Errors only the replay can produce.** The search committed with the rule found none in either
condition, and no step was refused by the guard in either. Of the 59 frozen steps that failed, 43
were refused by the agent's own harness (45 live), 10 were `kubectl top` with no metrics API to ask
(9 live: the kind cluster has none either), 2 were metrics queries that matched nothing (2 live),
and four are left, none of them the replay's: two flags kubectl does not have (`top pod
--all-containers`, `events --field-selector`), a shell glob that matched no file, and a `NotFound`
for a pod name the agent had assembled from one pod's ReplicaSet and another pod's suffix.

**Dates after the freeze in a frozen answer.** None in 36 answers.

**What that search could not see.** *Post hoc.* The rule's own words include "a malformed table".
The search could find error text and nothing else, and these are not errors — the step succeeds,
and says what a live cluster does not:

| what the step shows | live steps | live runs | frozen steps | frozen runs |
|---|---|---|---|---|
| `kubectl get … --sort-by=<a field outside metadata>` answering "No resources found" | 0 | 0/36 | 13 | 11/36 |
| `kubectl get pods … -o wide` with no IP and no NODE column | 0 | 0/36 | 10 | 9/36 |
| a listing whose whole header is `NAME AGE` | 0 | 0/36 | 10 | 8/36 |
| an event listing headed `LASTTIMESTAMP` where a cluster prints `LAST SEEN` | 0 | 0/36 | 1 | 1/36 |
| **any of them** | 0 | 0/36 | 33 | 19/36 |

Live, the same commands did what they do: all 18 sorted listings were answered, and all 9 wide pod
listings carried their NODE column. Read against the rule rather than against the search that was
committed with it, **the count is 33, and the rule was zero.**

The cause was reproduced on this round's frozen `s1`, served again. To sort, kubectl asks the server
for a table whose rows carry the whole object (`includeObject=Object`) and sorts by a path into it.
The snapshot server returns rows carrying metadata only: a sort on `.metadata.creationTimestamp`
works, a sort on `.lastTimestamp` — how both agents ask for recent events — finds the path in no
row, and kubectl prints "No resources found". The server's tables for pods and Deployments have no
wide columns; for ReplicaSets, Endpoints and EndpointSlices they are a name and an age; its event
table puts the event's own name where a cluster puts `pod/<name>`; and a request for one object by
name is answered with the object instead of a table, which kubectl prints as `NAME AGE` — for a
pod, a Deployment, a Service or a node alike.

This is the family round 37 §9 found twice, in field selectors and in `--tail`: the snapshot server
accepts a request it does not implement, and the answer is well formed and wrong. Round 37's two
were found by a reviewer reading code. These were found by reading 72 transcripts for a different
reason. Neither is a method.

What it did to the runs cannot be read off eighteen a side. Of agent A's frozen runs, the nine given
an empty sorted list passed 3 times and the nine not given one twice. A typed `describe pod` 37
times frozen against 17 live — the one difference that stands out among sixteen comparisons of what
the agents typed (p = 0.013, uncorrected; with sixteen looks and nothing going on, one that small
turns up about one time in five). Its eight frozen runs that were shown a wide pod listing with no
NODE column described 2.9 pods a run, its other ten 1.4, its live runs 0.9: what an agent does when
the listing no longer says where a pod runs. That is a reading made afterwards of numbers that small,
and it is offered as one.

**As the agents used what round 37 §9 repaired.** `kubectl logs --tail=N` alone on a line: 54 live
steps and 73 frozen, none returning more than N lines. `--field-selector` was typed three times and
reached the replay in none of them — once live, where it was answered; twice frozen, where kubectl
rejected the flag once and agent B's own harness refused the command once. The filter was exercised
only through `describe`, which is where like for like comes out at 1.00.

### R3 — retrieval is still necessary

| all decisive evidence retrieved | runs | pass (both judges) |
|---|---|---|
| yes | 15 | 10 |
| no | 57 | 0 |

**Held: 0 of 57.** Round 37 had 0 of 9.

It held because a pass needs both judges. Judge 1 alone passes none of the 57. Judge 2 alone passes
three, and they are the three packets on which the judges disagree.

### The two judges

| packets | agree | Cohen's κ | judge 1 passes | judge 2 passes |
|---|---|---|---|---|
| agent A's, 36 | 33 (92%) | 0.81 | 10 | 13 |
| agent B's, 36 | 36 (100%) | undefined: both failed all 36 | 0 | 0 |
| all 72 | 69 (96%) | 0.85 | 10 | 13 |

The 0.85 is helped by 36 packets on which there was nothing to disagree about; the figure that says
something is 0.81 over A's.

The three disagreements are one disagreement three times. All are `s2`, all on the third statement
of its key — that the caller's share is "supported by a number taken from metrics". In each, the
answer gives a share near 90% worked out from configured rates and a capacity model. Judge 1 failed
all three for that; judge 2 passed all three on the number. In all three the process check had
already recorded that the metrics evidence was never retrieved. So on the one point where the two
models differ, the deterministic half of the grade sides with the stricter, and "both judges" is
what kept R3 at zero. Two models agreeing is still two models (`ROADMAP.md` §7).

### By case — reported, not claimed

Pass · all decisive evidence retrieved, of six.

| case | agent | live | frozen |
|---|---|---|---|
| s1-shared-cache-exhaustion | A | 3 · 3 | 1 · 4 |
| s1-shared-cache-exhaustion | B | 0 · 0 | 0 · 0 |
| s2-periodic-saturation | A | 2 · 4 | 3 · 3 |
| s2-periodic-saturation | B | 0 · 0 | 0 · 0 |
| s3-node-local-drift | A | 0 · 0 | 1 · 1 |
| s3-node-local-drift | B | 0 · 0 | 0 · 0 |

Six a side carries no claim, as Part 1 said.

### What else the transcripts show

*Post hoc*, all of it.

- **Agent B fails in one place, and it is the same place in both conditions.** It retrieved the
  evidence that is in the logs and metrics of the workloads the question names about as often as A
  did (44 of 48 against 43), and none of the four items that have to be gone and looked for — two
  settings, a line of code in a ConfigMap, a node agent's log — in any of 36 runs. Both judges
  found the first statement of each key in 35 of its 36 answers and the second in none. An agent at
  the floor cannot show a difference between conditions; a comparison needs one that sometimes
  passes, and this round had one.
- **`s3` is close to out of reach for both.** One run of 24 retrieved `max_open_files=64`, from a
  node agent's log in another namespace. In 20 of 24 answers — five of six in every cell — the
  rollout was named as the cause, which the key forbids.
- **The harnesses refused more than the guard would have.** The guard refused nothing because
  nothing it would refuse reached it: Claude Code, under the permissions its adapter gives it,
  refused 74 steps itself (36 live, 38 frozen), HolmesGPT 14. Most are what cannot be allowed —
  `exec`, `debug`, `port-forward`, `run`. Twelve are `kubectl rollout history`, six in each
  condition, **which the guard allows and the adapter's narrower list does not**. Not a difference
  between conditions, and not a guard refusal; a read-only question an investigation asked and was
  not answered all the same.
- **Time.** A run of A took 74 seconds live and 80 frozen; of B, 134 and 128.

### What can be said now

Round 37 §3 said an agent does the same on a frozen copy as on the live cluster, from three runs a
side on an instrument later found faulty. With that instrument repaired, two agents and two judges:

> On three synthetic cases, for the one agent that sometimes passes, a frozen copy was not shown to
> differ from the live cluster by more than 28 points in pass rate. The differences found in round
> 37 are gone where they were looked for. The same reading found more of the same kind: on a frozen
> copy a sorted listing can come back empty, and tables lack columns a cluster prints. In half the
> frozen runs an agent was shown at least one such answer.

The first sentence is weaker for the third. A comparison that does not move while the thing compared
is visibly different is a comparison with little power, and 28 points is a wide door. What was
measured is that **the differences did not cost passes that eighteen runs could detect**, not that
there were none.

### What this cannot show

Part 1's list stands: the cases share an author with the grader; a strong agent may behave
otherwise; no person judged; fidelity was checked where someone thought to look, and this round is
the second time that has not been enough. To which the runs add:

- **Ages.** A case's frozen runs began within a minute of its freeze and the last ended within
  eighteen. What a case replayed a month later looks like to an agent — pods a month old
  (`design-case.md` §8) — was not tested.
- **The same moment.** Live runs saw a cluster that kept changing for a quarter of an hour; frozen
  runs saw its last instant.
- **The adapters.** Each agent works under a list of what it may run, and that list shaped both
  conditions alike.

### What follows

In `ROADMAP.md` §7, in this order. Repair the replay's tables and sorted listings, and bring the
Claude Code adapter's list up to what the guard allows. Then stop finding these one at a time: the
72 transcripts hold every command two agents thought to type, and **the same command against a
cluster and against its frozen copy, output compared**, is a test that needs no judge, no model and
no reading. It should have come before this round and not after.
