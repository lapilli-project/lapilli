# Round 39 — the records of the second set

Everything [`docs/design-review-round39.md`](../../../../docs/design-review-round39.md) Part 4 was
computed from. Eighteen runs made on 2026-10-07 between 22:19 and 22:23 UTC with `lapilli-case`
built at `2b7ef93` — the commit, pushed before any of these existed, that carries Part 3: the rule
of this set and the adapter's first repair. The cases are round 38's frozen ones,
[`../2026-10-07-round38/frozen-cases/`](../2026-10-07-round38/frozen-cases/); the first set's
records are in [`../2026-10-08-round39/`](../2026-10-08-round39/).

| path | what |
|---|---|
| `<case>/frozen-claude-code-haiku-<n>.json` | the run records, six a case |
| `signs.md` | **R1**: the output of `signs.py` |
| `sweep/<case>.md` | **R2**: the sweep's report for each scenario, with these runs' commands as the ones an agent typed. The sweep misread fifteen of them, as Part 4 says; [`../2026-10-08-round39-after/sweep-again/`](../2026-10-08-round39-after/sweep-again/) is the same asking with the reading repaired, made afterwards and under no rule |
| `reader.json` | **R3**: the reader's list, as it wrote it. What became of each item is in Part 4 |
| `packets.json`, `key.json` | the eighteen blind packets both judges were given, and packet id → run. Seed of the drawing: `5377798145592972393` |
| `verdicts-1.json`, `verdicts-2.json` | each judge's verdicts, by packet id, under round 38's rule |
| `described.md` | the output of `describe.py` |

The judges and the reader were started as for the first set, each anew and given only this set.

**The adapter these runs used is not the one that was merged.** They ran with a pattern for each
read in each place a namespace can stand. A review then found that list incomplete and, in one
respect, too wide, and the adapter was changed again: Part 4 says how, and what was run after.

**To recompute:**

```
D=test/fixtures/case-runs/2026-10-08-round39-second
python3 test/round39/signs.py $D/s*/ | cmp - $D/signs.md
python3 test/round39/describe.py $D $D/key.json $D/verdicts-1.json $D/verdicts-2.json | cmp - $D/described.md
```

There is no `excluded/`: no run was set aside as a provider failure. Nothing was removed from the
transcripts, and they were searched for local paths and account names before being committed.
