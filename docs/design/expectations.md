# Expectations: what a rule was tested to do, kept and held to

Status: accepted; steps 1 to 5 built (see "As built" at the end). Under [GOVERNANCE.md](../../GOVERNANCE.md)
this is a new kind of data in the ledger, and a backtest that can fail a
pull request, so it was designed first, as a pull request (#81); it was
merged on 2026-11-16, before its week was out, by the maintainer's
decision. Its open questions stay open until the step that needs each. The companion to
[changes.md](changes.md), which applies what a person approves, and to
[work.md](work.md), whose tasks a broken expectation becomes.

## Why

Lapilli tests a change before it is made: `--compare` replays the rules
proposed beside the rules in place, the pull request backtest comments what
the change does, and MCP's `compare` replays a rule an agent proposes. Each
of these says something once, and then it is gone:

- The next change isn't held to it. A pull request that makes a rule fire
  forty times a week gets a comment saying so, and merges.
- The weeks after aren't held to it. A rule backtested at three firings a
  week that fires thirty, once traffic changes, is noticed when someone
  looks at its page.
- Nothing says what a rule is for. A rule's backtest says how often it
  fires and what it caught, not how often it should fire or what it must
  keep catching.

Other tools cover parts of this, for Prometheus's own rule files:

- `promtool test rules` tests a rule against series written by hand
  (`input_series`), and checks which alerts fire at a time (`exp_alerts`):
  a scenario, not the history.
- pint lints rules in CI and can fail the build (`pint ci`, exit code 1 on
  a problem of severity Bug or higher). Its `alerts/count` check runs a
  rule's `expr` against a live Prometheus, over a day by default, and
  reports how many alerts it would make; `minCount` sets when it reports,
  at severity info. Its `promql/series` check, and `pint watch` every ten
  minutes after deploy, say when a series a rule reads has disappeared.
- pint says it is "not intended to be used with other services that offer
  partial compatibility with Prometheus", with no plans to add any.

None of them judges a rule against incidents, holds it to how often it may
page, or checks after deploy that it fires as it was tested to; and none
covers Grafana-managed rules. For those, no automated test was found:
Grafana's TestData data source is a playground in the UI, and a question on
Grafana's forum asking for one (2024) has no answer. Lapilli replays
Grafana-managed rules as Grafana evaluates them ([states.md](states.md)),
so it can test them where nothing else does.

There is a second reason. Lapilli's verdict needs incidents written down: a
deployment that writes none gets "not known" for precision and noisy rules
([0.41.0](../../CHANGELOG.md)), however long it runs. A team can still say
what it expects of a rule: that it fires at most ten times a week, at most
twice while people sleep. That needs no record, and it is what an on-call
team argues about.

## What an expectation is

An expectation is what a rule is held to, written beside it, of three kinds:

- **A budget.** How often the rule may fire in a week; how often while
  people slept, by the hours the rule's team keeps (`--sleep-hours`, a
  team's own); how many re-fires. Counted by the replay as the rule fires:
  its `for`, `keep_firing_for`, Grafana's states, quiet windows excusing
  what they excuse.
- **Catches.** Incidents the rule must keep catching, and how soon: `INC-43
  within 5m`. Judged as the verdict judges them, so an incident caught by
  another rule first still counts as caught by this one if this one caught
  it within the delay.
- **Scenarios.** `promtool test rules` files that test the rule: run as
  promtool runs them. For rules promtool can't read, see the open
  questions.

Expectations are kept where the rules are kept, so a pull request changes
both together and the diff shows it:

```yaml
# expectations.yaml, beside the rule files
expectations:
  - rule: ContainerRestarts          # a rule's name; or group:, or labels: as --rules-select takes
    budget:
      week: 10                       # firings in each whole week
      asleep: 2                      # of them, while people slept
    catches:
      - incident: INC-43
        within: 5m
    scenarios:
      - tests/restarts.test.yaml     # promtool's own test files
```

A rule kept only in Grafana or a ruler, with no file in git, takes its
expectations from serve's declared configuration (`--config`,
[serveops.md](serveops.md)), under the same key. A rule may carry a short
budget in an annotation instead (`lapilli.dev/budget: "10/week, 2 asleep"`),
for teams that keep everything in the rule.

A budget is held each whole week of the period it is checked over, and the
week that fired most is the one reported. A period with no whole week, such
as a pull request backtest over three days, holds the budget to the days it
has, scaled, and says so. Catches need incidents: with none written down,
they are "not known", never met or broken. A budget needs no record.

## Before a change is merged

`lapilli verdict --compare` takes `--expect FILE`. Its report gets a section:
each expectation of a rule the change touches, before and after, as met,
broken or not known, with the numbers that decided it.

```
Expectations: 1 broken by this change, 0 already broken, 3 met
  ContainerRestarts  budget week 10   before 3, after 41   broken
                     budget asleep 2  before 0, after 17   broken
                     catches INC-43   before +2m, after +2m  met
```

`--fail-expectations` exits 1 when the change breaks an expectation that the
rules in place meet. An expectation already broken before the change is
reported, and fails nothing: a pull request isn't held to fix what it didn't
break. The backtest recipes take `FAIL_ON_EXPECTATIONS=true`, so the job
fails and a branch rule can require it. Today the job fails only when the
backtest itself fails (`FAIL_ON_ERROR`).

A pull request that changes `expectations.yaml` is read like one that
changes rules: from the commit it goes into for "before", from the pull
request for "after", with the change shown (`budget week 10 → 50 in this
pull request`). Loosening a budget is a change a reviewer sees, not a way
around the check. From forks, the same care as for rules applies: what the
pull request holds is read, never run.

A rule a pull request adds that has no expectation is reported as such;
`--expect-required` fails the job for it.

## After it is deployed

`lapilli serve` takes `--expect FILE` (the chart's `serve.expectations`, as
a ConfigMap, read again when it changes, as `--config` is). After each
hourly run, it checks each rule's expectations over the week so far and the
twelve whole weeks before:

- **A budget broken.** A rule that fired more than its budget in the week.
  The week so far is reported once it is over budget, not only at its end.
- **A catch lost.** A new version of a rule, or a new incident written down
  that the rule was expected to catch, judged again.
- **A scenario failing.** Run again when the rule's version changes, where
  scenarios can run (`--promtool PATH`).

What is broken shows on the rule's page, on a tab of its expectations with
each one's weeks. It is a line in the overview's "Look at these first",
which a person makes into a task with one click, as the others there; it is
counted in the weekly digest, and told through the outbox, once per rule per
expectation per week, where notices are set. With `--expect-tasks`, Lapilli
opens the task itself, for the rule's team: the first time Lapilli makes a
task nobody asked for, so it is off unless turned on ([work.md](work.md)).
"Not known" is shown as such, never as met or broken. A rule with no expectations is listed apart
on the rules list, so a team can see what isn't held to anything.

