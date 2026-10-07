# Grading a case (rule version 0)

The rule is this page: the process checks and the judge's instructions together. It has a version
(`internal/grade`: `RuleVersion`), a run record carries the version it was graded under, `report`
prints it and refuses to mix runs of different versions, and `packets` prints it with the
instructions. Changing anything here changes every number computed under it, so a change is made in
the open, in this file, and the version moves. Lapilli ships no agent, no model and no judge, and
nobody decides alone how their own agent is graded: `GOVERNANCE.md`, *Grading neutrality*, is the rule.

Version 0 is the first one written down. Nothing has been published under it but the 100 runs below.

## Process checks — deterministic, no model

Computed by `internal/grade.Check` from the transcript alone.

| check | definition |
|---|---|
| `evidence_retrieved` | for each `evidence` pattern, whether it matches the concatenated **output** of the agent's steps |
| `evidence_all` | all of the above |
| `specificity_named` | the case's `specificity` string occurs in the answer (case-insensitive) |
| `decoys_mentioned` | which `decoys` occur in the answer. **Mentioned is not blamed**: ruling a decoy out mentions it |
| `cited_entities` | how many distinct pod names (`<deployment>-<replicaset hash>-<pod suffix>`) and IPv4 addresses the answer contains |
| `ungrounded_entities` | those of them that occur in no step's output |
| `steps`, `failed_steps` | counts; a step is failed when the tool reported an error or was refused |
| `used_metrics` | true when a step's command contains `promq` or its tool's name contains `prometheus` |

**What the agent observed is what its tools returned; what it typed does not count.** The prototype
searched inputs as well, and that was a hole: `grep -c CACHE_CONN_MODE` contains the evidence whatever
it prints, and a query that names `client="catalog-indexer"` matched the metrics evidence of `s2`
before any number came back. Found by mutating the check and seeing no test fail. The 28 runs recorded
at the time grade identically under both readings, and a test now pins the difference.

**An output is the text the agent's model was shown**, not the record an agent's harness wraps it in
([`design-case.md`](design-case.md) §5). The one real HolmesGPT run made so far was credited with the
metrics evidence of `s2` only after its adapter stopped storing that wrapper.

What these checks do **not** establish:

- that the agent understood what it retrieved — three of nine runs retrieved everything and still
  failed, and in round 38 five of fifteen;
- that a mechanism it describes is real — an invented mechanism names no pod and no address;
- where the evidence came from — a pattern is looked for in everything returned, so one enormous
  dump earns the credit, and evidence that belongs to the metrics store is credited if a Kubernetes
  command happens to print matching text;
- anything about output the agent's own harness truncated — it is seen truncated;
- grounding for pods that are not a Deployment's: a StatefulSet's `name-0` or a DaemonSet's
  `name-x7k2p` is not recognised as a pod name, so an invented one is not flagged.

## Outcome — a blind judge

`lapilli case packets <results> <case>...` writes, per run, `{id, question, expected, must_not,
answer}`, shuffled, each with an id drawn from a seed. The mapping back to runs goes to a separate key
file that the judge must not see. With the seed and the list of runs anyone can rebuild that key, so
by default the seed is drawn fresh and printed, to be kept with the key; `--seed` repeats a drawing.

The instructions printed with the packets are the rule:

> Grade each packet independently of every other packet.
>
> 1. For each statement in `expected`: is it clearly conveyed by the answer as a finding? Same meaning
>    counts; wording may differ. A statement that appears only as one unconfirmed possibility among
>    several, or only as a suggestion for further checking, is NOT conveyed. If a statement contains
>    specific values (a number, a file path, a setting, a node or workload name), the answer must give
>    those specifics or unmistakable equivalents — a vaguer version ("some node-level problem",
>    "resource limits") is NOT conveyed.
> 2. For each statement in `must_not`: does the answer do it? Mentioning the item in order to rule it
>    out is NOT doing it. Presenting it as the root cause, as a contributing cause, or as the fix IS
>    doing it.
> 3. Verdict: PASS only if every expected statement is conveyed and no must_not statement is done.
>    Otherwise FAIL.
>
> Do not try to infer anything about how or where an answer was produced; it is irrelevant.

followed by the shape to answer in: per packet a `verdict`, one boolean per `expected` statement, one
per `must_not` statement, and a one-sentence `reason`.

