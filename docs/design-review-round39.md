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
