# Round 39 — the instruments

What runs [`docs/design-review-round39.md`](../../docs/design-review-round39.md), and what counts
for it. Committed before any run, like the rule.

| file | what |
|---|---|
| `run.sh` | Part 1's runs: one agent, six runs a case, on the three cases frozen for round 38. No cluster is built. One invocation a run; what is set aside as a provider's failure, and when the round stops, are round 38's (`test/round38/lib.sh`) |
| `signs.py` | **R1.** The five signs of an answer only the old replay gave, counted over every step that did not fail. Round 38's own patterns, copied from `test/round38/posthoc.py` |
| `on-round38.md` | what `signs.py` says of round 38's records, made before any new run |
| `reader-instructions.txt` | **R3.** What the reader is told, word for word, with the two local paths replaced by `<records directory>` and `<output file>`. It says what a case is and not what was repaired |
| `describe.py` | what Part 1 says is described and not claimed: passes by each judge and by both, their agreement, evidence retrieved, steps, what the guard refused, and whether the one prediction held |

**R2** has no script of its own: it is `test/replay-diff/sweep.sh`, with the commands of these
runs as the ones an agent typed.

```
LAPILLI_CASE=$PWD/lapilli-case LAPILLI_CRUST_GATHER=kubectl-crust-gather test/round39/run.sh "$PWD" /tmp/round39

python3 test/round39/signs.py /tmp/round39/runs/*/                              # R1
RECORDED=/tmp/round39/runs OLD_SNAPSHOTS=$PWD/test/fixtures/case-runs/2026-10-07-round38/frozen-cases \
  LAPILLI_CASE=$PWD/lapilli-case test/replay-diff/sweep.sh "$PWD" /tmp/round39-sweep \
  s1-shared-cache-exhaustion s2-periodic-saturation s3-node-local-drift          # R2

lapilli-case packets /tmp/round39/runs cases/*/ -o /tmp/round39/packets.json --key /tmp/round39/key.json > /tmp/round39/judge-rule.txt
#   judge 1 reads packets.json under the rule; judge 2:
python3 test/round38/judge2.py /tmp/round39/packets.json /tmp/round39/judge-rule.txt /tmp/round39/verdicts-2.json gpt-5.5 1.5
python3 test/round39/describe.py /tmp/round39/runs /tmp/round39/key.json /tmp/round39/verdicts-1.json /tmp/round39/verdicts-2.json
```

`signs.py` was run on round 38's records before any new run existed. On agent A's eighteen frozen
runs it counts 34 steps in 15 runs; on its eighteen live runs, none. That output is in
[`on-round38.md`](on-round38.md). A count that could not have found the old replay's answers would
not be worth making of the new one's.
