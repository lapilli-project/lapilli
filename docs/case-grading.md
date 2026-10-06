# Grading a case (rule version 0)

A result states the rule version that produced it. Changing anything on this page changes every
number computed under it, so a change is made in the open, in this file, and the version moves
(`internal/grade`: `RuleVersion`). Lapilli ships no agent, no model and no judge; whoever maintains a
graded agent does not get to change how it is graded without that being visible here.

## Process checks — deterministic, no model

Computed by `internal/grade.Check` from the transcript alone.

| check | definition |
|---|---|
| `evidence_retrieved` | for each `evidence` pattern, whether it matches the concatenated **output** of the agent's steps |
| `evidence_all` | all of the above |
| `specificity_named` | the case's `specificity` string occurs in the answer (case-insensitive) |
| `decoys_mentioned` | which `decoys` occur in the answer. **Mentioned is not blamed**: ruling a decoy out mentions it |
| `ungrounded_entities` | pod names (`<deployment>-<replicaset>-<pod>`) and IPv4 addresses in the answer that occur in no step's output |
| `steps`, `failed_steps`, `used_metrics` | counts; a step is failed when the tool reported an error or was refused |

**What the agent observed is what its tools returned; what it typed does not count.** The prototype
searched inputs as well, and that was a hole: `grep -c CACHE_CONN_MODE` contains the evidence whatever
it prints, and a query that names `client="catalog-indexer"` matched the metrics evidence of `s2`
before any number came back. Found by mutating the check and seeing no test fail. All 28 recorded runs
grade identically under both readings, so no published number moved, and a test now pins the
difference.

What these checks do **not** establish: that the agent understood what it retrieved (three of nine
runs retrieved everything and still failed), or that a mechanism it describes is real (an invented
mechanism names no pod and no address). They see what the adapter recorded; output the agent's own
harness truncated is seen truncated.

## Outcome — a blind judge

`lapilli case packets <results> <case>...` writes, per run, `{id, question, expected, must_not,
answer}`, shuffled, with a random id. The mapping back to runs goes to a separate key file that the
judge must not see. The instructions printed with the packets are the rule, verbatim:

> Grade each packet independently and strictly. PASS only if the answer clearly conveys every
> statement in `expected` (same meaning; wording may differ) AND does not do anything listed in
> `must_not`. An expected statement that is offered only as one unconfirmed possibility is missing. A
> statement that contains specifics (a number, a path, a setting, a node or workload name) is conveyed
> only if the answer gives those specifics or unmistakable equivalents. Blaming something listed in
> `must_not` as the cause is a FAIL even if the right cause is also mentioned as the cause. Mentioning
> a `must_not` item in order to rule it out is fine.

The sentence about specifics — "a value below 200" does not convey "64" — decided several verdicts in
the recorded runs.

`lapilli case report <results> --verdicts <file> --key <file>` shows the two halves side by side and
does not combine them. Without verdicts the outcome column reads "not judged".

## What a result must carry to be comparable

Agent and its version, model, the tools the agent was allowed, runs per case, the rule version, who or
what judged, and whether the judge was blind. A pass rate without those is not a result.

## The recorded runs

`test/fixtures/case-runs/2026-10-06-hard-cases/` holds 18 judged runs (three cases, live and frozen,
three runs each: Claude Code 2.1.289 headless, `--model haiku`, read-only tools) with the
packets the judge saw, the verdicts and the key. `2026-10-06-s3-extra/` holds ten more runs of one
case, not judged. Transcripts are the agent's recorded steps with local file paths removed.

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

Two tests keep these honest: every recorded run must grade to the process result stored beside it,
and the report must still say 3 of 9 in each condition, with 6 of the 9 runs that retrieved all
decisive evidence passing and 0 of the 9 that did not (`internal/grade/grade_test.go`).

Read them with their limits: one small model, one author for cases, grader and rubric, a judge that
was a model from the same family as the agent, and three runs per condition
([`design-review-round37.md`](design-review-round37.md) §3).
