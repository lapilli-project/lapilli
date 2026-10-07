# Design review — round 39 (an agent on the repaired replay)

Round 38 compared an agent on a live cluster with the same agent on its frozen copy, and what it
found that mattered it found outside its rule: the frozen copy answered some `kubectl get` commands
as no cluster does ([`design-review-round38.md`](design-review-round38.md), *Outside the rule*).
Those were repaired, and a test now asks a cluster and its frozen copy the same commands
(`test/replay-diff`) — some two and a half thousand of them, on seven versions of Kubernetes, with
seventeen that differ, of three kinds that are written down. What has not been looked at since is
an investigation: **no judged run has seen the replay as it now is** (`ROADMAP.md` §7, 1d).

This is not round 38 again. It has no live condition and makes no claim about outcomes. It is a
handful of runs of the one agent that could pass, on the frozen cases round 38 used, to see whether
the transcripts still hold an answer a cluster would not give.

Part 1 is written and committed before any run. Part 2 is the result, under Part 1's rules. Part 3
is the rule of a second set, pushed before its runs, and Part 4 is that set, what a review of the
repair found, and what a reader of these pages found wrong in them.

## Part 1 — fixed before any run

### What is being asked

On the replay as it is on `main` — tables built as the API server builds them, a log read by its
times — **does an investigation still meet an answer that a cluster would not have given?**

### What is run

| | |
|---|---|
| cases | the three cases frozen for round 38, as recorded: `test/fixtures/case-runs/2026-10-07-round38/frozen-cases/<case>`. The same snapshots round 38's frozen runs saw, served by the tool as it is now |
| agent | **A** of round 38: Claude Code, headless, `--model haiku`, the `claude-code` adapter as it is on `main`. Agent B passed no run of 36 in round 38, in either condition, and is not run |
| runs | **6** per case, 18 in all, one invocation each |
| tool | `lapilli-case` built from `main` at the commit Part 2 names; crust-gather v0.17.1 |

No cluster is built for the runs. One is built afterwards, for R2 and R3.

### The rules

**R1 — none of the answers round 38 found.** `test/round39/signs.py` counts, over every step that did
not fail, the five signs of an answer only the old replay gave: a sorted list that came back empty,
a pod listing asked for `-o wide` with no NODE column, a listing whose whole header is `NAME AGE`,
an event listing headed `LASTTIMESTAMP`, and `kubectl get all` printing names without their kinds.
They are round 38's own patterns, copied from `test/round38/posthoc.py`, where they were counted
with the results in hand. On round 38's records the script finds **34 such steps in 15 of agent A's
18 frozen runs, and none in its 18 live runs** — it is run on those first, and that is the output
committed beside it.

**The rule is zero.** Two of the signs a cluster can show too: a namespace with nothing in it sorts
to nothing, and a v1.37 cluster lists a few kinds — service accounts among them — by name and age
alone. So every step counted is listed with its command, and each is looked up in R2's sweep, which
puts that command to a live cluster: **a sign the cluster itself shows for the same command is the
cluster's own answer, reported and not counted.** A counted step whose command the sweep could not
ask stays counted. Nothing else is set aside.