## Drafts: from the history, and from an agent

Nobody writes tests for rules that already page them, so expectations need
a first draft. Two ways, neither of which lets Lapilli write a rule or apply
anything:

- **From the history.** For each rule, the most it fired in a week over
  twelve weeks, and the most while people slept, rounded up, written as a
  budget "as it is now", and the incidents it caught as catches. A team
  starts from what is, and lowers it. No language model; the same numbers
  each time.
- **From an agent.** An agent connected over MCP, with whatever model it
  runs, drafts what the history can't: a budget lower than today's with a
  reason, and scenarios, written as promtool test files, of the failure the
  rule is for. Lapilli checks each draft against the rule as it is: a
  budget against the twelve weeks, a scenario by running it. It returns,
  per item, met, broken or couldn't be run. A draft is kept for a person to
  accept or drop. Accepting one is a change like any other: a pull request
  to `expectations.yaml`, or, kept in serve's configuration, written there
  ([changes.md](changes.md)).

When a pull request breaks an expectation, an agent may read the report
and propose rewrites of the rule: a `for`, a threshold, but also an
aggregation, a `> 3` for a `> 0`, a warning split from a critical, which
Lapilli's own suggestions don't reach. Each rewrite is replayed against the
rule's expectations, and only those that meet all of them are posted, marked
as an agent's, each with its backtest. A rewrite that meets the budget by
losing a catch is never shown.

Three MCP tools carry this:

| Tool | What it does | Kept |
|---|---|---|
| `test_expectation` | Checks an expectation, or a rewrite of a rule, against the rule and its history | nothing |
| `propose_expectation` | The same, kept as a draft a person accepts | a draft |
| `compare` (as now) | Replays a rule an agent proposes beside the rule in place | nothing |

Lapilli itself calls no model and holds no model's key: which one an agent
runs is the agent's, as which cloud a rule runs on is the rule's. An agent's
drafts count toward the person's hourly limit, as its proposals do, and an
agent can't accept its own draft ([0.38.0](../../CHANGELOG.md)).

## Adding a rule

