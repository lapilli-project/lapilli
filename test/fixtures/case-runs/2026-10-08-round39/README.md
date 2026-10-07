# Round 39 — the records of the first set

Everything [`docs/design-review-round39.md`](../../../../docs/design-review-round39.md) Part 2 was
computed from. Eighteen runs made on 2026-10-07 between 21:57 and 21:59 UTC with `lapilli-case`
built from `main` at `ffc133f`, the commit that put the rule and the instruments under
[`test/round39/`](../../../round39/) before any of these existed. The cases they ran on are round
38's frozen ones, [`../2026-10-07-round38/frozen-cases/`](../2026-10-07-round38/frozen-cases/).

| path | what |
|---|---|
| `<case>/frozen-claude-code-haiku-<n>.json` | the run records: the transcript as the agent's model saw it, its answer, usage, and the process checks. Six a case |
| `signs.md` | **R1**: the output of `signs.py` |
| `sweep/<case>.md` | **R2**: the sweep's report for each scenario, with these runs' commands as the ones an agent typed |
| `reader.json` | **R3**: the reader's list, as it wrote it. What became of each item is in Part 2 |
| `packets.json`, `key.json` | the eighteen blind packets both judges were given, and packet id → run. Neither judge saw the key. Seed of the drawing: `4491937857107549670` |
| `verdicts-1.json`, `verdicts-2.json` | each judge's verdicts, by packet id. The rule is round 38's, byte for byte ([`../2026-10-07-round38/judge-rule.txt`](../2026-10-07-round38/judge-rule.txt)) |
| `described.md` | the output of `describe.py`: what Part 1 said would be described and not claimed |

**The judges and the reader.** Judge 1 was a Claude Code subagent started with round 38's
instructions for judge 1 and nothing else, in one fresh context, all eighteen packets; the call did
not pin a model, and the session that started it ran Claude Opus 5.5. Judge 2 was `gpt-5.5` over the
API, one call a packet. The reader was a Claude Code subagent started with
[`reader-instructions.txt`](../../../round39/reader-instructions.txt) and nothing else, the two
paths filled in.

**To recompute:**

```
D=test/fixtures/case-runs/2026-10-08-round39
python3 test/round39/signs.py $D/s*/ | cmp - $D/signs.md
python3 test/round39/describe.py $D $D/key.json $D/verdicts-1.json $D/verdicts-2.json | cmp - $D/described.md
lapilli case report $D --verdicts $D/verdicts-1.json --key $D/key.json
```

`internal/grade` recomputes the process checks of all eighteen from their transcripts.

There is no `excluded/`: no run was set aside as a provider failure. Nothing was removed from the
transcripts. They were searched for local paths and account names before being committed; there
were none.
