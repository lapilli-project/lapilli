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

*Not written yet.*
