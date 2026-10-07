# The same command, against a cluster and against its frozen copy

A frozen case is worth what its answers are worth, and an answer is right when it is what the cluster
said. This asks both and compares. No agent, no model, no judge and no reading.

It exists because fidelity was twice found by accident: field selectors and `--tail` by a reviewer
reading code (round 37 §9), the tables by someone reading transcripts for another reason (round 38,
R2). Both times an experiment built to compare live with frozen had already run and seen nothing.

| file | what |
|---|---|
| `sweep.sh` | per scenario: build it on a kind cluster, ask every command of the live cluster, freeze it, serve the frozen copy and ask them of that, and ask them of the live cluster again — all within half a minute. It exits 0 only if every scenario was built, asked three times and compared |
| `replaydiff.py` | `commands` writes what to ask, `capture` asks, `compare` prints the report and exits 1 if a command differs that `known.txt` does not excuse |
| `known.txt` | the differences that are known and written down in [`docs/design-case.md`](../../docs/design-case.md) §8: a verb, a kind, and a pattern the difference itself must show, so that a line excuses what it names and no more |
| `selftest.py` | what `compare` must call a difference and what it must not: answers a frozen case once gave, and ones a looser comparison let through. It runs in the required Go job, on no cluster |

```
go build -o lapilli-case ./cmd/lapilli-case
LAPILLI_CASE=$PWD/lapilli-case KIND=kind LAPILLI_CRUST_GATHER=kubectl-crust-gather test/replay-diff/sweep.sh "$PWD" /tmp/replay-diff
cat /tmp/replay-diff/*/report.md
```

**What is asked.** Two sets, together a few hundred commands a scenario:

- *About every kind the cluster has* (`kubectl api-resources --verbs=list`): the list, the list with
  `-o wide` and with `-o name`, one object by name, that object with `-o wide`, and `describe`. Then,
  for the scenario's own namespaces, the ways an investigation asks — sorted, by field selector, by
  label, `-o yaml`, `-o json`, `jsonpath`, `custom-columns`, `get all`, several kinds at once, each
  pod's `describe`, its events, its log whole, by its tail and from its previous container, each
  Deployment's `rollout history` and `rollout status` — and for what is not there.
- *What agents typed.* Every `kubectl` command in the recorded transcripts of
  `test/fixtures/case-runs/2026-10-07-round38/`, with the pod names of the cluster they ran on
  replaced by this one's.

Everything goes through the guard, as an agent's `kubectl` does, so nothing but a read is sent; the
kind cluster has its own kubeconfig, named on every call, and the default one is never read.

**How an answer is compared.** Token for token, with two allowances for time. An age may differ by
as long as lay between the two askings, which is noted beside each answer, and by what the finer of
the two last units hides: `12m` and `13m`, `119s` and `2m` — never `89m` and `1h`, nor `250m` of CPU
and `3m`. And the count of a
repeating event may lie between the two live counts. Everything else has to be the same: a restart
count, a column, the order of the lines.

| | |
|---|---|
| the same | the frozen answer is one of the two live ones, and so is what was said beside it on stderr |
| the same lines in another order | under `--sort-by` only, where two rows of equal key may stand either way round. Without it, another order is a difference |
| the cluster moved | the two live answers differ from each other, the frozen one from both, and all three have the same heading: the cluster changed while it was being asked, and nothing is concluded |
| **both fail, worded differently** | both refuse, in other words, or having printed something else first. **Fails the sweep** |
| **differ** | the two live answers agree and the frozen one does not, one side fails where the other answers, or a command timed out. **Fails the sweep** |

A log is compared as a log. Whole, the frozen one must sit between the two live ones, line for line.
By its tail, it must be a live tail or a live tail moved on by some lines, and no longer than was
asked for; a log that went by faster than its tail is the cluster moving.

A command that fails the sweep is excused only if a line of `known.txt` names its verb and kind and
matches the difference it shows. `describe secret` is excused for counting other bytes, not for
failing — which it once did, under a line that excused both.

**What it has found.** `2026-10-07/` holds the reports of the first two full runs — the tool as it
was when round 38 ran, and the tool repaired — and the records of two agent runs on the repaired
replay. Over three scenarios and 1,577 commands: 635 differed or were refused in other words
before, 12 after, of the three kinds `known.txt` lists. [`docs/design-review-round38.md`](../../docs/design-review-round38.md), Part 3,
says what they were.

**What it does not do.** It compares what `kubectl` prints, not what the API returns; a client that
reads the API another way is not covered. It does not ask what a watch sends. It asks about the
kinds a kind cluster with one of three small scenarios has — no StatefulSet, no Job, no Ingress, no custom resource. And it asks the frozen
copy within seconds of the freeze, while the cluster still stands: what a case looks like replayed a
month later — when `kubectl describe` says a month has passed — is not something a live cluster can
be asked.