Proposals change rules that exist: a `for`, a threshold, turning one off,
deleting it, what it says, undoing a change. A rule added, held to its
expectations before it is approved, is the step after this one. It needs a
place to go (a file, a group, a folder, a cloud's alarms), which the person
proposing it gives, and then goes the four paths a change goes. This design
named it so expectations were made with it in mind, and left it out of the
first four steps.

Decided by the maintainer, 2026-11-20: **all four paths**, the git path,
Grafana, the rulers and the clouds' alarms, each making the rule as undoing
a deletion already makes one there, and taking it out, once added, as a
deletion does. Built as step 5 ("As built").

## Not in this design

- **Who a firing pages.** Alertmanager's `amtool config routes test` says
  where a set of labels is routed; Lapilli doesn't read routing
  ([notifications.md](notifications.md)).
- **A series that disappeared.** pint's `promql/series` and `pint watch` say
  it, before and after deploy.
- **Text.** Lapilli writes no expectation, test or rule itself: drafts come
  from the history, which is counted, or from an agent, which a person
  accepts.

## In the ledger

Expectations are declared, in a file or serve's configuration, not made in
the UI: the ledger keeps what checking them found, not what they are. Two
tables, each in the schema of the step that needs it (a schema takes the
next free number when its pull request opens: this design's first text
said 020, which goals took):

- `expectation_checks` (step 2, schema 027): by rule, expectation (a hash
  of its text), week, and result (met, broken, not known), with the
  numbers. The rule's tab and the digest read it.
- `expectation_drafts` (step 3; built in 027, unreleased, in place, not a
  028 of its own): an agent's or the history's draft, who proposed it,
  what checking it found, and whether a person accepted or dropped it.

As with every schema change, a release before 027 doesn't open a ledger at
027: upgrading adds two tables, with nothing to backfill, and going back
takes the ledger as it was before, so a deployment keeps a copy of it (a
snapshot of its disk, or of its database) before it upgrades.

## Three cases

1. A pull request adds a restart rule. Over the fourteen days the backtest
   reads, it fires forty a week, seventeen while people slept; its
   expectation, written with it, says ten and two. The job fails, and the
   comment says by how much. The author raises `for` from 1m to 10m, which an agent's
   rewrite, checked by `test_expectation` and posted by the agent, also
   suggests: eight and one, and the incident it was written for is still
   caught. It passes.
2. Two weeks after a rule goes out, a release doubles a service's traffic,
   and the rule fires twenty-six times in a week against a budget of five.
   The next hourly run puts it first on the overview and tells the rule's
   team, and the digest says it that Monday. Before, someone noticed when
   they looked.
3. A pull request raises a 5xx threshold from 2% to 5%. INC-43, a 3.4%
   error rate for twenty minutes, is no longer caught. The job fails on the
   catch, with the incident named.

The first two need no incident written down.

## Open questions

1. **Scenarios for rules promtool can't run.** Grafana-managed rules, and
   rules on a cloud's own monitoring, have no promtool. Two ways: embed
   Prometheus's PromQL engine with its test storage, as promtool does, and
   evaluate Grafana's expressions on it with Lapilli's own
   (`internal/sse`); or backfill a throwaway Prometheus with the scenario
   and replay against it. The first is a large dependency; the second is a
   process to run. Neither is in the first steps.
2. **A team's budget.** How many times a team is paged in a week, over all
   its rules, is what on-call people feel. It is a budget of a team, not a
   rule. It could be a fourth kind, checked the same way. Answered by
   [goals.md](goals.md) (#91): a team's budget is a goal of the team's
   (`critical.firings` or `firings`, `at_most`), not a fourth kind of
   expectation.
3. **Default budgets.** Whether a team may set a budget every rule of it is
   held to unless the rule says otherwise, or every rule must say its own.
   Decided by the maintainer, 2026-11-20: **none**. Every rule says its
   own; a team's total is a goal of the team's ([goals.md](goals.md)), and
   step 3's drafts from the history fill every rule's budget at once.
4. **The week.** Whole weeks as the ledger's (Monday, in `--tz`), or a
   rolling seven days for the week so far. Decided by the maintainer,
   2026-11-20: **the ledger's weeks** (Monday, in `--tz`), the week so far
   reported as soon as it is over budget.
5. **Agents' rewrites on a failed pull request: who posts them.** "Only
   those that meet all of them are posted" doesn't say by whom: Lapilli
   would need to write to the pull request, a write path of its own.
   Decided by the maintainer, 2026-11-20: **MCP only returns the text to
   post.** `test_expectation`, given a rewrite of a rule, replays it
   against the rule's expectations; only when it meets all of them, losing
   no catch, does its answer include a Markdown block a person or the
   agent may post on the pull request, marked as an agent's rewrite
   backtested by Lapilli, with its backtest (the numbers before and
   after). Otherwise the answer has each item's result and no such block.
   Lapilli writes nothing to the pull request and gains no write path for
   it: posting is the agent's own access, or a person's.

## Steps

1. Budgets and catches in `--compare` and the backtest recipes, with
   `--fail-expectations`. No schema change; the PR check is the first use.
2. serve's checks after deploy: the rule's tab, the overview line, tasks,
   notices, the digest. Schema 027.
3. Drafts: from the history, and MCP's `test_expectation` and
   `propose_expectation`; agents' rewrites on a failed pull request.
4. Scenarios: promtool test files, for Prometheus-format rules
   (`--promtool PATH`).
5. Adding a rule, as a proposal held to its expectations (all four paths,
   decided 2026-11-20).

## As built

Step 1, in `lapilli verdict --compare` and the backtest recipes:

- **The file.** `expectations:`, each naming a `rule` or a `group` (one of
  them), with a `budget` (`week`, `asleep`) or `catches` (`incident`,
  `within`). Read strictly, 1 MiB at most, before anything is replayed.
  Labels as a selector, re-fires in a budget and `scenarios` aren't read
  yet: a file with them is refused, rather than passed over (step 2 reads
  the first two).
- **The weeks.** Whole weeks counted back from the period's end, the most
  in one reported; a period with none is scaled to a week (`scaled_days`).
  While people slept is by `--sleep-hours` (and `--shifts`, `--on-call`),
  as the rest of the verdict counts it; with hours of no sleep, not known.
- **What a change broke.** Broken after, and met before or a rule the
  change adds. A rule the change takes away breaks its catches (it catches
  nothing), not its budget. `--expect-before` holds the side before to the
  expectations as they were, matched by the same `rule:` or `group:`: one
  raised is held to its old value before it; one added, on both sides; one
  taken away (whole, or a field, or a catch) is shown, held to what it
  was, and fails nothing, as a raised one doesn't.
- **Names told apart.** Rules sharing a name are named apart in a run
  (`CheckoutErrors [severity=page]`); `rule: CheckoutErrors` holds each,
  and so a rule whose own name begins `CheckoutErrors [`. How names are
  told apart can change as rules are added; a written name told apart is
  for a rule that will stay so.
- **Bounds.** 2,000 expectations and 2,000 catches in a file; the rules
  that differ held to 100,000 at most, each rule to each budget and catch
  that holds it, past which the run fails, saying what to do (a catch by
  `rule:`, not `group:`; fewer rules compared at once); each side's
  firings counted once, by rule and week, whatever the expectations.
- **Out.** A section of the text, `--markdown` (right after the totals) and
  `--json` (`"expectations"`); four summary fields (`expected`,
  `expectations_broken`, `expectations_already_broken`, `unexpected`).
  `--fail-expectations` and `--expect-required` exit 1. The recipes take
  `EXPECT`, read from both commits, and `FAIL_ON_EXPECTATIONS`,
  `EXPECT_REQUIRED`, which fail the job once the comment is posted, from the
  summary, not lapilli's exit status: the backtest didn't fail. Held to
  them, a run that can't judge them fails too: the file unreadable, a rule
  that differs not replayed, no report; and one that broke them whose
  comment couldn't be posted. A file the pull request takes away is read
  as holding none.
- **One rule, one entry.** Entries for the same `rule:` or `group:` are
  made one when read; the same budget or incident twice is refused. A rule
  moved out of a group that held it is said: the group's expectation no
  longer holds it, failing nothing.
- **Serve** takes none of it yet (step 2).

Step 2, serve's checks after deploy, as built:

- **Three sources, one list.** `--expect FILE` (the chart's
  `serve.expectations`, a ConfigMap of its own) is read every 15 s and
  kept to once read the same twice in a row, 15 s apart, as `--config` is;
  one that can't be read changes nothing and is said (the status line,
  logged once). `expectations:` in `--config` is read as an expectations
  file is, strictly (a key an entry doesn't have is refused with its line
  in the config), and kept to as the ledger is kept to the config. The two
  are one list, read as one file (`verdict.NewExpectations`): a rule's, a
  group's or a selector's entries in both are made one, and the same budget
  or incident given in both is refused, the two kept as they last could
  be, and said. Nothing is checked until both are read in the leader's
  term. `GET /config.yaml` gives no `expectations:`: the ledger doesn't keep
  them.
- **The annotation** `lapilli.dev/budget` is the rule author's, not the
  operator's: read from the latest version's rules, strictly (parts
  separated by commas, `N/week`, `N asleep`, `N refires` or `N re-fires`,
  each once, 0 to 1,000,000, 100 bytes), it holds the rule by its own name.
  A budget the file or `--config` give that rule by the same `rule:` wins,
  and the annotation's is passed over and said on the rule's tab as not
  known (`annotation_twice`); one that can't be read is said too
  (`annotation_unread`), without its text. So no annotation can make the
  operator's file unusable, nor change what it holds a rule to.
- **What step 1 refused.** `labels:`, a selector as `--rules-select` takes
  one, of the labels the rule's alerts carry (its group's among them): in
  serve, of the latest version, and of a rule only copied in, the labels
  all its alerts share (as `label:` on the rules list); in `lapilli verdict
  --compare`, of the rules each side replayed (`Comparison.LabelRules`). A
  selector that selects every rule is refused, and one is kept as it reads,
  so the same selector written with other spaces is one entry.
  `refires` in a budget, the firings that re-fired (`--refire`), by week.
  `scenarios:` stays refused, for step 4.
