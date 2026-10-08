# The same command, against a cluster and against its frozen copy

A frozen case is worth what its answers are worth, and an answer is right when it is what the cluster
said. This asks both and compares. No agent, no model, no judge and no reading.

It exists because fidelity was twice found by accident: field selectors and `--tail` by a reviewer
reading code (round 37 §9), the tables by someone reading transcripts for another reason (round 38,
R2). Both times an experiment built to compare live with frozen had already run and seen nothing.

| file | what |
|---|---|
| `sweep.sh` | per case: build it on a kind cluster, ask every command of the live cluster, freeze it, serve the frozen copy and ask them of that, and ask them of the live cluster again — all within three quarters of a minute. It exits 0 only if every case was built, asked three times and compared. A case is one of the three scenarios under `scenarios/`, or a directory with a `setup.sh`, a `teardown.sh` and a `case.yaml` of its own |
| `kinds/` | such a directory: the kinds the three scenarios lack that an investigation is likely to list, and a pod in each state it is likely to meet — see below |
| `replaydiff.py` | `commands` writes what to ask, `capture` asks, `compare` prints the report and exits 1 if a command differs that `known.txt` does not excuse |
| `known.txt` | the differences that are known and written down in [`docs/design-case.md`](../../docs/design-case.md) §8: a verb, a kind, and a pattern the difference itself must show, so that a line excuses what it names and no more |
| `selftest.py` | what `compare` must call a difference and what it must not: answers a frozen case once gave, and ones a looser comparison let through. It runs in the required Go job, on no cluster |
| `spoil.py` | how much of a frozen answer the comparison holds it to: it spoils each recorded frozen answer in five ways and counts how many still pass |
| `promdiff.py` | the same for a case's metrics: what `promq` prints, and what the Prometheus HTTP API sends, from a Prometheus and from the store frozen from it — see below |
| `known-promq.txt` | the differences of that kind that are known: a pattern over the query or the request, what each side has to say for the line to apply, and the reason |

```
go build -o lapilli-case ./cmd/lapilli-case
LAPILLI_CASE=$PWD/lapilli-case KIND=kind LAPILLI_CRUST_GATHER=kubectl-crust-gather test/replay-diff/sweep.sh "$PWD" /tmp/replay-diff
cat /tmp/replay-diff/*/report.md

# one case, and another version of Kubernetes than kind's own
KIND_NODE_IMAGE=kindest/node:v1.33.1 LAPILLI_CASE=$PWD/lapilli-case test/replay-diff/sweep.sh "$PWD" /tmp/replay-diff-1.33 test/replay-diff/kinds

# the commands of other recorded runs than the ones in this repository, made on the same frozen cases: directories, a colon between them
RECORDED=<a directory>:<another> OLD_SNAPSHOTS=$PWD/test/fixtures/case-runs/2026-10-07-round38/frozen-cases LAPILLI_CASE=$PWD/lapilli-case \
  test/replay-diff/sweep.sh "$PWD" /tmp/replay-diff-typed s1-shared-cache-exhaustion s2-periodic-saturation s3-node-local-drift
```

**What is asked.** Two sets, together five to eight hundred commands a case:

- *About every kind the cluster has* (`kubectl api-resources --verbs=list`): the list, the list with
  `-o wide` and with `-o name`, one object by name, that object with `-o wide`, and `describe`. Then,
  for the scenario's own namespaces, the ways an investigation asks — sorted, by field selector, by
  label, `-o yaml`, `-o json`, `jsonpath`, `custom-columns`, `get all`, several kinds at once, each
  pod's `describe`, its events, its log — whole, by its tail, with its times, since a time and since
  so many seconds ago, from its previous container and from each of its init containers — each
  Deployment's `rollout history` and `rollout status` — and for what is not there.
