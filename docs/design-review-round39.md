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

Part 1 is written and committed before any run. Part 2 is the result, under Part 1's rules.

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

Eighteen runs, made on 2026-10-07 between 21:56 and 22:00 UTC, with `lapilli-case` built from
`main` at `ffc133f` — the commit that put Part 1 and its instruments there. No provider
failed; none was excluded or repeated. The records, the packets, both judges' verdicts, the reader's
list and the sweep's reports are in `test/fixtures/case-runs/2026-10-08-round39/`.

### In short

- **R1 held: no sign in any run.**
- **R2 held: of the 83 commands the runs typed that could be asked, none differs.**
- **R3 held: the reader listed ten answers, all one claim, and a cluster gives the same answer.**
- **And the round did not do what it was for.** In eight of the eighteen runs no command ran at
  all. The agent wrote the namespace before the verb — `kubectl -n ledger get pods -o wide` — the
  adapter's list of what Claude Code may run knew each read only with the verb first, and after one
  to three refusals the agent answered that it could not investigate. All six runs of
  `s3-node-local-drift`, and two of `s1`. Part 1 said what would count against the instrument: *a
  refusal by the guard of something a read-only investigation needs*. The guard refused nothing. The
  adapter refused twenty-one reads the guard allows, and that is what the sentence was written
  for: **it counts.** Three rules held over ten investigations and eight refusals.

### What was run, and what ran

| case | runs | in which a command ran | steps that did not fail | refused by Claude Code | refused by the guard |
|---|---|---|---|---|---|
| `s1-shared-cache-exhaustion` | 6 | 4 | 39 | 7 | 0 |
| `s2-periodic-saturation` | 6 | 6 | 54 | 5 | 0 |
| `s3-node-local-drift` | 6 | **0** | 0 | 15 | 0 |
| all | 18 | **10** | 93 | 27 | 0 |

Of the 27 steps Claude Code's permission list refused, 21 are the 21 steps of the eight runs that
ran nothing, and every one begins `kubectl -n <namespace>`. The other six are what round 38
also saw and no list of commands changes: a pipe into `sed`, a `for` loop, `echo "exit=$?"`, and one
`kubectl exec`, which is not a read. The twenty-eighth failed step is the cluster's own: `the server
doesn't have a resource type "ciliumnetworkpolicy"`.

In round 38 the same agent, under the same word `haiku`, began no command of its 739 steps with
`kubectl -n`. It also took four times as long over three times as many steps, and its harness
reported twenty-four times the cost: 80 seconds, 20.9 steps and US$1.99 for its eighteen frozen
runs then; 19.5 seconds, 6.7 steps and US$0.08 for these. Part 1 said the model behind `haiku` is
whatever it is on the day. On this day it was one that writes the namespace first.

### R1 — none of the answers round 38 found

`signs.py` over the eighteen records: **no step shows any of the five signs.** Nothing was set
aside, because nothing was counted.

| what the step shows | round 38, agent A, frozen: steps, runs | round 39: steps, runs |
|---|---|---|
| an empty sorted list | 11, 9 of 18 | 0, 0 of 18 |
| a pod listing asked for `-o wide`, without NODE | 9, 8 of 18 | 0, 0 of 18 |
| a listing whose whole header is `NAME AGE` | 10, 8 of 18 | 0, 0 of 18 |
| an event listing headed `LASTTIMESTAMP` | 0, 0 of 18 | 0, 0 of 18 |
| `kubectl get all` with names and no kinds | 4, 4 of 18 | 0, 0 of 18 |
| any of them | 34, 15 of 18 | **0, 0 of 18** |

It holds over less than was meant. Ten runs had answers to show a sign in, and the case in which
round 38 found most of them — `s3`, in each of its six runs — has none here because it has no
answers here.

### R2 — what the agent typed, put to a cluster

The eighteen runs typed 135 `kubectl` commands in 121 steps — the refused ones among them: the
sweep asks what was typed, whether or not it ran. Seven command lines needed a shell to mean
anything and were not asked; one was `exec`. The other 134 are 83 different commands, and the
sweep put every one of them to a live cluster built from the scenario, to its frozen copy, and to
the cluster again:

| case | asked | the same | the cluster moved | differ |
|---|---|---|---|---|
| `s1-shared-cache-exhaustion` | 36 | 35 | 1 | 0 |
| `s2-periodic-saturation` | 41 | 41 | 0 | 0 |
| `s3-node-local-drift` | 6 | 6 | 0 | 0 |
| all | **83** | 82 | 1 | **0** |

The whole sweep — these and the fixed set, 1,514 commands — differs on twelve, of the three kinds
`known.txt` has always named.

### R3 — a reading

The reader was a Claude model started with the instructions committed in Part 1 and nothing else.
It read the eighteen records and listed **ten answers, at medium confidence, all of them one
claim**: under `kubectl get events --sort-by=.lastTimestamp`, in four runs of `s1` and six of `s2`,
the scheduler's events stand among the others in order of time, where on a cluster — it reasoned —
they would stand first. The scheduler writes its events through `events.k8s.io`, with an
`eventTime` and no `lastTimestamp`; `kubectl` sorts what has no value before what has; so a cluster
lists `Scheduled` and `FailedScheduling` at the top, and a frozen case that sorts them by time has
given them a time they did not have.

It is a good reading and it is not what this cluster does. R2's sweep had asked that very command
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

The two judges agree on all eighteen (κ 1.00); judge 2 cost US$0.21. Beside round 38's frozen cell
for this agent, 5 of 18: **nothing is concluded**, as Part 1 said, and here for a reason it did not
foresee as well — eight of these eighteen are not investigations. Of the ten that are, seven passed
and nine retrieved all the decisive evidence; that is reported because it is what there is, and it
is ten runs of two cases.

The prediction — `kubectl describe pod` steps a run below 2.0 — held at 0.11, and says nothing:
two such steps in ten runs that ran, none in eight that did not.

### What can be said, and what cannot

That in ten investigations of two cases, on the replay as it is, nothing was found that a cluster
would not have said: by five patterns that found thirty-four such answers before, by putting every
command to a cluster, and by a reader who was not told what to look for and looked hard enough to
be wrong in an interesting way.

Not that an investigation of `s3` meets nothing, since there was none. Not that the instrument is
sound: the adapter turned away a way of writing a command that the guard, `kubectl` and every
operator accept, and it took a change in the model's habits, on no one's schedule, to show it. The
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