**R2 — what the agent typed is answered as a cluster answers it.** After the runs, `test/replay-diff/sweep.sh`
is run over the three scenarios with round 39's transcripts as the commands an agent typed
(`RECORDED`, with round 38's frozen cases as `OLD_SNAPSHOTS`): each is put to a live kind cluster
built from the scenario and to its frozen copy, with the pod names of the recorded cluster replaced
by the new one's. **No command typed in round 39 may differ,
or be refused in other words, unless `test/replay-diff/known.txt` as it stands at this commit
excuses it.** Part 2 gives how many commands the 18 runs typed, how many of them the sweep could ask
— one that needs a shell to mean anything is not asked — and the verdict of each kind.

**R3 — a reading, by someone who did not write the replay.** A Claude model, started with no other
context, is given the 18 transcripts — every step's command and what came back — and the
instructions in `test/round39/reader-instructions.txt`, which say what a case is and do not say what
was repaired or what round 38 found. It lists every answer it believes a Kubernetes cluster in that
state would not have given, with the step and its reason.

Each item is then put to a cluster — the command, on a live kind cluster built from the scenario and
on its frozen copy — and is **confirmed** if the two differ in the way the reader said, **not
confirmed** if they agree, and **not testable** if the command cannot be asked of both. **The rule
is zero confirmed.** Every item is listed in Part 2 with what became of it, and "not testable" is
counted separately and not as a pass.

**What would count against the instrument**, stated now: a sign under R1 that is the replay's; a
command under R2 that differs; an item under R3 confirmed; a refusal by the guard of something a
read-only investigation needs.

### Described, and not claimed

Reported for the 18 runs, per case and together, with no rule on any of it:

- by `lapilli case` itself: `evidence_all`, each evidence item, steps, failed steps, and what the
  guard refused, by verb;
- by two judges on blind packets, as in round 38 and under the same rule version — judge 1 a Claude
  model started with no other context and given all eighteen at once, judge 2 `gpt-5.5` over the
  API, a packet a call: runs passed by both, by each, and Cohen's κ;
- `kubectl describe pod` steps a run. In round 38 agent A typed 0.9 a run live and 2.1 frozen — 2.9
  in the eight frozen runs whose wide pod listing had lost its NODE column, 1.4 in the other ten —
  counted afterwards, and so an observation there. Stated here beforehand, as a prediction and not
  a rule: **with the listings repaired, the mean of the eighteen lies below 2.0.** Part 2 says
  whether it did.

These numbers will stand beside round 38's frozen cell for the same agent — 5 of 18 passed — and
**nothing is concluded from the difference, whichever way it falls.** Eighteen runs a side resolve
some thirty points. And the two sets differ in more than the replay: the adapter's list of commands
was widened with the repair, the model behind `haiku` is whatever it is on the day, and round 38's
runs are a record, not a control run beside these.

### Exclusions, fixed now

As round 38's. A run that ends without an answer **because the provider failed** — an HTTP 5xx, a
429, a quota message in the run's error — is kept, marked, left out of every count, and run once
more; two in a row stop the round, and what there is is reported. A run that ends without an answer
for **any other reason** stays in every count and fails. Nothing else is excluded, and no run is
repeated because of what it said.

### Limits on spending

The agent, at most US$5 as its own harness reports cost; judge 2, US$1.50. Reaching a limit stops
that part, and Part 2 reports what there is.

### What this cannot show, whatever it finds

That another agent, or a stronger model, would meet nothing either: the commands are the ones this
agent thought to type. That the kinds these three cases lack are answered rightly to an agent — the
fixture of `test/replay-diff/kinds` compares them command by command, and no agent has been run on
it. That the replay matches a live cluster beyond the commands asked: R2 compares on a cluster built
again from the scenario, not on the one that was frozen. That the reader of R3 would see what a
person who operates clusters would see: it is a model, and it shares a family with the agent it
reads. And nothing about outcomes.

## Part 2 — the result

Eighteen runs, made on 2026-10-07 between 21:56 and 22:00 UTC — twelve hours after round 38's —
with `lapilli-case` built from `main` at `ffc133f`, the commit that put Part 1 and its instruments
there. No provider failed; none was excluded or repeated. The records, the packets, both judges' verdicts, the reader's
list and the sweep's reports are in `test/fixtures/case-runs/2026-10-08-round39/`.

### In short

- **R1 held: no sign in any run.**
- **R2 held as it was run: of the 83 commands the sweep asked, none differs** — and the sweep did
  not ask quite what the runs typed, which is told under R2 and put right in Part 4.
- **R3 held: the reader listed ten answers, all one claim, and a cluster gives the same answer.**
- **And the round did not do what it was for.** In eight of the eighteen runs no command ran at
  all. The agent wrote the namespace before the verb — `kubectl -n ledger get pods -o wide` — the
  adapter's list of what Claude Code may run knew each read only with the verb first, and after one
  to three refusals the agent answered that it could not investigate. All six runs of
  `s3-node-local-drift`, and two of `s1`. Part 1 said what would count against the instrument: *a
  refusal by the guard of something a read-only investigation needs*. By its letter that did not
  happen: the guard refused nothing. The adapter refused twenty-one reads the guard allows. This
  page reads the sentence as being about the instrument and not about one part of it, and **counts
  it** — a reading, made after the fact, and said to be one. Three rules held over ten
  investigations and eight refusals.

### What was run, and what ran

| case | runs | in which a command ran | steps that did not fail | refused by Claude Code | refused by the guard |
|---|---|---|---|---|---|
| `s1-shared-cache-exhaustion` | 6 | 4 | 39 | 7 | 0 |
| `s2-periodic-saturation` | 6 | 6 | 54 | 5 | 0 |
| `s3-node-local-drift` | 6 | **0** | 0 | 15 | 0 |
| all | 18 | **10** | 93 | 27 | 0 |

Of the 27 steps Claude Code's permission list refused, 21 are the 21 steps of the eight runs that
ran nothing, and every one begins `kubectl -n <namespace>`. The other six are of kinds no list of
`kubectl` commands changes: three `for` loops, a line that ends one command with `echo "exit=$?"`,
a line of three commands with a `sed` range in it, and one `kubectl exec`, which is not a read. The
twenty-eighth failed step is the cluster's own: `the server doesn't have a resource type
"ciliumnetworkpolicy"`.

In round 38, twelve hours earlier, the same agent under the same word `haiku` began none of its
739 steps with `kubectl -n`. Its eighteen frozen runs then took 80 seconds and 20.9 steps each, and
its harness reported US$0.111 a run. The ten runs here that investigated took 30 seconds and 10.0
steps, at US$0.008 a run. **What changed is not known.** A record carries the word `haiku` and
nothing more about the model, and Claude Code itself went from 2.1.292 to 2.1.293 between the two
rounds. Part 1 said the model behind `haiku` is whatever it is on the day; so is the harness, and
from here the two cannot be told apart. Whatever answered wrote the namespace first in eight of
these eighteen runs.

### R1 — none of the answers round 38 found

`signs.py` over the eighteen records: **no step shows any of the five signs.** Nothing was set
aside, because nothing was counted.

| what the step shows | round 38, agent A, frozen: times shown, runs | round 39: times shown, runs |
|---|---|---|
| an empty sorted list | 11, 9 of 18 | 0, 0 of 18 |
| a pod listing asked for `-o wide`, without NODE | 9, 8 of 18 | 0, 0 of 18 |
| a listing whose whole header is `NAME AGE` | 10, 8 of 18 | 0, 0 of 18 |
| an event listing headed `LASTTIMESTAMP` | 0, 0 of 18 | 0, 0 of 18 |
| `kubectl get all` with names and no kinds | 4, 4 of 18 | 0, 0 of 18 |
| any of them | 34, 15 of 18 | **0, 0 of 18** |

Part 1's 34 counts a step once for each sign it shows; they are 29 steps.

It holds over less than was meant. Ten runs had answers to show a sign in, and the case in which
round 38 found most of them — `s3`, 17 of the 34, in five of its six runs — has none here because
it has no answers here.

### R2 — what the agent typed, put to a cluster

The sweep read 135 `kubectl` commands out of the runs' 121 steps — the refused ones among them: it
asks what was typed, whether or not it ran. It left out seven command lines as needing a shell to
mean anything, and one `exec`. The other 134 are 83 different commands, each put to a live cluster
built from the scenario, to its frozen copy, and to the cluster again:

| case | asked | the same | the cluster moved | differ |
|---|---|---|---|---|
| `s1-shared-cache-exhaustion` | 36 | 35 | 1 | 0 |
| `s2-periodic-saturation` | 41 | 41 | 0 | 0 |
| `s3-node-local-drift` | 6 | 6 | 0 | 0 |
| all | **83** | 82 | 1 | **0** |

The whole sweep — these and the fixed set, 1,514 commands — differs on twelve, of the three kinds
`known.txt` has always named.

**That is the rule as it was run, and the sweep had not read the runs rightly.** A reader of these
pages, given the records, found what the count hides. The sweep takes a redirection off a line
before it splits the line into commands, and with `2>&1;` it took the semicolon too, so that two
commands became one: `kubectl logs … --tail=50 2>&1; echo ---` was asked as `kubectl logs …
--tail=50 echo ---`, which `kubectl` turns away for its `---` before it asks a server anything, on
a cluster and on a case alike — the same, and not what was typed. **Seven of the 83 are such commands, typed by no run, and five reads that a run did
type were asked of nothing**: four that stood behind such a join, and one on a line left out for a
`$` that stood inside quotes, where it needs no shell. Seventy-six of the 83 askings were of a
command as an agent typed it. And `promq` was never asked in any form: the sweep is of `kubectl`,
and 26 `promq` commands of these runs went unlooked at. The reading is repaired
(`test/replay-diff/replaydiff.py`, with cases in its `selftest.py`), and what the runs typed is
asked again in Part 4, outside the rule.

### R3 — a reading

The reader was a Claude model started with the instructions committed in Part 1 and nothing else.
It read the eighteen records and listed **ten answers, at medium confidence, all of them one
claim**: under `kubectl get events --sort-by=.lastTimestamp`, in four runs of `s1` and six of `s2`,
the scheduler's events stand among the others in order of time, where on a cluster — it reasoned —
they would stand first. The scheduler writes its events through `events.k8s.io`, with an
`eventTime` and no `lastTimestamp`; `kubectl` sorts what has no value before what has; so a cluster
lists `Scheduled` and `FailedScheduling` at the top, and a frozen case that sorts them by time has
given them a time they did not have.

It is a careful reading and it is not what this cluster does. R2's sweep had asked that very command
of a live cluster of each scenario: the scheduler's rows stand at places 0, 3, 5, 18, 22, 25, 29,
30 and 31 of 51 in `s1` and at 0, 8, 10, 12, 14 and 32 of 41 in `s2` — in both live answers and in
the frozen one, the same. The events in the snapshot say why: this scheduler writes
`deprecatedLastTimestamp`, and no `eventTime`.

**Ten listed, none confirmed, ten not confirmed, none not testable.**

### Described, and not claimed

| | runs | pass, both judges | judge 1 | judge 2 | all decisive evidence retrieved | mean steps |
|---|---|---|---|---|---|---|
| `s1-shared-cache-exhaustion` | 6 | 3 | 3 | 3 | 4 | 7.8 |
| `s2-periodic-saturation` | 6 | 4 | 4 | 4 | 5 | 9.8 |
| `s3-node-local-drift` | 6 | 0 | 0 | 0 | 0 | 2.5 |
| all | 18 | **7** | 7 | 7 | 9 | 6.7 |

The two judges agree on all eighteen (κ 1.00); judge 2 cost US$0.21 and the agent's harness
reported US$0.08 for the eighteen. Beside round 38's frozen cell for this agent, 5 of 18: **nothing is concluded**, as Part 1 said, and here for a reason it did not
foresee as well — eight of these eighteen are not investigations. Of the ten that are, seven passed
and nine retrieved all the decisive evidence; that is reported because it is what there is, and it
is ten runs of two cases.

The prediction — `kubectl describe pod` steps a run below 2.0 — held at 0.11, and says nothing:
two such steps in ten runs that ran, none in eight that did not.

### What can be said, and what cannot

That in ten investigations of two cases, on the replay as it is, nothing was found that a cluster
would not have said: by five patterns that had found twenty-nine such steps before, by putting to a
cluster most of what was typed, and by a reader who was told what kind of thing to look for and
not what had been found, and looked hard enough to be wrong in an interesting way.

Not that an investigation of `s3` meets nothing, since there was none. Not that the instrument is
sound: the adapter turned away a way of writing a command that the guard, `kubectl` and every
operator accept, and it took a change in how the commands came written, on no one's schedule, to
show it. No rule of Part 1 would have caught it; reading the first run's answer did. The
list was built from the guard's so that it could not refuse a read the guard allows; it was built
from the guard's *verbs*, and a command is more than its verb.

## Part 3 — a second set, its rule fixed before its runs

Not Part 1's runs again under another name: Part 2 stands as it is, with its eight refusals. This
is what is done about them, and it is written and pushed before any of its runs exists.

**The repair.** The adapter's list now has each read in each form it is written in: the verb
first, or a namespace first — `-n <namespace>`, `--namespace <namespace>`, `--namespace=<namespace>`
(`internal/agent/claude_code.go`). Tried against Claude Code 2.1.293 with a stand-in for `kubectl`
that only echoes: the namespace-first read runs; a namespace-first `delete` is refused; a second
command behind a semicolon is matched on its own, so `…; python3 -c …` is refused. One thing the
wider patterns do let through, and the code says so: the `*` after the flag is any text, so
`kubectl -n shop auth can-i get pods` is passed to the guard by the pattern for `get`, and the
guard, to which `auth can-i` is a read, runs it. The four commands the list withholds are withheld
for what they answer on a frozen case, not for what they can do; what a command can do is the
guard's to decide, and every `kubectl` goes through it.

**What is run.** Eighteen more runs, exactly as Part 1's table has them — the same three frozen
cases, the same agent and model word, six a case — with `lapilli-case` built from this branch at
the commit that carries this Part.

**The rules** are Part 1's, word for word: R1, R2 and R3, each with zero as its bar; the same
exclusions; the same limits on spending, counted afresh; the same things described and not
claimed, and the same prediction. The reader of R3 is started anew and is given only this set.

**One rule more, because Part 2 is why this set exists: R0 — a command runs.** In every one of the
eighteen runs at least one step does not fail, and no step that begins `kubectl -n` or
`kubectl --namespace` is refused by Claude Code's list. A run without one, or a refusal of that
form, is a miss of R0 and is reported as one; it does not excuse the run from any other count.

**What this set cannot show** beyond what Part 1 already lists: that the repair is why a run
investigates. The model behind `haiku` changed its habits once without notice and may again
between two sets an hour apart. Part 4 will say what was typed.

## Part 4 — the second set

Eighteen runs, made on 2026-10-07 between 22:19 and 22:23 UTC with `lapilli-case` built at
`2b7ef93`, the commit that carries Part 3 and was pushed before them. No provider failed; none was
excluded or repeated. The records are in `test/fixtures/case-runs/2026-10-08-round39-second/`.

### In short

- **R0 held: a command ran in every run**, and none that began with a namespace was refused — in
  the five runs of the eighteen that wrote one so.
- **R1 held: no sign in any run**, over 163 steps that did not fail — 27 of which three of the five
  patterns could not have matched whatever they showed.
- **R2 held as it was run: of the 176 commands the sweep asked, none differs** — 161 of them a
  command as an agent typed it.
- **R3 held: the reader listed two answers, at low confidence, and a cluster gives the same.**

Four rules, eighteen investigations, three cases. Under the rules this round set itself, **no
answer that a cluster would not have given was found in an investigation on the replay as it is.**
What those rules' instruments could not see, and what was done about it afterwards, is under
*After the fact*.

### R0 — a command runs

| case | runs | in which a command ran | steps that did not fail | begun with a namespace, and ran | refused by Claude Code |
|---|---|---|---|---|---|
| `s1-shared-cache-exhaustion` | 6 | 6 | 60 | 15 | 0 |
| `s2-periodic-saturation` | 6 | 6 | 53 | 5 | 2 |
| `s3-node-local-drift` | 6 | 6 | 50 | 17 | 0 |
| all | 18 | **18** | 163 | 37 | 2 |

The two refusals are `for` loops, one of them behind a `cd /tmp;`. The guard refused one command,
inside a step that otherwise ran: `kubectl get --raw` on a path that is a proxy to a service — a
request sent, not something read, which it refuses on a cluster as on a case.

The namespace stood first in 37 steps of **five runs**: two of `s1`, one of `s2`, two of `s3`. In
the first set it had been eight runs, all six of `s3` among them; in the other thirteen here the
agent wrote the verb first throughout, as it had in round 38. The half of R0 that is about the
repair was tried by five runs, then, and not by eighteen.

### R1 and R2

`signs.py`: none of the five signs, in any of the eighteen records.

The sweep read 227 `kubectl` commands out of the runs' 165 steps. It left out five command lines as
needing a shell, and one command was the `get --raw` above. The other 226 are 176 different
commands:

| case | asked | the same | the cluster moved | differ |
|---|---|---|---|---|
| `s1-shared-cache-exhaustion` | 67 | 67 | 0 | 0 |
| `s2-periodic-saturation` | 49 | 49 | 0 | 0 |
| `s3-node-local-drift` | 60 | 60 | 0 | 0 |
| all | **176** | 176 | 0 | **0** |

The whole sweep, 1,601 commands with the fixed set, differs on twelve, of the three known kinds.

As in Part 2, and found by the same reader: where a redirection stood before a semicolon the sweep
had made one command of two. **Fifteen of the 176 are commands no run typed, twelve reads that a
run typed were asked of nothing, and 161 askings were of a command as an agent typed it.** The 37
`promq` commands of this set were not asked either.

### R3 — a reading

A reader started anew, with the same instructions and only this set, listed **two answers, both at
low confidence and both one claim**: `kubectl describe node` in one run of `s3` shows two or three
events for the node, where other steps of the same run show eight or nine in the snapshot — the kubelet's
and kube-proxy's own, `Starting kubelet.`, `NodeHasSufficientMemory` and the rest. `kubectl
describe node` asks for a node's events twice, it reasoned, once by the node's UID and once by its
name, which is what the kubelet writes in the UID's place; a frozen case that answers only the
first has lost the second.

R2's sweep had asked `kubectl describe node rcabench-worker2` and `rcabench-worker` of a live
cluster: it lists `RegisteredNode` and `NodeReady` and no more, in both live answers and in the
frozen one. The cluster does not list the kubelet's events there either.

**Two listed, none confirmed, two not confirmed, none not testable.** Twelve items in the two sets
were two claims, each a careful argument from how Kubernetes works to what a cluster must print,
and each time the cluster printed what the frozen case had. That is the use of asking the cluster.

### Described, and not claimed

| | runs | pass, both judges | judge 1 | judge 2 | all decisive evidence retrieved | mean steps |
|---|---|---|---|---|---|---|
| `s1-shared-cache-exhaustion` | 6 | 5 | 5 | 5 | 5 | 10.0 |
| `s2-periodic-saturation` | 6 | 6 | 6 | 6 | 6 | 9.2 |
| `s3-node-local-drift` | 6 | 1 | 2 | 1 | 3 | 8.3 |
| all | 18 | **12** | 13 | 12 | 14 | 9.2 |

The judges disagree on one run of `s3` (κ 0.87); judge 2 cost US$0.28 and the agent's harness
reported US$0.13. The prediction held: 0.28 `kubectl describe pod` steps a run, five in all.

Beside round 38's frozen cell for this agent — 5 of 18 passed, 8 of 18 with all the evidence —
these are 12 and 14. The difference in passes is 39 points and its 95% interval, +6 to +62, does
not contain zero. **Part 1 said nothing would be concluded from it, and nothing is**: not that the
repaired replay made an agent better, and not that it did not. The two rounds differ in more than
the replay, as Part 1 said they would: these runs cost a fifteenth of round 38's (US$0.007 a run
against US$0.111), took a third of the time (25 seconds against 80) and less than half the steps
(9.2 against 20.9), and no record says what changed in the twelve hours between — the model, the
harness, or both. One thing is worth writing down as an
observation for whoever runs `s3` next: its second evidence item, `max_open_files=64`, was
retrieved in 1 of round 38's 24 runs of that case, by either agent in either condition, and in 3 of
these 6.

### What a review of the repair found

The repair of Part 3 was read after this set had run, while its sweep did, by a reviewer that had
not written it and was given the diff, the guard's code, and a way to put command lines to Claude
Code with a stand-in for `kubectl` that only echoes. It found no way to write to the machine or to
reach another cluster: every shell escape it tried was refused by Claude Code, and every write,
every other identity and every other kubeconfig that the wider patterns passed on was refused by
the guard. That is what it tried, and not all there is to try. It found the repair wrong in three
other ways:

- **It was not finished.** `kubectl -nshop get pods` and `kubectl -n=shop get pods` were refused
  still, and so was `kubectl -v6 -n shop get pods`: spellings the guard accepts and the list did
  not have. Part 2's failure, for the next way of writing a flag.
- **It let through what the list exists to withhold.** A `*` in a pattern is any text, so the
  pattern for `get` passed `kubectl -n shop auth can-i get pods` to the guard, to which `auth
  can-i` is a read. Part 3 says so, and calls it acceptable. It is a command an agent writes
  without any trick, and it made the withholding of `auth` void.
- **Its test asserted the wrong thing**: that no pattern names a withheld command, which was true
  while the command could be run.

It also wrote down something no list changes. Claude Code runs some commands of its own without
asking, whatever it is allowed: `id`, `whoami`, `pwd`, `hostname`, `uname -a`, `date`, `which` and
`ps aux` all ran with nothing permitting them. Reading a file outside the working directory and
printing the environment were refused. An agent run through this adapter can learn whose machine it
is on and see its process table; `design-case.md` §4 says so now.

### The adapter as it is merged

Three lists were wrong in the same way — round 38's lacked a verb, the one Part 2 ran with knew a
verb only in first place, Part 3's knew two places and not every spelling of them — and the way is
that Claude Code matches a command as text, and a list of texts is not a reading of a command. So
the list no longer tries. **Claude Code is given `kubectl` whole, and every `kubectl` it
runs is the guard**, which reads a command as `kubectl` does: what is a read, where its verb stands,
which flags may come before it. What this adapter withholds — `auth`, `cluster-info`, `config`,
`explain` — is withheld there too: the adapter names them in the agent's environment, and the guard
refuses a command whose verb is one of them, wherever its namespace stands and whatever word comes
after (`internal/guard`, `NotOffered`; `internal/agent/claude_code.go`).

The reviewer read that as well and tried it the same way. Under `kubectl` whole, every write it
could name is refused by the guard, every flag that reads or writes a file, every plugin, every
other cluster and identity; no spelling of a flag it tried walks a withheld verb past; and it found
no line by which an agent reaches the variable: each one it tried that would change the environment
begins with something other than `kubectl`, and Claude Code refused it. Claude Code also runs
commands nobody listed, as above, so this is a way not found and not a way shown closed. It found
one thing: **a word that is empty.** `kubectl "" delete pod x` has nothing where its verb stands, the guard checked a
verb only if it was not empty, and it passed the line on with `delete` in it. `kubectl` itself would
most likely have run nothing — the reviewer did not try, being forbidden the real one — but that is
`kubectl`'s reading of an edge and not the guard's refusal. It had been so since the guard was
written. This adapter's first two lists happened to stand in front of it; Part 3's patterns would
have passed such a line on, and an agent run through the `command` adapter, which has no list,
could always have typed one. The guard now holds
whatever stands in the verb's place to be a read, an empty word included, and a test runs the line
through the whole chain an agent's `kubectl` goes through.

**No run of either set used the adapter as it is merged.** The second set ran with Part 3's
patterns. What the merged one changes is which command lines reach the guard, not what a case
answers; R1, R2 and R3 are about the answers and stand. R0 is about the list, and was measured on
the list before this one.

Three runs were then made with it, one a case, to see that it works and not to measure anything
(`test/fixtures/case-runs/2026-10-08-round39-after/`; the tool built in a tree at `2628f48`, the
commit of the adapter and the guard, with these documents not yet committed): twenty-five steps,
every one of which ran,
nothing refused by Claude Code or by the guard, none of the five signs. None of the three happens
to write a namespace first. That form is held by tests of the guard, of the adapter and of the
whole chain, and by the reviewer's command lines; not by a run.

### After the fact

Parts 2 and 4 were read, with the records and without the author's account of them, by a reader
that had written none of it. It found two places where a rule's instrument did not see what the
rule says it looks at, and both were looked at again. **None of what follows is under a rule**: it
was done with every result known, and it is here so that what R1 and R2 are worth can be judged.

**R1's patterns.** Three of the five signs look for `kubectl get` with nothing between the two
words. They are round 38's patterns, and in round 38 nothing ever stood there; here an agent wrote
`kubectl -n shop get pods -o wide`, and a step so written could not be counted by those three
whatever it showed. In the first set that changes nothing: every such step was refused, and R1
counts steps that did not fail. In the second, 27 of the 163 steps are so written.
`test/round39/posthoc.py` is the same count with flags allowed before the verb. On round 38's
records it finds what `signs.py` finds, 34 signs in 29 steps of 15 runs; **on the first set, the
second, and the three runs after, none**
(`test/fixtures/case-runs/2026-10-08-round39-after/signs-again.md`).

**R2's reading of a command line.** With the reading repaired, the three scenarios were swept once
more, the commands an agent typed being those of all thirty-nine runs of this round — both sets
and the three runs after:

| case | typed, and asked | the same | the cluster moved | differ |
|---|---|---|---|---|
| `s1-shared-cache-exhaustion` | 91 | 90 | 1 | 0 |
| `s2-periodic-saturation` | 76 | 76 | 0 | 0 |
| `s3-node-local-drift` | 66 | 66 | 0 | 0 |
| all | **233** | 232 | 1 | **0** |

The reading takes 406 `kubectl` commands from the thirty-nine records. Two are not reads — the
`exec` and the `get --raw` — and the other 404 are these 233, each asked as it was typed. The whole
sweep, 1,656 commands, differs on twelve, of the three known kinds
(`test/fixtures/case-runs/2026-10-08-round39-after/sweep-again/`). Round 38's records are read
alike by the old reading and the new, 276 commands either way: the fault had been in the sweep
since it was written, and nothing typed before this round touched it.

**What was not asked, and still is not.**

- Five `for` loops, whose `kubectl` has a `$d` where its name should be: no command to ask until a
  shell has run the loop. Claude Code refused all five, so no answer to them is in a transcript.
- Two steps that ran, each holding a command written over three lines — a Go template with its
  line breaks inside the quotes. The reading goes line by line and takes nothing from either: four
  reads, of two ConfigMaps, that were answered and never compared.
- **The 66 `promq` commands.** The sweep compares `kubectl`. What a case's metrics answer was set
  beside what the Prometheus it was frozen from answers once, by hand, for five queries
  (`design-case.md` §3); never for a query an agent typed, and no test does it (`ROADMAP.md` §7,
  1e).
- And the reader was one reader. It found what it found in the places it looked hardest, after the
  author had read the same pages and passed them.

### What this round leaves

- **The question it asked has an answer, at the size it was asked.** In eighteen investigations of
  three cases by one agent, nothing a cluster would not have said was found by four rules fixed
  before they ran; nor, by three, in the ten of the first set that got as far as investigating. Two
  of the rules' instruments saw less than the rules say, and the look that made up for it found the
  same nothing, outside any rule.
- **The instrument was wrong in a place no one was looking, and no rule caught it.** Part 1 named
  the guard; the adapter's list was never thought of as something that could stop an
  investigation, because it had been built from the guard's own. What showed it was the first
  run's answer, read; what brought it about was commands coming written another way.
- **The round's own instruments were wrong in two more places, and a reader found both**: a sweep
  that made one command of two, and patterns that could not see a namespace before a verb. Neither
  changed what was found. Both were written by the author, and passed by the author when Parts 2 and 4 were.
- **A measurement made through a model alias and a harness is a measurement of that day.** The same
  word `haiku` and the same adapter, twelve hours apart: a fifteenth of the cost, a third of the
  time, less than half the steps, and a way of writing a command not seen once in 739 steps before.
  No record says what changed. Round 38's numbers are about what answered then.
- **Still not done**: a judge who is a person; a stronger agent; a case written by someone else;
  an agent on the kinds these three cases lack; `promq` compared as `kubectl` is (`ROADMAP.md` §7).