- **Which rules, which weeks.** The rules of the latest version and those
  copied in; with no version, every rule the ledger has. A budget's week
  before the rule came into effect has no check, shown empty. By the
  ledger's weeks (open question 4): over its budget, a week is broken, the
  week under way and a week the runs haven't finished too, as soon as it
  is over; under it, a whole week is met, the week under way (and one past
  the runs' cursor) is so far (`open`), and the week the ledger's first
  firing fell in, or the rule's first, is not known (`not_whole`). A rule
  only copied in is known where its source's copies cover it without a
  gap (the ledger's `Coverage` of it, the latest span): a week begun
  before the span began (in a gap, or before the first copy), a week past
  where it ends, whole, or the week so far none of reached, is not known
  (`not_copied`), however far the runs' cursor is, so that a job that
  stopped copying, or began late, doesn't read as weeks of no firings; and
  a catch of an incident outside the span, its delay with it, not caught
  in the copies, is not known too, rather than broken. Not in the design;
  a review found such weeks met, and a second the weeks before the span. While people slept is
  by the rule's team's own hours, or the server's (as the tiles count it);
  by hours with no sleep, not known (`no_hours`). A rule Lapilli can't
  replay and doesn't copy in is not known (`unreplayed`). A budget needs
  no record.
- **Catches.** Judged by the ledger's catches of the incident written
  down (counted: not a dismissed impact window); with none written down,
  not known (`no_incident`). A catch's row is of the week its incident
  began in, 0 while it isn't written down: a catch is of an incident, not
  of a week. **A new version of the rule**, one whose definition
  (`rules.Rule.Whole`) took effect after the incident began, is judged
  again by replaying the rule as it is over the incident (from the lead,
  its `for` and an hour before its start to its end, the catch's delay
  and an hour after, now at most), through the replay change reviews use,
  five a round within one `--timeout`, while the runs don't wait for the
  history; until then not known (`not_judged_again`), and kept with the
  version it was judged on, so each version is replayed once. An incident
  older than the thirteen weeks is not replayed (`older`): the history may
  not reach so far back, and a replay of nothing would read as a catch
  lost. A replay that fails is not known (`replay_failed`).
- **In the ledger (027).** A row a rule, an expectation and a week; the
  expectation is the hash of what holds the rule, what and the bound
  (`rule: Restarts`, `budget week`, `10`), so one budget held by two
  selectors is two expectations, each shown. Written only where a row
  changed, 20,000 rows a round at most, the rest the rounds after; read by
  the leader before it takes the writer, never firings under it. One no
  longer held is kept not current, so that a ConfigMap put back tells no
  week again; a budget's weeks out of the thirteen are let go, and a catch
  held no more, 13 weeks on. A rule's name is kept unique in Postgres by
  its md5, as `checked`'s.
- **Bounds.** 10,000 rules and expectations a round, each rule to each
  budget and catch that holds it. The operator's own (the file's and
  `--config`'s) are counted first: past 10,000 of them the check fails,
  saying what to do (a catch by `rule:`, not `group:` or `labels:`). A
  rule's annotation, its author's, is held after them, a rule's whole
  while it fits in what is left, in the rules' order; past it, one row a
  rule, not known (`annotation_past`). So no annotation, however many
  rules carry one, stops the operator's checks.
- **Told and made a task.** `expectation.broken` is told of a budget
  broken in the week so far or the last whole week, or a catch broken,
  once a rule, an expectation and a week, 50 a round, the rest the rounds
  after; a catch met again is told again once it is next broken, in a
  later week. It carries the rule, its team, what, held, the selector and
  the source, the count or the delay, whether it is the week so far, the
  week's span (a catch, its incident and its start), and links to the
  rule's tab. Make it a task (`POST /expectations/task`, a team's editor's
  of their own team's rule) works the row out again as the ledger last
  checked it, refuses one not broken, and goes to a task open about the
  expectation, of any week. `--expect-tasks` makes one of kind tune for
  the rule's team, 20 a round, once a rule, an expectation and a week,
  none while one made of it is open; unlike a goal's task, nothing is said
  on it afterwards.
- **Where it shows.** The rule's Expectations tab, each expectation with
  its thirteen weeks, or a catch with its incident; the overview's What to
  look at, after the goals' lines, five at most (the week so far first),
  the rest one line to the rules list; the rules list's facet
  Expectations (broken, held to some, held to none; `is:expected`,
  `is:unexpected`) and a badge of one broken; the digest of a week, its
  budgets' weeks and the catches of incidents begun in it (`expectations`
  in `digest.ready`), checked just before it in the round;
  `GET /api/v1/expectations` (a viewer's, `rule=`), MCP's `rule`
  (`expectations`) and `/metrics` (`lapilli_expectations_held`,
  `lapilli_expectations_broken`, `lapilli_rule_expectations_broken`, 500
  rules at most).
- **The backtest port** now counts while people slept by the rule's
  team's own hours too (`Comparison.ExpectBy`), as its design said it
  would once step 2 made it so; it still writes no checks, and doesn't
  read serve's own expectations.

Step 3, drafts, as built:

- **One way to draft, two places.** `verdict.Draft` makes a draft from
  what each rule did, and writes it with the YAML encoder; serve and
  `lapilli verdict` each count as they check budgets. serve counts the
  ledger's twelve whole weeks before this one (Monday, in `--tz`), a week
  whole for a rule as step 2 holds it (the ledger's first firing before
  it, the rule in effect from its start, the runs past its end), while
  people slept by the rule's team's hours, and the incidents written down
  in those weeks it caught. `lapilli verdict --draft-expectations FILE`
  counts the period it judged by whole weeks back from its end, as
  `--compare` holds budgets, a period of no whole week scaled to one; it
  needs no `--incidents` or `--impact` (budgets alone), and isn't taken
  with `--compare`, `--check-replay` or `--redact`.
- **Rounded up** (the design left it open): 1 to 9 as they are; 10 to 99
  to the next multiple of 5; from 100, to two significant figures (123 is
  130, 1,234 is 1,300); never below 1. A budget of 0 is broken by the
  rule's first firing however right it was, so **a rule that never fired
  is held to 1** a week, 1 while people slept, 1 re-fire: as it is,
  rounded up. While people slept is drafted only by hours that say when
  people sleep; with none, it is left out, and the line beside the rule
  says so. A catch is held within its delay rounded up to the whole
  minute, a minute at least (an alert before the incident began is held to
  come within a minute of it); a rule's latest ten.
- **Every rule at once** (the maintainer's decision of no default budgets
  rests on it): the rule's tab shows its draft, and a team's rules' or
  every rule's is a file to take (`GET /expectations/draft.yaml`, `team=`
  or `rule=`; `GET /api/v1/expectations/draft`, the same as JSON), bounded
  as an expectations file is (2,000 rules, 2,000 catches), what is left
  out said in its first lines: a rule nothing was counted of, one Lapilli
  doesn't replay nor copies in, those past the bounds. The file's comments
  are Lapilli's words and numbers alone, never a rule's name; a name of
  more than one line is quoted on one. The same weeks give the same file.
- **`test_expectation`** (behind `--mcp-compare`, as `compare` is, one at a
  time with it, the whole check within `--timeout` or 25 s, whichever is
  less: a scenario still running then is stopped, and not known). It
  replays
  the rule as it is, and the rewrite beside it when one is given, over the
  twelve weeks to now (or since the runs began), its firings kept, as the
  backtest port's (`replayCompared`), and holds them with
  `CheckExpectationsBy`: budgets by whole weeks back from the period's end,
  as the port and `--compare` count them, not the ledger's Mondays as
  serve's checks do; a catch as the replay of the rule as it is judges
  it, as step 2 judges a rule changed since (an incident outside the
  weeks is not known); a scenario run by `--promtool` through step 4's
  runner, against the rule's group as it is, and with the rewrite in its
  place. The expectations are the text given, its entries that hold the
  rule, or serve's own for it (the file's, `--config`'s and its
  annotation's, as step 2 holds it, their test files as serve reads them).
  Each item is met, broken or not known, with why; not known is never
  met.
- **A rewrite** is one alert rule, as `compare` takes one (a Prometheus
  rule, `for` and `keep_firing_for` 48h at most, an interval of an hour
  at most), keeping the rule's name and group; where several share both,
  the labels given pick it. A warning split from a critical is two rules,
  one of them new: adding a rule is step 5's. It is posted (`post`) only
  when the rule as it is and rewritten were each replayed to the end, it
  is held to one expectation at least, meets each, and loses no incident
  the rule catches as it is (`lost`). A side whose replay was cut short,
  or couldn't be made, catches nothing, so an incident the rewrite loses
  would go unseen: no post, and the answer's note says which side and
  why. The post says it is an agent's rewrite backtested by Lapilli,
  gives the period, each expectation as it is and rewritten, the firings
  and catches before and after, and the diff, in a fence a backtick
  longer than any run of them in it. Lapilli posts nothing (open question
  5).
- **`propose_expectation`** (behind `--mcp-propose`, as `propose` is) takes
  an expectations text holding the rule by `rule:` alone, checks it as
  `test_expectation` does (no rewrite: a rule's change kept for a person
  is `propose`), and keeps it, as Lapilli writes it, with its test files
  and what checking it found, for the person the agent acts for, refused
  as `propose` is when who that is isn't known. Before anything is
  replayed, the hour's limit is asked (`ProposalsAnHour`, 20, which
  proposals and drafts now share, a person's with their agents') and a try
  is taken (`triesAnHour`).