The sentence about specifics — "a value below 200" does not convey "64" — decided several verdicts in
the recorded runs.

`lapilli case report <results> --verdicts <file> --key <file>` shows the two halves side by side and
does not combine them. The outcome is out of the runs that have a verdict; a batch judged in part
says how many were not. Without verdicts the column reads "not judged". A run that ended without an
answer — the agent's limit, or its provider's outage — is counted in its own column and stays in
every denominator: no answer is not a right answer, and an outage should not read as an agent that
cannot investigate.

## What a result must carry to be comparable

Agent and its version, model, the tools the agent was allowed, runs per case, the rule version, who or
what judged, and whether the judge was blind. A pass rate without those is not a result. The run
record carries the agent's name, the model and the rule version; the rest is the publisher's to state.

## The recorded runs

`test/fixtures/case-runs/2026-10-06-hard-cases/` holds 18 judged runs (three cases, live and frozen,
three runs each: Claude Code 2.1.289 headless, `--model haiku`, read-only tools) with the packets the
judge saw, the verdicts and the key. `2026-10-06-s3-extra/` holds ten more runs of one case, not
judged. Transcripts are the agent's recorded steps with local file paths removed.

They were made by the prototype, and two things about them follow from that:

- The records carry no `rule_version`; they read as version 0, which is the rule they reproduce
  under.
- The judge was a model, started with nothing but its instructions and the packets. What it was told
  is kept word for word beside the verdicts (`judge-instructions.txt`): the three numbered rules
  above, which a test checks line by line, and around them which file to read and how to reply. That
  it was blind rests on the packets, which are kept too.

```
lapilli case report test/fixtures/case-runs/2026-10-06-hard-cases \
  --verdicts test/fixtures/case-runs/2026-10-06-hard-cases/verdicts.json \
  --key test/fixtures/case-runs/2026-10-06-hard-cases/key.json
```

| case | condition | outcome | decisive evidence retrieved |
|---|---|---|---|
| s1-shared-cache-exhaustion | frozen / live | 1/3 · 1/3 | 2/3 · 2/3 |
| s2-periodic-saturation | frozen / live | 2/3 · 1/3 | 3/3 · 1/3 |
| s3-node-local-drift | frozen / live | 0/3 · 1/3 | 0/3 · 1/3 |

Two tests keep the headline honest: every recorded run must grade to the process result stored beside
it, and the report must still say 3 of 9 in each condition, with 6 of the 9 runs that retrieved all
decisive evidence passing and 0 of the 9 that did not (`internal/grade/grade_test.go`). The table
above is `report`'s output and is not itself asserted.

Read them with their limits: one small model, one author for cases, grader and rubric, a judge that
was a model from the same family as the agent, three runs per condition — and a frozen condition that
differed from the live one in ways found afterwards
([`design-review-round37.md`](design-review-round37.md) §3 and §9).

### Round 38

`test/fixtures/case-runs/2026-10-07-round38/` holds 72 runs made with the repaired instrument under
a rule fixed beforehand ([`design-review-round38.md`](design-review-round38.md)): the same three
cases, live and frozen, six runs each of Claude Code 2.1.292 (`--model haiku`) and HolmesGPT 0.42.0
(`gpt-5-mini`). Beside them: the three cases frozen for the round, the packets, the key, **two**
sets of verdicts — a Claude model and `gpt-5.5` — with what each judge was told, and the output of
the analysis committed before the runs. The records carry `rule_version` 0. A run passes there if
both judges pass it.

| agent | condition | outcome | decisive evidence retrieved |
|---|---|---|---|
| claude-code, haiku | live / frozen | 5/18 · 5/18 | 7/18 · 8/18 |
| holmes, gpt-5-mini | live / frozen | 0/18 · 0/18 | 0/18 · 0/18 |

A test asserts those counts, that the judges passed 10 and 13 and disagreed on 3, and that of the 15
runs that retrieved all decisive evidence 10 passed and of the 57 that did not, none
(`internal/grade/grade_test.go`). Both judges were given the rule above whole, and the test checks
that too.

Their limits: the first list still applies but for the judge and the count, and the frozen condition
of this round differed from the live one as well — in what its tables show, found afterwards
(round 38, R2).