- *What agents typed.* Every `kubectl` read in the recorded transcripts of rounds 38 and 39 under
  `test/fixtures/case-runs/` (or of `RECORDED`), with the pod names of the cluster they ran on
  replaced by this one's. A step is parted into commands as a shell parts it —
  at a newline, a `;`, a `|` or an `&` that stands outside quotes — and each command is taken as it
  was written, without its redirections; an argument quoted over several lines is one argument.
  **What cannot be asked as it stands is not asked, and is counted**: a command with a `$` or a
  backtick that a shell would expand, and any step with a here-document or a quote left open. The
  sweep prints how many of those held a `kubectl`, and each of them. `promq` is not asked: the
  sweep compares `kubectl`.

  This reading was wrong twice before it was written so, as a pattern over a line of text
  (`docs/design-review-round39.md`, Parts 2 and 4). A redirection before a semicolon took the
  semicolon with it and made one command of two: 22 commands of round 39's were asked that nobody
  had typed. And a line was read or left out whole: a `$` inside single quotes left one out, and a
  command standing after a loop on the same line was never asked. Of round 38's records the old
  reading and this one take the same 276 commands but for one, a field selector written with an
  unquoted `>=`, which is now asked as the shell ran it.

Everything goes through the guard, as an agent's `kubectl` does, so nothing but a read is sent; the
kind cluster has its own kubeconfig, named on every call, and the default one is never read.

**The fixture, `kinds/`.** The scenarios are three small incidents, and a kind they do not have is a
kind whose answers were never compared. `kinds/` is not an incident: it is a StatefulSet with its
claims, a DaemonSet, a ReplicationController, Jobs that finished, failed and ran in parallel,
CronJobs, Services of every type and one with hand-written Endpoints, Ingresses and their classes,
network policies, disruption budgets, quotas and a limit range, an autoscaler, a claim nothing
binds, a volume, two storage classes that both say they are the default, roles with colons in their
names, a webhook that matches nothing, and custom resources — one with three versions and a date
that has not come, one with no columns, and one whose kind is called `Service`, with a column of
every type and paths written several of the ways a definition may write one. And pods in the states an investigation meets: running with a sidecar, waiting on an init
container, failing in one, crashing, killed for memory, finished, failed, unschedulable, unable to
pull its image, half ready, held at a scheduling gate, being deleted with an hour to stop in, and
deleted and held by a finalizer after its container is gone.

Its `setup.sh` ends by waiting, some seven minutes in all. What fails there fails again and again,
each time after a longer wait — ten seconds, twenty, forty, to five minutes — and the cluster is
handed over when the next minutes are ones in which none of it changes: each crashing container
restarted five times and written down by the kubelet as waiting for the next, and the image that
cannot be pulled just tried, with the next try at least two and a half minutes off. Some twenty of
791 commands then get two different answers from the cluster itself — an event seen again, a node's
heartbeat, a log that went on — where 53 of 743 did before the wait was written this way.

**How an answer is compared.** Token for token, with two allowances for time.

*An age has to be older by what lay between the two countings, give or take five seconds* and how
finely an age of that size is written — to the second under ten minutes, to the minute under eight
hours, to the hour under eight days: `12m` and `13m`, `119s` and `2m`, `47h` and `2d` — never `89m`
and `1h`, nor `3h5m` and `3h`, nor `250m` of CPU and `3m`. Who counted, and when, depends on the command. `kubectl get` prints a table the server made,
and a case's server counts every age to the freeze, however much later it is asked; `describe` and
`kubectl events` count for themselves, from the moment they are run. So a frozen table is held to
the freeze and a frozen `describe` to its own asking, and an age that is right by the wrong one of
the two is a difference. One kind of age does not only grow: when an event was last seen is when it
was last seen, and it is seen again. Where the cluster's own two answers show that — the age in the
second is not the age in the first grown by the time between — the frozen one may be any age no
older than either allows.

*The count of a repeating event may lie between the two live counts*, wherever its line stands in
each: two events of one second may be listed either way round.

Everything else has to be the same: a restart count, a column, the order of the lines.

