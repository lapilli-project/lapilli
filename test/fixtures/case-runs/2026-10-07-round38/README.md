# Round 38 — the records

Everything [`docs/design-review-round38.md`](../../../../docs/design-review-round38.md) Part 2 was
computed from. 72 runs made on 2026-10-07 between 09:03 and 10:10 UTC with `lapilli-case` built from
`main` at `600cfdd`, the commit that put the instruments under [`test/round38/`](../../../round38/)
before any of these existed.

| path | what |
|---|---|
| `<case>/<condition>-<agent>-<model>-<n>.json` | the run records: the transcript as the agent's model saw it, its answer, usage, and the process checks. 24 per case: 6 per agent and condition |
| `frozen-cases/<case>/` | the three cases `freeze` made from the cluster the live runs had just investigated, and that the frozen runs were given. Same answer keys as `cases/<case>/`; different snapshots |
| `packets.json` | the 72 blind packets both judges were given |
| `key.json` | packet id → run. Neither judge saw it. Seed of the drawing: `7692343065575330885` |
| `judge-rule.txt` | the rule as `lapilli case packets` printed it. Judge 2 was sent this, one packet, and one closing line — [`judge2.py`](../../../round38/judge2.py) is the whole of it |
| `judge-1-instructions.txt` | what judge 1 was told, word for word, with the two local file paths replaced by `<packets file>` and `<verdicts file>` |
| `verdicts-1.json`, `verdicts-2.json` | each judge's verdicts, by packet id |
| `analysis.md` | the output of `analyze.py`, the analysis committed before the runs |
| `posthoc.md` | the output of `posthoc.py`, written after the results were known and labelled so |

**The judges.** Judge 1 was a Claude Code subagent started with those instructions and nothing else,
in three fresh contexts at once: every third packet of `packets.json` each (`[0::3]`, `[1::3]`,
`[2::3]`), 24 apiece. The call did not pin a model; the session that started them ran Claude
Opus 5.5. Judge 2 was `gpt-5.5` over the API, one call per packet; three of its replies were not
valid JSON and were asked for again, unchanged, as the script does.

**To recompute:**

```
D=test/fixtures/case-runs/2026-10-07-round38
python3 test/round38/analyze.py $D $D/key.json $D/verdicts-1.json $D/verdicts-2.json $D/frozen-cases $D/excluded | cmp - $D/analysis.md
python3 test/round38/posthoc.py $D $D/key.json $D/verdicts-1.json $D/verdicts-2.json | cmp - $D/posthoc.md
lapilli case report $D --verdicts $D/verdicts-1.json --key $D/key.json
lapilli case verify $D/frozen-cases/s2-periodic-saturation
```

There is no `excluded/`: no run was set aside as a provider failure. `internal/grade` recomputes the
process checks of all 72 from their transcripts and asserts the headline counts.

Nothing was removed from the transcripts. They were searched, like the archives beside them and
inside them, for local paths and account names before being committed; there were none.
