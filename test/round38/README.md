# Round 38 — the instruments

What ran the comparison in [`docs/design-review-round38.md`](../../docs/design-review-round38.md), and
what computes its numbers. Committed before the runs, like the rule: an analysis written after the
data can be made to say what the data was hoped to say.

| file | what |
|---|---|
| `run.sh`, `lib.sh` | the procedure of Part 1: per case, build the scenario on a kind cluster, the live runs of both agents, freeze, tear down, the frozen runs. One invocation per run, so that a provider failure is seen, set aside and run once more; two in a row, or a spending limit, stop that agent |
| `judge2.py` | judge 2: one model call per blind packet, under the rule `lapilli case packets` prints. It is given a packet and the rule and nothing else |
| `analyze.py` | every number of Part 2, from the run records, the key and the two judges' verdicts: the counts, R1's differences with Newcombe intervals, Cohen's κ, R2's `describe` lengths, replay-only failures and late dates, R3 |

```
LAPILLI_CASE=$PWD/lapilli-case KIND=kind LAPILLI_CRUST_GATHER=kubectl-crust-gather HOLMES_BIN=<dir with holmes> \
  test/round38/run.sh "$PWD" /tmp/round38

lapilli-case packets /tmp/round38/runs cases/*/ -o /tmp/round38/packets.json --key /tmp/round38/key.json > /tmp/round38/packets.txt
#   judge 1 reads packets.json and the rule; judge 2:
python3 test/round38/judge2.py /tmp/round38/packets.json <the rule, as printed> /tmp/round38/verdicts-2.json gpt-5.5 3
python3 test/round38/analyze.py /tmp/round38/runs /tmp/round38/key.json /tmp/round38/verdicts-1.json /tmp/round38/verdicts-2.json \
  /tmp/round38/frozen /tmp/round38/excluded
```

`run.sh` creates a kind cluster named `rcabench` with its own kubeconfig under the output directory
and names that file on every call; it never reads the default kubeconfig. It needs Docker, `kind`,
`kubectl`, crust-gather, Claude Code logged in, HolmesGPT, and `OPENAI_API_KEY`.

`analyze.py` was tried on the recorded runs of 2026-10-06 before any new run existed, with the one
set of verdicts standing in for both judges. It gives 3 of 9 in each condition, as recorded — and
its `describe` rule fails on them, 4.37 and 2.29 where the band is 0.8 to 1.25, which is the
difference round 37 §9 found. A rule that could not have caught the old instrument would not be
worth applying to the new one.