| | |
|---|---|
| the same | the frozen answer is one of the two live ones, and so is what was said beside it on stderr. Where the cluster refused at one asking and answered at the other, it is one of those two, whole |
| the same lines in another order | of several logs asked for at once — by selector, or of all a pod's containers — with another of them first and the rest following round: `kubectl` keeps them in a map, and prints them so one time in four of a cluster as of a case (30 of 120 askings of one frozen case; a full sweep failed on it once, 2026-10-08). The lines of each log are held to their order. And under `--sort-by` and for `kubectl events`, where two rows of equal key may stand either way round — and not the whole listing the other way up, which is told by the ages at the start or the end of its lines where it has them. And for what `kubectl describe` says a LimitRange limits, which it writes in the order a Go map gives it. Anywhere else, another order is a difference |
| the cluster moved | the two live answers differ from each other and the frozen one from both: the cluster changed while it was being asked. The frozen answer cannot then be told line by line, but it is still held to what did not move: every line the cluster printed both times, the heading among them, in the order it kept; no more lines than the longer live answer and no fewer than the shorter; and beside it on stderr what the cluster said beside one of its own |
| **both fail, worded differently** | both refuse, and the frozen copy in words that are neither of the cluster's two, or having printed something else first. **Fails the sweep** |
| **differ** | anything else: the two live answers agree and the frozen one does not, one side fails where the other answers, the cluster moved and the frozen answer is not between its two, or a command timed out. **Fails the sweep** |

A log is compared as a log. Whole, the frozen one must sit between the two live ones, line for line.
By its tail, or by the last so many seconds, it must be a stretch of the log the cluster's own two
windows show — those lines, in that order, none left out and none put in: no longer than was asked
for, no shorter than the cluster's two if it is a tail, and holding what both of the cluster's hold
if it is a window of time. Two windows of time may share no line and still meet, where no line says
otherwise, and one open between them may then hold nothing. A log that went by faster than its tail leaves the cluster's two windows with no line in
common: that is the cluster moving, and then only the number of the frozen lines is held. When a container started again
between the two live askings its log was not added to but replaced, and the frozen one must be the
first grown or the start of the second — and not empty, which is the start of anything. Several
logs asked for at once come in no fixed order, so when one of them is refused what was printed
before is chance; each line of it must still be a line the cluster printed. What is said beside a
log on stderr is compared too.

A command that fails the sweep is excused only if a line of `known.txt` names its verb and kind and
matches the difference it shows. `describe secret` is excused for counting other bytes, not for
failing — which it once did, under a line that excused both.

**What the comparison lets by**, measured and not argued. `selftest.py` holds some hundred answers
it has to judge rightly, each one an answer a frozen case once gave or a looser rule let through. And
on the recorded answers of a full sweep, the frozen answer of every command that printed at least
three lines was spoiled in five ways and judged again:

| the frozen answer, spoiled | still passes | of | with the comparison as it was before |
|---|---|---|---|
| replaced by nothing | 0 | 1,157 | 22 |
| cut to its first line | 0 | 1,157 | 47 |
| its last line taken off | 16 | 1,157 | 63 |
| its last line written twice | 15 | 1,157 | 40 |
| its lines the other way up | 38 | 1,149 | 84 |

(`spoil.py`, on the answers of the v1.37 run of 2026-10-08. The answers themselves are megabytes
and are not kept here; its output is, in `2026-10-08/spoiled.md`, and any sweep's output directory
can be given to it. "Passes" is the same, another order, or the cluster moved.)

What still passes is what three answers cannot decide. The 16 are logs with their last line taken
off: a log that grew is also a log that grew by one line less. The 15, and 22 of the 38, are logs
that raced past their tail, so that the cluster's own two windows share no line: the frozen lines
are then taken on trust and only counted. The other 16 of the 38 are listings under `--sort-by`,
whose key is often not printed, and what `kubectl describe` says a LimitRange limits: an order
cannot be checked against a key that is not there, and so `kubectl events` shuffled, and not merely
turned over, would pass too. And where a container started again, what its first log grew by before
it was replaced no asking saw. A pod that cannot pull its image is the
one thing that moves and shows nothing: `ErrImagePull` for some seconds at each try and
`ImagePullBackOff` between, with no count; a try that falls between the two live askings leaves
them alike and the frozen answer different. The fixture hands the cluster over when the next try is
minutes off. The comparison itself is blind to it.