- **Drafts the history makes** are kept from the rule's tab ("Keep it as a
  draft", a team's editor of their own team's rule), made by the person
  who kept them: Lapilli keeps no draft of itself. The design said an
  agent can't accept its own draft; as built, **no one accepts a draft
  they proposed, or their agent did**, as no one approves their own
  proposal, refused in the ledger's write by the folded name.
- **In the ledger** (027, in place). A draft is bounded: its expectations
  64 KiB, its test files 20 and 256 KiB in all, its reason 2,000 bytes,
  what it found 64 KiB, the file it makes 1 MiB; ten wait for a rule at
  most, and 1,000 are kept, the oldest decided let go to keep another,
  none kept while as many wait.
- **What an agent sends**, beyond what `compare` bounds: the rule's name 1
  KiB, the expectations 256 KiB, the rewrite 64 KiB, the test files as a
  draft keeps them, each by a path an expectations file may name, those
  given named by the scenarios and the scenarios' given, each read as
  promtool would expand it before it runs (`scenario.Bound`: 100 tests,
  1,000 input series and 100 a test, values expanding to 100,000 in all,
  and 10,000 evaluations a test), then rewritten as step 4's are. All
  promtool parses as values notation is counted, in one pass, never less
  than promtool expands: an input series' values, a blank among them, as
  promtool's parser counts `0+1x100000000`, `_x5` and each repetition in
  an item; a native histogram as the numbers it holds (its buckets and
  other fields) each time it is repeated, as promtool copies
  `{{…}}+{{…}}xN` at each step, an item of more than two refused; and an
  expected sample's `histogram`, which promtool parses the same way.
  promtool writes each input series as a line of its load command,
  `  <series> <values>`: so a `series` is a metric name, its labels in
  braces, or both, with no space, line break or anything else outside the
  labels' quoted values (`big 0+0x100000000` would carry values, a line
  break another series), and `values` holds no line break before its end,
  nor anything but values notation (a quote or a lone brace would have
  promtool read labels there). Each is plain text: a tag such as
  `!!binary`, which promtool's YAML would decode into another text, is
  refused. An agent's scenarios run one at a time across serve, whatever
  else holds the comparison's turn.
