# The same command, against a cluster and against its frozen copy

A frozen case is worth what its answers are worth, and an answer is right when it is what the cluster
said. This asks both and compares. No agent, no model, no judge and no reading.

It exists because fidelity was twice found by accident: field selectors and `--tail` by a reviewer
reading code (round 37 §9), the tables by someone reading transcripts for another reason (round 38,
R2). Both times an experiment built to compare live with frozen had already run and seen nothing.

| file | what |
|---|---|
| `sweep.sh` | per scenario: build it on a kind cluster, ask every command of the live cluster, freeze it, ask them of the live cluster again, serve the frozen copy and ask them of that |
| `replaydiff.py` | `commands` writes what to ask, `capture` asks, `compare` prints the report and exits 1 if a command differs that `known.txt` does not list |
| `known.txt` | the differences that are known and written down in [`docs/design-case.md`](../../docs/design-case.md) §8 |

```
go build -o lapilli-case ./cmd/lapilli-case
LAPILLI_CASE=$PWD/lapilli-case KIND=kind LAPILLI_CRUST_GATHER=kubectl-crust-gather test/replay-diff/sweep.sh "$PWD" /tmp/replay-diff
cat /tmp/replay-diff/*/report.md
```

**What is asked.** Two sets, together a few hundred commands a scenario:

- *About every kind the cluster has* (`kubectl api-resources --verbs=list`): the list, the list with
  `-o wide`, one object by name, that object with `-o wide`, and `describe`. Then, for the scenario's
  own namespaces, the ways an investigation asks — sorted, by field selector, by label, `-o yaml`,
  `-o json`, `jsonpath`, `custom-columns`, `get all`, each pod's `describe`, its log and its events.
- *What agents typed.* Every `kubectl` command in the recorded transcripts of
  `test/fixtures/case-runs/2026-10-07-round38/`, with the pod names of the cluster they ran on
  replaced by this one's.

Everything goes through the guard, as an agent's `kubectl` does, so nothing but a read is sent; the
kind cluster has its own kubeconfig, named on every call, and the default one is never read.

**How an answer is compared.** Ages are taken out — a cluster is a few seconds older by the time it is
frozen — and so is the count of a repeating event. Then:

| | |
|---|---|
| the same | the frozen answer is one of the two live ones |
| the same lines in another order | as it says: two events of one second, the other way round |
| both fail, worded differently | both refuse, in different words |
| the cluster moved | the two live answers differ from each other, the frozen one from both, and all three have the same heading: the cluster changed while it was being asked, and nothing is concluded |
| **differ** | the two live answers agree and the frozen one does not, or one side fails where the other answers |

A log is compared as a log: the frozen one must sit between the two live ones, and a `--tail` must
return as many lines.

**What it has found.** `2026-10-07/` holds the reports of the first two full runs — the tool as it
was when round 38 ran, and the tool repaired — and the records of two agent runs on the repaired
replay. Over three scenarios and 1,346 commands: 584 differed before, 12 after, of the three kinds
`known.txt` lists. [`docs/design-review-round38.md`](../../docs/design-review-round38.md), Part 3,
says what they were.

**What it does not do.** It compares what `kubectl` prints, not what the API returns; a client that
reads the API another way is not covered. It asks about the kinds a kind cluster with one of three
small scenarios has — no StatefulSet, no Job, no Ingress, no custom resource. And it runs within a
minute of the freeze: what a case looks like replayed a month later is not something a live cluster
can be asked.