`2026-10-08/spoiled-with-round39.md` is the same measure of a later sweep, which asked round 39's
commands with round 38's and judged them with the comparison as it now is: 0, 1, 39, 17 and 47 of
1,325 (1,319 for the last). The one that passes cut to its first line is a tail whose first line is
also the first of the cluster's next window, which reads as the start of a log that began again.
That sweep is also where two things were found that had gone unseen: several logs with another of
them first, which the table above now names, and a log whose tail ends in a blank line the
application wrote, which was taken off the end and made a tail of five into four. And a window
of no seconds, `--since=0s`, was let by empty though `kubectl` prints the tail for it.

**The metrics of a case, `promdiff.py`.** A case frozen with its Prometheus is asked the other half
of what an agent reads: what `promq` prints, and what the Prometheus HTTP API sends to an agent
whose tool asks it directly. Of the four cases one has a Prometheus, `s2-periodic-saturation`, and
the sweep does this for that one, by its name.

A metric's value cannot be compared as a table can: no two askings share a now. An instant can. So
every query is put **about the instant of the freeze, by name** (`promq --at`): to the Prometheus as
soon as the freeze is done, to the frozen store, and to the Prometheus again at the end. The three
answers have to be the same text. The frozen store is then asked once more with no instant named,
as an agent asks, and has to say what it said by name. And the request `promq` makes for each query
is made of the API itself, with the ones a client finds its way about by, and the answers compared
as they were sent: the series in the order they came, each value the string it was, each number as
it was written, the engine's remarks beside them.

- *What is asked.* A fixed set about every metric the Prometheus holds — the series, its samples as
  stored, which of them an instant picks, some twenty functions and aggregations, subqueries with a
  step and without, by each label and for one value of each label, and each as a window too — then
  about no metric in particular, what a request may be asked with (`limit`, `timeout`,
  `lookback_delta`), and what is refused. And what recorded agents asked: every `promq` command in
  a transcript, and the calls of an agent that has a Prometheus tool of its own (HolmesGPT's: a
  query as an instant or as a window of the length it asked for; its searches for metric names,
  label values, series, and what kind a metric is). A window is always put to end at the freeze;
  what such a tool sent word for word is not in a record, so these are what its calls ask for and
  not a copy of them. A `promq` that needs a shell to mean anything and a call that cannot be read
  are counted and printed, as for `kubectl`.
- *What a difference is.* Text that is not the same text. Nothing is an age here, so nothing is let
  by for having moved — unless the Prometheus's own two answers differ, and then the frozen one has
  to be one of the two; more than one answer in fifty like that and nothing was compared, which
  fails. Of the API, the same series and values in another order is said to be that and does not
  fail — but only where the order is no one's: a request whose outermost operation an engine keeps
  in a map (`count_values`, `histogram_quantile`, `histogram_fraction`), or one whose two answers
  from the Prometheus itself came in two orders. The series of a selector, a sorted answer and a
  window in another order are differences, and which series `topk` kept is in the series.
- *What is known* is in `known-promq.txt`: the query, what the Prometheus has to say and what the
  frozen store has to say for the line to apply, and why. Four lines, of three differences.
- *What it lets by*, measured: `promdiff.py spoil` spoils every frozen answer and counts the ones
  that still pass — replaced by nothing, by an empty result, a line less, a line more, a digit
  changed, its lines the other way up, and of the API its series the other way round.
  On the reports beside this, none of 636 answers of `promq` and none of 725 of the API passes
  spoiled, in any of those ways (`2026-10-08/promq/spoiled.md`).