- **Accepting** is an approver's, in a browser, as approving a proposal is
  (same site, no bearer token, `--changes` on, the page's form). Where
  serve's expectations come from `--expect`, with the change recipes'
  token, and the draft holds no scenario (whose test file is another
  file), it goes **the git path**: the file is read again, the draft taken
  into it (`verdict.TakeIn`: the file's tree, each budget field, catch and
  scenario the draft gives in place of the file's by the same `rule:`, its
  comments kept, written by the encoder and read back; a file with anchors
  is for a person), its hash kept, and it is handed to the change recipes
  beside the proposals (`GET /api/v1/changes`'s `expectations`, `PATCH
  /api/v1/changes/expectations/{id}`). The recipes write `EXPECT_FILE`
  alone, a tracked, non-executable `*.yml` or `*.yaml` not of a CI system,
  only while its sha256 is that hash, and only as an expectations file,
  one document whose only key is `expectations`, before and after: a
  Lapilli someone took over can't make them write anything else.
  Otherwise accepting keeps the decision, and the tab says it isn't
  applied and gives the YAML: the expectations file with the draft taken
  in, or, for `--config` or none, its entries, to put there by hand.
- **Told**: `expectation.drafted`, `expectation.accepted`,
  `expectation.dropped` and `expectation.pull_request`, never a draft's
  text, test files or reason. **Shown**: a Drafts part of the rule's
  Expectations tab, each draft with what it found, its reason as the
  agent's words, escaped, and its text; the open ones on the proposals
  page; `GET /api/v1/expectation-drafts` and `/{id}`.

Step 4, scenarios, as built:

- **The file.** `scenarios:`, paths of promtool test files relative to the
  expectations file, inside its directory (`ScenarioPath`: no `.`, `..` or
  empty part, no leading `/`, no `\`, nothing that doesn't show, 512
  bytes), 200 in a file, the same path twice for one rule, group or
  selector refused. A test file is read only where promtool is given, 1
  MiB at most, inside the expectations file's directory through `os.Root`:
  a symbolic link out of it isn't followed (in a ConfigMap's directory the
  kubelet's own links stay inside, and are). Nothing in the file, a rule or
  a test file names a binary or an argument: `--promtool` is the
  operator's, resolved at start (on `PATH`, made absolute, a file that can
  be executed).
- **What promtool reads** (`internal/scenario`). The test file as promtool
  reads it, its top-level keys promtool's alone (one it doesn't have is
  refused here, rather than left to promtool, which would say `FAILED`),
  with one test at least; its `rule_files` replaced by `rules.yaml`, the
  group Lapilli writes, whatever it named, globs and paths outside
  included; its `group_eval_order` left out, there being one group. The
  group is the rule's as Lapilli has it on that side, by its file, its
  ruler's namespace and its name: its settings (`interval`,
  `query_offset`, `limit`, `labels`) as its rules share them, its
  recording rules first, then its alert rules under the names their
  alerts carry, each with what it says as Lapilli keeps it. That differs
  from the file where Lapilli reads it otherwise: a character that doesn't
  show is U+FFFD, an annotation is cut at 4 KiB and 32 besides summary and
  description, and a recording rule's place among the alert rules is
  lost; a test's `exp_annotations` can tell. A test that needs rules of
  another group fails: the design said "as promtool runs them", and
  promtool is given this group alone, so that no test file chooses what it
  reads.
- **How it is run.** In a directory of its own, removed after, holding
  `test.yaml` and `rules.yaml`: promtool started directly, never through a
  shell, as `promtool test rules test.yaml`, with nothing in its
  environment but `TMPDIR`, a directory there (its test storage writes
  under it), in a process group of its own; 30 s at most, the group killed
  whole past it, so that a `--promtool` that is a wrapper leaves no child
  running; 64 KiB of what it says kept, the rest neither kept nor read.
  **Its memory, capped** (not in the design: a review found a rule
  author's annotation whose template promtool expands at each evaluation
  of an alert that fires, such as `absent(up)`'s, which fires in
  promtool's empty storage and never in production, took promtool 3.5.0 to
  2.6 GB in 1.6 s, from rule text alone). On Linux, within
  `--promtool-memory` (256Mi unless given, 64Mi at least; the chart's
  `serve.promtool.memory`): Lapilli starts itself again under a name of
  its own (`lapilli-promtool-capped`, never a command a person gives,
  which reads promtool's path and the cap from its environment and refuses
  any other argument or variable), which sets RLIMIT_DATA, hard and soft,
  to the cap and executes promtool in its place with the same fixed
  arguments and `TMPDIR` alone. Past it promtool's allocations fail, Go's
  runtime says so, and the scenario is not known (`promtool took more
  memory than it may`). RLIMIT_DATA rather than RLIMIT_AS: since Linux 4.7
  it holds every private writable mapping, Go's heap among them, while
  RLIMIT_AS would hold the address space Go's runtime reserves as it
  starts, far more than it uses, and stop promtool before it read a test.
  macOS refuses RLIMIT_DATA: there promtool runs without a cap, said once
  as serve starts. `lapilli verdict --promtool` runs it without one, in a
  CI job whose own limits hold it. Exit 0 saying `SUCCESS` is met; exit 1
  saying `FAILED:` is broken, the lines after it kept (2,000 bytes);
  anything else couldn't be run, and is not known: a time out, a crash,
  another exit status, an answer promtool doesn't give. promtool also says
  `FAILED` of a test it can't read past the top level, or rules it can't
  load: those are broken, with promtool's words. On both sides, as such a
  file is, it is "broken before too", and fails no pull request. The tests
  run a stand-in for promtool (the test binary, through a link named
  promtool, which refuses any other command line, environment or
  `rule_files`), and promtool itself where one is on `PATH`
  (`TestRealPromtool`, skipped in CI); the group and the rewritten test
  file were checked with promtool 3.5.0. The cap is tested on Linux
  alone: the stand-in says the RLIMIT_DATA it was started under, and one
  that takes a GiB is stopped (`TestRunMemoryCap`, `TestScenarioRunsCapped`);
  the wrapper refuses another command line (`TestCapWrapperRefuses`).
- **Which rules.** Those read from Prometheus's own format
  (`rules.Rule.Prometheus`): a rule file, a PrometheusRule, a ConfigMap of
  one, an Amazon namespace, a ruler, Google Cloud's `ClusterRules` and
  `GlobalRules`, Prometheus's rules API. Not an Azure rule group (its own
  format), nor Google Cloud's namespaced `Rules`, whose expression Lapilli
  scopes to its namespace and so isn't the file's; a Grafana rule, a Loki
  ruler's, a cloud's alarm: not known, open question 1 still open.
- **In `lapilli verdict --compare --promtool PATH`.** Each scenario of a
  rule the change touches, on each side the rule is on, read in the
  directory of the file that names it (`--expect-before`'s, for what it
  names); one new in the change is run before too, one taken away on both
  sides from the file before, and a rule taken away fails its scenarios,
  as it catches nothing. Each test file is run against each group once,
  200 runs a comparison at most, the rest not known. Counted as budgets
  and catches are, so `--fail-expectations` and the recipes'
  `FAIL_ON_EXPECTATIONS` cover them; a test file the change edited is said
  (`edited`). Not in the design: the summary's `scenarios_unrun`, those
  that couldn't be run after the change, promtool given and the rule one
  it runs (the test file unread, a time out, a crash, past the 200), and
  one run before the change and not known after it (the change moved the
  rule out of Prometheus's format, or its definition out of reach), which
  the recipes' gate fails on, as on a rule not replayed: a gate that can't
  judge doesn't pass. Without `--promtool`, each is not known (`not run:
  no --promtool`) and counts nothing. The text and Markdown say what
  promtool said after the table, each line a code span, eight at most; the
  JSON has `before_why`, `after_why`, `said`, `edited`, `unrun`;
  `--redact` aliases a scenario's path, and hides what promtool said.
- **The recipes.** `PROMTOOL` (the action's `promtool`), with `EXPECT`:
  passed as `--promtool`, found on the runner, refused in the checkout.
  The directory `EXPECT` is in is read from each commit as `PATHS` is
  (`git archive`, a symbolic link refused, `MAX_TREE_FILES`,
  `MAX_TREE_BYTES`), so each commit's test files are read beside its
  expectations file. In serve mode, chosen: the job runs no scenario, and
  `PROMTOOL` is refused with `LAPILLI_SERVE`; serve reports each as not
  run through lapilli serve (`NotThroughServe`), and its summary's
  `scenarios_unrun` is 0, so the gate doesn't cover them there. Serve mode
  installs and runs nothing in the job by design; running them there
  would mean installing Lapilli and promtool in the job and joining two
  reports and two summary lines into one comment and one gate, for what a
  `promtool test rules` step of its own already gates.
- **Through the backtest port** nothing of a scenario is read or run: a
  request carries the expectations' text, never a test file, and
  `Comparison.Scenarios` is `NotThroughServe` whatever `--promtool` serve
  has.
- **In serve** (`--promtool PATH`, the chart's `serve.promtool`). The
  scenarios of `--expect`'s and `--config`'s expectations, each read in
  the directory of its own file, every round (a test file edited is
  checked again, though nothing else changed), against the rule's group as
  the latest version has it, its recording rules read from the version's
  documents. A scenario's row is of the week what it found was found in,
  one a rule and an expectation, as a catch's is of its incident's; its
  version is a hash of promtool's path, the group and the test file, and
  it is run again only when that changes: the design's "when the rule's
  version changes", and also when its test file, its group's other rules
  or promtool do. One that couldn't be run is kept so too, not run again
  each round. Five a round (`expectScenariosARun`), never under the
  ledger's writer, each kept as begun before promtool starts (its row,
  alone: not known, `run_failed`, what it is run on) and as found once it
  ends; a run that never ends, serve killed as promtool took the
  container's memory, is so for the next leader, which runs it again only
  once promtool, the group or the test file changes, rather than each term
  (of a scenario's two rows, the run begun and the one it was to replace,
  the later week's is read). A run serve stops itself, its round's context
  ended (shut down, or the lease lost), isn't one that died: it is kept
  with no version, written past the ended context, and run again by the
  next leader. The rest not known (`not_run_yet`) until a
  later round. Not known says why: `no_promtool`, `not_prometheus`,
  `not_run_yet`, `test_unread`, `run_failed`. Schema 027, unreleased, was
  changed in place for it: a column `detail` (what promtool said, or why
  it couldn't run, 2,000 bytes). A broken one is told
  (`expectation.broken`, with `scenario`), made a task, and shown on the
  rule's tab (with what promtool said), the overview, the rules list, the
  digest (in the week it was found), `GET /api/v1/expectations`, MCP's
  `rule` and `/metrics`, as a catch is.
- **The chart.** `serve.scenarios` puts each test file beside
  `expectations.yaml` in its ConfigMap, a scenario's path its name.
  Lapilli's image stays without promtool: `serve.promtool.image`, an image
  holding promtool and `cp` (Prometheus's own, pinned by digest), from
  which an init container copies `serve.promtool.from` into an emptyDir
  only the pod has; or `serve.promtool.path` in an image of one's own.
  With either, `/tmp` is an emptyDir, the root being read-only. promtool
  runs in serve's container: its memory counts toward serve's limit, each
  run held to `serve.promtool.memory` (256Mi) on Linux.

Step 5, adding a rule, as built:

- **One form for every path.** The rule is one Prometheus alerting rule,
  as a rule file's rules list holds one: `alert`, `expr`, `for`,
  `keep_firing_for`, `labels`, `annotations`, nothing else (no recording
  rule, no group, no file). Not each path's own form (a Grafana rule's
  queries and expressions, a CloudWatch alarm's or a Google policy's
  document): one text to read strictly, the same rule replayed wherever it
  goes, and every field each path writes written by Lapilli. Read strictly
  (`rules.ParseAdded`): one YAML document, a mapping of those keys, each
  once, each value text as written (a number or a word YAML reads
  otherwise, such as `team: 7`, is to be quoted), no anchor, alias, tag or
  merge; bounded: 64 KiB in all, the expression 16 KiB, a `for` and a
  `keep_firing_for` 48 hours at most, 20 labels and 20 annotations, a
  label's value 1 KiB, an annotation's 4 KiB, nothing in them that doesn't
  show but line breaks and tabs (and those only in the expression and the
  annotations); its name letters, digits, `_`, `:`, `-` and `.`, 200 at
  most, a letter or `_` first; a label or an annotation named as
  Prometheus names one, not its own (`__`), and no label `alertname`. Its
  labels and annotations are held as a change to what a rule says is
  (changes.md, `says`): each a template that parses in the place's
  template language, calling no `query`, nothing that grows without bound
  each time the alert fires; a summary or a description 4 KB at most, a
  runbook an https URL. Its labels are plain text: one holding `{{` is
  refused, Prometheus and Grafana expanding a label's template at each
  evaluation (`{{ $value }}` makes each one another alert), which the
  backtest, keeping a label as written, wouldn't count; its annotations
  may be templates. Prometheus refuses a whole rule file for one that
  doesn't parse, and every rule in it with it. It is written by Lapilli's
  encoder, never as given: its keys in one order, its texts double-quoted,
  an expression of more lines a literal block.
- **Named apart from every rule in effect**, not only from its group's:
  a proposal, its expectations and the ledger name a rule by its name, and
  two rules of one name are told apart by labels that can change. A rule
  in effect of its name since refuses it as it is carried out.
- **Where it goes: a place serve offers, and nothing else.** Worked out
  from the rules it read last (`addPlaces`, the new rule's form, MCP's
  `places`), never from what a proposal says, which is only matched to
  them; anything else is refused with one reason, whether it exists or
  not. The git path: a group of a Prometheus rule file read under
  `--rules` (a rule file, a PrometheusRule, Google Cloud's `ClusterRules`
  or `GlobalRules`, a rule file a ConfigMap holds in a literal block; in a
  file of two PrometheusRules, either's groups, the other document as it
  was), or a new group after the last, offered only in a file where one
  document holds groups (`rules.NewGroupFits`); not Google Cloud's
  namespaced `Rules`, whose expressions Lapilli scopes to their namespace,
  nor a Grafana provisioning file, nor a file a CI system runs, by
  Lapilli's own copy of the change recipes' list, a first lock beside
  theirs, nor a new file, which isn't a document Lapilli read, and so not
  one a recipe finds by its hash. Grafana: a group of a folder whose rules
  Lapilli replays as PromQL, its query one of the data sources they read;
  not a new group, which Grafana would make with an interval of its own. A
  ruler: a group, or a new group, in a Mimir's, a Cortex's or an Amazon
  namespace `--changes-in` names; not an Azure rule group, whose rules
  carry action groups of their own. A cloud's alarms: CloudWatch's or
  Google Cloud's `--alarms` with alarms read, under a name `--changes-in`
  names there; not Azure Monitor's alert rules (a PromQL metric alert
  takes a workspace's scope and a location Lapilli would have to guess)
  nor OCI's (MQL, which Lapilli doesn't replay, so one added couldn't be
  held to anything). Those left out aren't offered.
- **Whom it tells: no power over the account the rules there don't already
  use.** A rule added to a rule file or a ruler names no one: its labels
  route it through Alertmanager, as every rule's there do, to whichever
  receiver its routes match; its page says so. In Grafana, one contact
  point at most (`notification_settings`), of those the rules of its group
  name: where every rule there names one, it names one of theirs (named
  none, Grafana's notification policies would route it by its labels to
  whichever contact point they match); where none of them names one, it
  names none, routed by the policies as they are, its page saying so;
  where some do, either. In CloudWatch, actions (`AlarmActions` and
  `OKActions`) of those the alarms of its `--alarms` take that only tell:
  an SNS topic, a Systems Manager OpsItem, an Incident Manager response
  plan, each by its ARN's shape; never a Lambda function or a Systems
  Manager automation, which run code, nor an action that does more (Auto
  Scaling, EC2: stopping or terminating a machine), even where an alarm
  there takes one (a change to an alarm already there, alarms.md, still
  counts a Lambda function as telling, as released). In Google Cloud,
  notification channels of those its policies tell. Anything else is
  refused, as a place is; it is checked again, against the rules read
  then, as it is carried out.
- **Held to its expectations before approval.** An expectations text
  holding it by `rule: NAME` alone, one expectation at least (a budget's
  field, a catch or a scenario: an added rule held to nothing is refused,
  and the ledger keeps none), with the test files its scenarios name,
  bounded as an agent's are. The rule is written where it goes and read
  back as Lapilli reads rules there (for Grafana, a group of its folder
  with it alone, in the export's form; for a cloud, the alarm's document as
  `--alarms` reads one), then replayed over the twelve weeks to now with
  nothing beside it, and held by step 3's own check (`replayHeld`, what
  `test_expectation` was split into, `verdict.CheckExpectationsBy`): a
  budget by whole weeks back from the period's end, scaled when there is
  none; a catch by the incidents written down; a scenario run by
  `--promtool` against its group with it added (a rule file's and a
  ruler's; Grafana's and a cloud's aren't promtool's, and are not known).
  Its backtest is kept as other proposals' are, nothing before it.
- **Approval** (the recommendation, taken): no one approves one that
  breaks an expectation it is held to, or whose backtest didn't replay it
  to the end, or that is held to nothing. Not known is shown, and is never
  met, but doesn't hold it: a catch of an incident older than the twelve
  weeks can't be known. Its page says why it waits and offers no form;
  the server refuses a form shown before (400), and `--approvals` lowered
  doesn't approve it. An expression the history refuses isn't replayed,
  and so isn't approved: what replays is what the place would load. No
  one approves their own, or one their agent made; an agent's counts
  toward its person's twenty an hour, as proposals do.
- **Stale** as changes.md says of any backtest (an earlier lapilli's, a
  record changed since): held until it is backtested again, from its page,
  by MCP's `backtest_again` or by Lapilli itself, which writes it where its
  place says from the rules read again and holds it to its expectations
  anew.
- **Carried out** after it is checked again: its place still offered, its
  name still free, held to its expectations again over the twelve weeks to
  now, refused when it now breaks one or isn't replayed to the end. It
  waits for a comparison running (an agent's `compare` or
  `test_expectation`), as a change's backtest does, rather than failing.
  The git path: the rule file's other lines changed since, the rule is
  added afresh in it; the file as it was is kept for undo, and the change
  recipes write it as any change, their own guards on which file and what
  of it. Grafana: a `POST` of the rule with a uid of Lapilli's (`lapilli-`
  and twenty hex digits), in the folder and group a rule there is in now
  (their uid and name read from it), refused when another tool keeps that
  rule (provenance), when its folder's rules no longer read its data
  source or tell its contact point, or when a rule of its title is in its
  folder; read back by its uid. A ruler: the group written as rulers.md
  writes one, only the rule added to it as it is now (`rules.AddsRule`), a
  new group only while it is still gone. CloudWatch: `PutMetricAlarm`, as
  undoing a deletion makes one (its PromQL in
  `EvaluationCriteria.PromQLCriteria`: `Query`, `PendingPeriod`,
  `RecoveryPeriod`; `EvaluationInterval` 60), in the account the alarms
  there are read in, which the write role's must be; refused when an alarm
  of its name is there. Google Cloud: `alertPolicies.create`, a policy of
  one condition, its display name, the condition's and `alertRule` the
  rule's name, refused when a policy of that display name is there, read
  back by the name the create answers with, or, that answer lost, by its
  display name. An alarm is read back by what it holds (its rule as it
  fires), not whole: the cloud adds fields of its own to one it makes.
- **Undone, it is deleted**, only while it is as the add left it: the git
  path, the file put back as it was (refused if it changed since, as any
  undo there); Grafana, a `DELETE` by its uid, kept by no other tool; a
  ruler, the rule taken out as a deletion takes one (a group made for it
  goes with it; an Amazon namespace's last rule is a person's); a cloud,
  `DeleteAlarms`, or the policy's `DELETE`, by the name the create gave.
- **Its expectations, once it is added**, go where an accepted draft goes
  (step 3): kept as a draft of origin `proposal` (`expectation_drafts`'
  `proposal`), made by whoever proposed the rule and accepted by its first
  approver, by the git path to `--expect`'s file when it can go there,
  otherwise the YAML on its page and the rule's tab, for a person. Kept
  once the rule is applied, not before: a rule not added holds nothing.
  Kept only of one someone approved: one applied with no one's approval
  (its proposed diff merged by hand) has no approver to accept them, and
  is marked never to be kept as it is applied; one the ledger refuses as
  it is kept is marked so too, and one that fails otherwise is tried the
  next round, so that none holds up another, nor the changes carried out
  after. The proposal counted toward the hour, so the draft doesn't.
- **In the ledger** (027, unreleased, in place): `proposals.expect`, what
  a rule added is held to (the text as Lapilli wrote it, and its test
  files, bounded as a draft's), read with its proposal alone, never with a
  list; `proposals.expect_kept`, what became of it once the rule was added
  (0 waiting, the draft it was kept as, so that it is kept once however
  many drafts are let go, -1 never), those waiting read along
  `proposals_unkept`, never by what the JSON holds;
  `expectation_drafts.proposal`.
- **Grafana's `is_number`** is now replayed (`internal/sse`), as Grafana
  evaluates it: the condition Grafana's own conversion of a Prometheus rule
  writes, `is_number($A) || is_nan($A) || is_inf($A)`, firing on every
  series the query returns, as a Prometheus rule does; no data is OK.
- **Preflight** (step 2): a CloudWatch prefix of `--changes-in` is also
  asked of one name under it no alarm read has, the alarm a rule added
  there makes (`addedUnder`); Grafana's `:create` and Google's `create`
  were asked already, for undoing a deletion.
- **Tested** against the stand-ins the paths' own tests use: each path
  made, read back and undone (a file's group, a Mimir group and a new one,
  a Grafana folder's group, a CloudWatch alarm, a Google policy); each
  guard (the place offered, a name in effect, the strict read and
  Lapilli's own encoding, whom it tells, the templates, the roles and
  self-approval, the hour, one expectation at least, a broken or
  unreplayed one holding approval, the check again as it is carried out)
  taken out in turn, a test failing for each; none against a real account.
  After review: an add carried out while a comparison runs, the adds whose
  expectations can't be kept ahead of one that can, a file of two
  PrometheusRules, a Grafana group whose every rule names a contact point,
  a Lambda function and an automation among CloudWatch's actions, a
  label's template; each fails with its guard taken out.
