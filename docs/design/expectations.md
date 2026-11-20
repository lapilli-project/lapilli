# Expectations: what a rule was tested to do, kept and held to

Status: accepted; its first two steps built (see "As built" at the end). Under [GOVERNANCE.md](../../GOVERNANCE.md)
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

## Adding a rule, later

Proposals change rules that exist: a `for`, a threshold, turning one off,
deleting it, what it says, undoing a change. A rule added, held to its
expectations before it is approved, is the step after this one. It needs a
place to go (a file, a group, a folder, a cloud's alarms), which the person
proposing it gives, and then goes the four paths a change goes. This design
names it so expectations are made with it in mind, and leaves it out.

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
- `expectation_drafts` (step 3, its own schema, 028 as things stand): an
  agent's or the history's draft, who proposed it, what checking it found,
  and whether a person accepted or dropped it.

As with every schema change, a release before 027 doesn't open a ledger at
027: upgrading adds a table, with nothing to backfill, and going back
takes the ledger as it was before, so a deployment keeps a copy of it (a
snapshot of its disk, or of its database) before it upgrades.

## Three cases

1. A pull request adds a restart rule. Over the fourteen days the backtest
   reads, it fires forty a week, seventeen while people slept; its
   expectation, written with it, says ten and two. The job fails, and the
   comment says by how much. The author raises `for` from 1m to 10m, which the comment's
   agent rewrite also suggests: eight and one, and the incident it was
   written for is still caught. It passes.
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

## Steps

1. Budgets and catches in `--compare` and the backtest recipes, with
   `--fail-expectations`. No schema change; the PR check is the first use.
2. serve's checks after deploy: the rule's tab, the overview line, tasks,
   notices, the digest. Schema 027.
3. Drafts: from the history, and MCP's `test_expectation` and
   `propose_expectation`; agents' rewrites on a failed pull request.
4. Scenarios: promtool test files, for Prometheus-format rules
   (`--promtool PATH`).
5. Adding a rule, as a proposal held to its expectations.

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
  ledger's weeks (open question 4): over its budget, a week is broken,
  the week under way and a week the runs haven't finished too, as soon as
  it is over; under it, a whole week is met, the week under way (and one
  past the runs' cursor) is so far (`open`), and the week the ledger's
  first firing fell in, or the rule's first, is not known (`not_whole`).
  While people slept is by the rule's team's own hours, or the server's
  (as the tiles count it); by hours with no sleep, not known
  (`no_hours`). A rule Lapilli can't replay and doesn't copy in is not
  known (`unreplayed`). A budget needs no record.
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
  budget and catch that holds it, past which the check fails, saying what
  to do (a catch by `rule:`, not `group:` or `labels:`).
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