`2026-10-08/promq/` holds `before.md`, the first report: 546 queries, 42 that differed and 12 refused
in other words, and 55 that the frozen store itself answered two ways — and `promq.md` and `api.md`,
the reports after the repairs: 636 queries, of which one is refused in other words; and 725 requests,
of which two are refused in other words and one is answered with nothing where the Prometheus has
something to say. All four are known.
[`docs/design-case.md`](../../docs/design-case.md) §3 says what the reasons were.

Two reviews of this comparison, each by a reader given the code and the data of its runs, found
things wrong with it, and it is as above because of them. The first: a choice among equal series
was let by on the values alone, so that a made-up series with the right value passed (`topk` is no
longer read specially: a case keeps its Prometheus's order, and the choice is the same); a known
line excused whatever its query answered; a second asking of the Prometheus that failed altogether
made every answer "moved" and the sweep pass; a window of thirty days was written `2.592e+06` and
asked as something else; the API's answers were compared in an order that is no one's; and only
what an agent typed into a shell was taken from a transcript. The second, of the repairs: any
reordering of what the API sent passed, 108 answers of 108 turned the other way round; a time
written `…354` and one written `…354.000` were read as one number; a known line was held to the
frozen store's words and not to the Prometheus's; six of the twenty-five windows the other agent's
tool had asked for were dropped for how their times were written, and nothing said so; and half of
the changes one could make to this file went unnoticed by `selftest.py`, which had never run the
part that decides what to ask, nor the command itself.

What it cannot do: ask about a time past the freeze, of which a Prometheus asked afterwards knows a
later, so that how a request that overshoots the freeze is answered is held by unit tests alone
(`internal/metrics`); ask the old edge of a case, while the Prometheus is younger than the window
frozen; compare what a Prometheus sends for `stats`, which is how long it took, or a label's values
under a `limit`, of which it sends whichever it met first; or say anything of a Prometheus other
than the one it ran against (§8 of the design has the list).

**What it has found.** `2026-10-07/` holds the reports of the first two full runs — the tool as it
was when round 38 ran, and the tool repaired — and the records of two agent runs on the repaired
replay. Over three scenarios and 1,577 commands: 635 differed or were refused in other words
before, 12 after, of the three kinds `known.txt` lists. [`docs/design-review-round38.md`](../../docs/design-review-round38.md), Part 3,
says what they were.

`2026-10-08/` holds the runs made when the fixture was added: `before/`, the fixture against the
tool as the first runs had left it — 143 of 791 commands differing or refused in other words,
beside five of the three known kinds — and, on v1.33 and v1.31, against a build that still printed
every table as v1.37 does, which is how the differences between versions were found (those two
reports are of the fixture and the comparison as they were that hour); `v1.37/`, the three scenarios and the fixture against the tool repaired again —
2,469 commands, 17 that differ, of the same three kinds; the same on an older cluster, `v1.33/` —
2,340 commands and the same 17; `kinds-on-other-versions/`, the fixture alone on v1.31, v1.32,
v1.34, v1.35 and v1.36; and `spoiled.md`, the measure above. Part 4 of the same document says what
was found, and what a review of the repair found after that.

**What it does not do.** It compares what `kubectl` prints, not what the API returns; a client that
reads the API another way is not covered. It does not ask what a watch sends, nor `logs -f`, which
does not end on a cluster. It asks about the kinds its four cases have an object of: seventeen of
the sixty-eight a v1.37 cluster can list have none in any of them — pod templates, CSI drivers,
admission policies, resource claims — and there is an autoscaler with no metrics to read and no
aggregated API. It asks clusters of v1.31 to v1.37 and none older, each with the `kubectl` of its
own version: a client of another version than its server was asked of none. And it asks the frozen
copy within seconds of the freeze, while the cluster still stands: what a case looks like replayed a
month later — when `kubectl describe` says a month has passed — is not something a live cluster can
be asked.
