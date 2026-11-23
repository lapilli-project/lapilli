# Keep judging

`lapilli serve` keeps a ledger. Every hour it replays the rules over the
hours since its last run, and the hour before that again for points that came
late, and records the firings and their stones. It reads the rules every
minute and records each change as a new version, so every hour is replayed
with the rules as they stood then, and it replays a change over the week
before it to show what the change would have done. Once a week it judges the
week before, works out longer `for`s for the noisy rules and the rules that
nearly caught what nothing caught, and posts the report to Slack. An incident
written down after that, or in the week under way, has the rules that nearly
caught it worked out within the hour.

```sh
lapilli serve --rules alert-rules/ --prometheus http://prometheus:9090 --match service \
  --impact 'sum by (service) (rate(http_requests_total{code=~"5.."}[5m])) / sum by (service) (rate(http_requests_total[5m])) > 0.01'
# then http://localhost:8080
```

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="../assets/serve-dashboard-dark.png">
  <img alt="The lapilli serve overview, with its pages in a sidebar: last week's precision, firings, incidents missed, time to catch (none), noisy rules, notifications for firings that caught nothing while people slept, re-fires and firings during deploys, each with a line of its weeks, most beside the week before, and a link to the list that makes it; under them, what to look at: rules to quiet or look at, each with a button to make it a task" src="../assets/serve-dashboard.png" width="820">
</picture>

A web UI opens on a dashboard of the last twelve weeks: last week's
precision, firings, incidents missed, time to catch, noisy rules and the
firings that woke people for nothing beside the week before; firings, precision and incidents by week; where the noise
is, rule by week; and what to look at first (a longer `for` that would quiet
a rule, an incident nothing caught, impact windows to decide on). Beyond it,
each rule over twelve weeks with its versions, the incidents and what caught
them, and each day's firings. People
write incidents down there, confirm or dismiss the windows `--impact` found,
or say a black stone was real; the stones are given again at once, for the
whole ledger, without asking Prometheus. The settings that only judge the
ledger again (`--lead`, `--match`, `--refire`, the noisy thresholds) can be
changed there and tried on the last week first (`--linked-only`, which
has an incident that lists the alerts its tool linked to it caught by
those alone, is a flag the Settings page says is on or off: given or
taken away at a start, every firing is judged again with it); so can the impact signal,
`--impact` and `--impact-for`, by an admin, under Incident criteria, tried
on the last four weeks first: the windows it would find, and how the
verdict would change. Saved, it is read again over the last four weeks,
which are judged again, and keeps the record from then on, not of the
weeks it wasn't read over, nor of those before every metric it reads had
data. A named signal may give a lead of its own there, as
`--impact-lead NAME=DUR` and `lead:` in `--impact-file` do, and a rule
entry one for its rules, with `require:`, labels its rules' firings and
its signals' windows must both carry: a cause that fires an hour before
the errors is caught by them without a longer `--lead` for every rule,
and incidents written down keep `--lead` (docs/design/judging.md, item
1). A named signal taken away there, or left out by a start, judges no
one: its windows keep no rule's record, and only the rules no rule entry
names catch them; confirming one that was a real outage keeps the record
of its weeks (item 5). Beside it, Incident sources says where
incidents come from (written in Lapilli, each tool a job copies them in
from, the incidents file, the impact signal) and whether each is alive.
`/settings.yaml` gives the changes as Helm values to commit. Teams, quiet windows, reports and roles
can be declared in Git instead: `--config FILE` (`serve.config` in the
chart) is YAML of them, as the UI's forms make them, which the ledger is
kept holding as the file changes, without a restart; what it declares is
marked declared in the UI, which doesn't change it, and what it no longer
declares is the UI's again, or deleted with `--config-prune`, but a role,
taken away. A file is kept to only once it has been read the same twice
in a row, so one read half written changes nothing. An admin's
`/config.yaml` gives everything the ledger holds in that form, to commit
(docs/design/serveops.md). The Cleanup page (`/orphans`) lists the notes,
tasks,
comments and proposals of rules deleted or renamed, to move to the rule as
it is named now, or delete. The API gives `lapilli verdict --json`'s
report for any period (`/api/v1/report?from=&to=`), its firings as CSV
(`&format=csv`, those quiet windows excused and each rule's team among
them), and takes incidents
(`/api/v1/incidents`); `/metrics` gives `--metrics` over the last seven days,
so Prometheus scrapes it instead of a Pushgateway, and the chart has a
Grafana dashboard of them.

The pages are in a sidebar of four groups, named as a console names them
(Monitoring: Overview, Rules, Incidents, Alert history; Improve: Tasks,
Proposals, Quiet windows; Reporting: Reports, Outcomes;
Administration: Teams, Roles, Settings, Sources, Run history, Cleanup), a
menu on a narrow screen, each with a breadcrumb and a bar to pick the team
and the period, which every link carries; a reader who edits one team sees
it first. The bar's search finds rules, incidents and teams, and opens the
one it finds. The rules list gives each rule a badge on three axes (Noisy,
No record, OK, Silent, Spare; Differs, Not replayed, Recorded only; keep, tune,
retire), filters by each, counted, and a search box that takes words of a
name and `is:noisy`, `noisy:episodes`, `is:unrecorded`, `severity:critical`, `status:tune`, `has:proposal`,
`label:team=pay`, `group:shop` and `team:pay`; each filter in effect is a
chip that takes it away, and the URL keeps them all; each rule's own labels
are chips too, each picking the rules with it
([docs/design/ui.md](design/ui.md)).

The Sources page (`/sources`) lists every source `serve` reads, as Argo
CD lists its applications: the rules where they are kept and what is read
beside them (a cloud's alarms, Loki's rules), Prometheus as the runs ask
it, the incidents, deploys and marked files, the impact signals, the
on-call schedules, and what jobs copy in; each with when it was last read
well, its last error (without the paths of `serve`'s files but to an
admin, and without a credential to anyone), the rules it gave, how far
behind it is, and a state: working, failing, quiet or waiting. A source
whose reads fail three times in a row, or that fails or goes quiet past
its bound, is down: the webhook and Slack are told once (`source.down`),
and once it has read well again for 15 minutes (`source.back`); one that
flaps is told of once, and again an hour after it was told back at the
soonest. `/api/v1/sources?all=true` gives the same.

The activity log (`/activity`, from Run history) lists every change made
through the UI, the API, `--config` and by Lapilli itself, 100 a page,
filtered by who, kind, team and period, and `/api/v1/activity` exports it
as JSON or CSV with the same filters (a cell a spreadsheet would take for
a formula starts with `'`). The roles given and taken are an admin's to
read, as on Run history. It is kept for `--keep-activity` (800 days
unless given, or `--keep` when longer; never less than `--keep`; 0 keeps
it all, as `--keep 0` does), past the firings it is about.

Beside its firings, each rule's are counted as **episodes**, one problem
however many alerts it is: its firings with the same values of the match
labels, overlapping or re-firing within `--refire`, parted at midnight in
`--tz` ([The numbers](numbers.md#episodes)). The rules list has an
Episodes column, and `noisy:episodes` picks the rules noisy by their
episodes, by the same thresholds as by firings; a rule's page says its
episodes, their precision, and when it is noisy by one and not the other
("Noisy by firings, not by episodes: 41 firings in 17 episodes"), and of
each of its latest firings the episode it is of; the overview's noisiest
rules their episodes; the digest each noisy rule's; `/api/v1/summary`,
MCP, the report, the CSV and `/metrics` theirs. Noisy by episodes decides
nothing: the badge, the suggestions and what a catch counts for stay by
firings. A ledger upgraded to this release, or opened in another `--tz`,
counts its episodes again a week at a time, the newest first, in the
leader's round: until it has, those weeks say their episodes aren't
counted yet. Goals take `episodes.total` and `episodes.precision`
([docs/design/goals.md](design/goals.md)).

A person may say what Lapilli can't see (docs/design/judging.md, item 3).
On Alert history, a rule's latest firings and, through a link from each
rule that caught it, an incident's page: that a white firing is
**unrelated** to what it caught, a log rotation during an outage, which
turns that firing alone black, the incident caught by the other firings
alone or missed; and whether a firing **needed action**, yes or no, which
changes no stone and is counted. "Unrelated" is an editor's of every team
and an admin's, as it takes a catch from what may be another team's
incident; "needed action" a team's editor's too, of their team's rules,
whose write leaves "unrelated" as it is. Each part keeps who said it,
when, and a note. One who isn't an admin speaks of the last 14 days'
firings alone, and presses "This was real" on them alone; each person
makes 30 verdict writes an hour, a bulk answer of 100 firings counted as
one. A verdict is kept by its firing's alert and start, so a run that
starts the firing a step later, or a window read again under another key,
takes it along, marked to look at again: the overview counts those, and
the Cleanup page lists them. The summary gives each row's `people`, its
precision as Lapilli judged it among them, and `action.share` once 80%
of its firings are answered; goals take `action.share`, a week under 80%
answered missed, and `stones: lapilli` on a precision goal to hold it to
Lapilli's own stones; `/metrics` gives `lapilli_rule_action`; the digest
a line; `/api/v1/verdicts` reads and writes them, MCP reads them. Every
replay, a proposal's backtest, `compare`, a pull request's backtest, the
Settings page's tries and the digest's suggestions, carries them on both
sides, so a change can't look better on the firings people called
coincidences.

A rule entry may say `judged: false` in place of signals, in
`--impact-file` or on the Settings page: a watchdog, a capacity forecast,
a reminder. Its rules' firings are counted, never white nor black, in no
precision, never noisy, and catch nothing; the summary says them
`not_judged`, the rules list and the rule's page "Judged by nothing", and the
Goals page beside each goal how many firings an entry kept from being
judged. A file whose entries all say so needs no signal.

With no incident written down over the weeks shown, or the two before
their end, the pages call no rule noisy and show no precision, and say why
once. For a week like that, the weekly digest says, of each rule replayed
that fired as often as a noisy rule, what its firings were: how many went
well past its bound, the alerts that fired most and whose labels they have;
a rule's page shows it in place of a suggestion, the overview first among
what to look at. Teams own rules, and their firings are counted by the
team whose labels they have: a rule most of whose firings came from what
another team owns says so on its page, on that team's rules list and in
its digest.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="../assets/serve-rules-dark.png">
  <img alt="The rules list filtered to the noisy rules over four weeks: the filters above it, side by side with their counts, the filter in effect as a chip, the orders, a line saying rules can be picked to act on several at once, and the rules, each with a checkbox, its badges, its labels as chips, its team and how it was found, and its numbers, and under it a line of what they come with: the for suggested, notifications, the firings that caught nothing while people slept and off hours, re-fires, those excused, precision without the deploys' firings" src="../assets/serve-rules.png" width="820">
</picture>

Many can be changed at once: rules picked on the list get a task each, a
team or a disposition, and firings picked are written down as real, each
as its own page would change it, with the same checks and a line each in
the changes log. Up to twenty rules picked can each be proposed a change
(a new `for`, the one suggested for it, retiring it), each the proposal
its own page would make, with its backtest, waiting for a person to
approve it. Each of the overview's numbers opens the list that makes
it, over the same week and team (the noisy rules, the incidents missed);
its weekly charts mark deploys and quiet windows. A day's firings, on
Alert history (`/firings`), are counted by the hour, and each firing's
labels are chips that pick the
firings with them; an incident's page draws when each rule fired around
it.

A rule's page has its badges, what to check about it and what can be done
at its top; five numbers over the period picked (precision, firings,
incidents caught, the notifications it sent, and how alike its backtest is
to what really fired), each beside as long a span before when the ledger
holds all of it; its last 84 days, a cell a day, marked where it changed,
where it fired during a deploy of what it is about and where a quiet
window was over it; and its sections in tabs: summary, firings, versions
(each beside the one before), proposals and activity.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="../assets/serve-rule-dark.png">
  <img alt="A noisy rule's page over four weeks: its name, its badges (noisy six weeks, to tune) and three actions; its team, severity, for, group and the runbook the rule names; five numbers in a row: precision beside the four weeks before, better, firings beside them, told as neither better nor worse, incidents caught the same, the notifications it sent and its backtest with nothing beside; its last 84 days as a strip of cells, marked where the rule changed and on the days it fired during a deploy of checkout; its tabs; and what to do first" src="../assets/serve-rule.png" width="820">
</picture>

A proposal's page shows the change as a diff (unified or side by side, the
lines around it or the whole rule), its backtest as a table, now beside as
proposed, the approval, and what happened to it; the proposals and the
tasks have counted facets, as the rules do; a quiet window can be
previewed before it is saved (the rules it applies to, the firings of the
last four weeks it would have excused, when it falls next); `/manage`,
Administration, gathers the pages that set Lapilli up; and an action a
reader may not take
is shown disabled, with why.

What an operations team measures its alerts by is there too, week by week
and team by team (docs/design/noc.md): the share of critical firings that
caught something; firings that re-fired, the alert that did most, and the
whole weeks with one; firings during deploys of what they are about
(`--deploys`, from what `recipes/versions/argocd.sh` and `flux.sh` write:
a firing agreeing with a deploy on every label both have), shown apart,
with precision both ways; incidents and impact windows caught,
caught only by noisy rules or missed, and the lead of the first alert. The
rules, a rule's page, the incidents and the overview show them, the digest
and reports tell them, and `/api/v1/summary` gives them, in a shape that
keeps its schema, to dashboards and other tools.

A team holds its alerting to goals, declared in `--config` under `goals:`
(docs/design/goals.md): each a bound on one of the summary's numbers, for
everyone, a team, or each team (`team: "*"`):

```yaml
goals:
  - {name: critical-caught, measure: critical.share_caught, at_least: 0.8}
  - {name: none-missed, measure: incidents.missed, at_most: 0}
  - {name: paged, team: payments, measure: critical.firings, critical: [critical, page], at_most: 40}
```

Each whole week is met, missed or not known (with why: no record, nothing
to divide, a week the ledger holds only part of), never met for a week
nobody wrote anything down. It stays open until the runs are past its end
by the longest lead and a signal's `for`, and `--settle` after that, so
that no window a firing of its last minutes catches comes after it was
told: with the defaults and an impact signal, `goal.missed`, the tasks and
`/metrics` come at about 01:20 on Monday, and the digest, at 09:00, isn't
later. The Goals page shows each goal's twelve weeks;
the overview, the digest, reports and `/api/v1/summary` (`goals`) the
results; `/metrics` the last whole week's (`lapilli_goal_met`,
`lapilli_goal_known`, `lapilli_goal_value`, `lapilli_goal_weeks_met`), for
a team's own Prometheus to alert on. A goal missed is told
(`goal.missed`), and becomes a task with one press, or by itself with
`--goal-tasks`. A goal of `each: sale` is checked for each sale instead
(a marked window's kind, below), and `no_worse_than: before` sets a
sale's precision or median lead beside the four weeks before it:

```yaml
goals:
  - {name: sale-no-worse, each: sale, measure: precision, no_worse_than: before, by: 0.05}
  - {name: sale-none-missed, each: sale, measure: incidents.missed, at_most: 0}
```

A rule can be held to what is expected of it after it is deployed, as a
pull request's backtest holds it before (docs/design/expectations.md):
`--expect FILE` takes the expectations file `lapilli verdict --expect`
takes, read again every 15s and kept to once read the same twice in a
row, as `--config` is (the chart's `serve.expectations`, a ConfigMap).
One that can't be read is said on the status line, why to admins alone.
Rules kept only in Grafana or a ruler can be held under `expectations:`
in `--config`; a rule can carry its own budget in its annotation
`lapilli.dev/budget: "10/week, 2 asleep, 3 refires"`. An expectation names
its rules by `rule:`, `group:` or `labels:` (a selector, as
`--rules-select` takes, of the rule's own labels), and holds them to a
budget (`week`, `asleep`, `refires`), to incidents they must keep
catching (`catches`, `within`), or to scenarios they must pass
(`scenarios`, promtool's own test files, paths relative to the file that
declares them, inside its directory):

```yaml
expectations:
  - rule: ContainerRestarts
    budget: {week: 10, asleep: 2}
    catches:
      - {incident: INC-43, within: 5m}
    scenarios: [restarts.test.yaml]
  - labels: severity=page
    budget: {refires: 3}
```

After each run the leader checks each rule over the week so far and the
twelve whole weeks before, by the ledger's weeks (Monday, in `--tz`): a
budget is broken in a week it went over, the week so far as soon as it
does, met in a whole week it didn't, and not known in a week the ledger
holds only part of, or, for a rule only copied in, a week no span of its
source's copies covers whole (before the first, in a gap, past the last); while people slept is by the rule's team's own hours,
or the server's. A catch is judged against the incident written down,
again on a new version of the rule by replaying it as it is over the
incident, a few a run; with no incident written down, or one a rule only
copied in has no copies to reach, it is not known. A budget needs no
record. A scenario is run with `--promtool PATH` against the rule's group
as the rules in effect have it, written to a file serve makes, the test
file's `rule_files` rewritten to name that file alone: promtool is started
directly, with no environment but its `TMPDIR`, in a process group of its
own killed whole at 30s, 64 KiB of what it says at most, and, on Linux,
`--promtool-memory` (256Mi unless given) of memory, and files of 256 MiB
at most (its RLIMIT_DATA and RLIMIT_FSIZE: a
rule's annotation template can ask promtool for gigabytes; promtool's
release, built without cgo, starts in less than 64Mi, while one built
with cgo maps 8 MiB more for each of its threads, so give it 512Mi; macOS
sets no cap), five a run, never while the ledger's writer is held, and
one at a time across serve, a run's and an agent's check alike (an
agent's waits within its check's time, and is not known, saying so, past
it), each kept as begun before it starts, and again only when promtool, the group or the
test file changed: one that never ended (serve killed as promtool took
its memory) isn't run again each term; one serve stopped itself, shutting
down or no longer the leader, is run again. It is met when promtool says
SUCCESS, broken when it says FAILED (what it said is kept, and shown), and
not known when it couldn't run (a time out, a crash, past its memory), the
test file can't be read, the rule isn't in Prometheus's format (a Grafana
rule, a Loki ruler's, a cloud's alarm), or serve has no `--promtool`.
Lapilli's image has no promtool: the chart's `serve.promtool.image` copies
it from an image that holds it (Prometheus's own) with an init container,
or `serve.promtool.path` names it in an image of your own, and
`serve.promtool.memory` caps each run; `serve.scenarios` puts the test
files beside `serve.expectations`. The scenarios a pull request's backtest
sends to `--backtest-listen` are never run: each is said as not run
through lapilli serve. A check holds 10,000 rules and expectations at
most, the file's and `--config`'s first: a rule's annotation past what is
left is not known, never stopping theirs. What is broken shows on the
rule's Expectations tab, each expectation with its weeks; in the
overview's What to look at, made a task with one press; on the rules list
(`expect=broken`, and `expect=none` for the rules held to nothing); in the
digest; in `/api/v1/expectations`, MCP's `rule` and `/metrics`
(`lapilli_expectations_held`, `lapilli_expectations_broken`,
`lapilli_rule_expectations_broken`). It is told (`expectation.broken`)
once a rule, an expectation and a week, and with `--expect-tasks` made a
task for the rule's team, once a rule, an expectation and a week, none
while one is open; a catch or a scenario is of one week (its incident's,
or when what it found was found), and one that moves to another week,
still broken, isn't told again, nor made a second task, its task kept on
its tab.

Nobody writes expectations for rules that already page them, so serve
drafts them (docs/design/expectations.md, step 3). **From the history**: a
rule's Expectations tab shows what it is held to as it is now, the most it
fired in a whole week of the twelve before this one, while people slept
and re-fired, rounded up (1 to 9 as they are, 10 to 99 to the next 5, from
100 to two figures, never below 1: a rule that never fired is held to 1),
and the incidents it caught, each within its delay rounded up to a minute.
A team's rules', or every rule's, is a file to take
(`/expectations/draft.yaml?team=payments`), counted, never written by a
model: the same weeks give the same file. `lapilli verdict
--draft-expectations FILE` drafts one from the period it judges. **From an
agent**: MCP's `test_expectation` checks an expectations text, or a
rewrite of a rule, against the rule replayed over the twelve weeks, and
keeps nothing; given a rewrite that meets each expectation, losing no
catch, it answers with Markdown the agent or a person may post on the pull
request (Lapilli posts nothing). `propose_expectation` keeps an agent's
draft, checked, for a person to accept or drop on the rule's tab, as
`--mcp-propose` lets it ([Ask it from an agent](mcp.md)). A person keeps
the history's too, there. No one accepts a draft they proposed, or their
agent did; accepting is an approver's, in the browser, as approving a
proposal is (`--changes` on). With `--expect` and `--changes-token-file`,
an accepted draft with no scenario goes the git path: the expectations
file with it taken in is handed to the change recipes, which open a pull
request to `EXPECT_FILE`, the file in git `--expect` is made from
([recipes/changes](../recipes/changes/README.md)). Otherwise accepting
keeps the decision, and the tab gives the YAML to put where the
expectations are declared: Lapilli doesn't write a ConfigMap or `--config`.
Drafts count with proposals toward a person's 20 an hour; ten wait for a
rule at most, and 1,000 are kept, the oldest decided let go first, never
one whose pull request is waiting for a recipe or open.

A rule not yet written can be proposed too, held to what it is to do
before anyone approves it (docs/design/expectations.md, step 5). **Propose
a new rule** on the proposals page (`/proposals/new`, an editor's), or an
agent with `propose`, kind `add`: one Prometheus alerting rule (`alert`,
`expr`, `for`, `keep_firing_for`, `labels`, `annotations`), named apart
from every rule; where it goes, among the places serve offers (a group of
a rule file under `--rules`, or a new group in it; a Grafana folder's
group, and the data source its query reads; a group of a ruler's namespace
`--changes-in` names, or a new group in it; CloudWatch's or Google Cloud's
`--alarms`, under a name `--changes-in` names there), and whom it tells,
of whom the rules there already tell: in Grafana, a contact point of its
group's rules, one of theirs where each of them names one, or none where
none does (Grafana's notification policies route it by its labels, as they
route those); in CloudWatch, an SNS topic, an OpsItem or an Incident
Manager response plan the alarms there take, never a Lambda function or an
automation; in Google Cloud, a notification channel the policies there
tell. A rule added to a rule file or a ruler names no one: its labels
route it through Alertmanager, as any rule's there do. And an expectations
text holding it by `rule: NAME`, one expectation at least; its labels are
plain text, its annotations may be templates. Lapilli reads the rule
strictly and writes it itself where it goes, replays it over the twelve
weeks with nothing beside it, and checks each expectation as
`test_expectation` does. No one approves it while one is broken, or its
backtest didn't replay it; not known is shown and never met. Approved, it
goes the way a change goes there (a pull request, Grafana's API, the
ruler's, the cloud's), checked again first; undone, the rule is deleted.
Once it is added, what it is held to goes where an accepted draft goes.

Some weeks aren't like the others: a sale, a launch, a campaign. Marked
windows (docs/design/marked.md) name them, each of a kind a team chooses,
from a file (`--marked`), `--config`'s `marked:`, the Marked windows page,
or a PromQL expression that returns something while one is on (`--mark
sale=shop_sale_active == 1`). A firing that starts during one and agrees
with it is marked with its kind; nothing is excused, and every count is
read inside the kind and outside it: precision during sales beside the
four weeks before, the rules noisiest during them (`during:sale` on the
rules), and the incidents missed in each.

The ledger is one SQLite file (`--ledger DIR`), or a Postgres database
(`--ledger-url-file FILE`, below). It keeps each firing's stones
and each rule's by day and by week, so reading twelve weeks costs what
reading one does, and a change gives stones again only to the firings near
it. Measured with two million firings (a thousand an hour for twelve
weeks): an hourly run 0.18 s, writing an incident 52 ms, twelve weeks by
rule 2 ms, judging everything again after a change to the settings 15 s;
the pages, worked out afresh over 200,000 firings, 1 to 21 ms.

It isn't only to look at. Teams own rules (by a note, a name pattern, or
labels, in that order: a team that owns by labels can't take a rule another
team's pattern names) and incidents, so the dashboard, the rules and the tasks can be shown for
one team. Rules that say whose they are in a label (`team`, `owner`,
`squad`, or those `--team-labels` names) make their teams with one press:
the Teams page suggests one for each value on rules no team owns, and a
rule's page says how its team was found. A rule keeps a note: its team, whether to keep, tune or retire
it, its runbook. A quiet window (a deploy, maintenance, a load test)
excuses the firings in it that caught nothing, so expected noise doesn't
weigh on a rule's precision. A suggestion on the dashboard becomes a task
with a press, with a team, an assignee, a due date, a status and a link
to the issue in your tracker, and rules, incidents and tasks take
comments. A quiet window can repeat every day or every week, for a
nightly batch or a weekly release; every month, on a day (`15`), the last
(`last`) or a weekday of the month (`first sat`), for maintenance; or by a
cron expression (`0 2 * * 6`), hourly at most. Its start and end are its
first repeat, which must be one of its schedule's times, and each repeat is
shorter than the time between two; daylight saving is counted on the
calendar, as the reports' is. A quiet window can also be a team's: it
excuses only the firings of rules the team owns, and its page names the
team of each firing it excused. A team can keep its own working hours,
hours asleep and time zone, or its shifts (on the teams page, or
`work_hours`, `sleep_hours`, `tz` and `shifts` in the teams API): its rules'
firings that caught nothing are counted asleep and off hours by those, every
other rule's by `--work-hours`, `--sleep-hours`, `--tz` and `--shifts`. All of it is in the same ledger,
through the UI or the API; see [docs/design/work.md](design/work.md).

The weekly digest can go where each team reads it. `--team-digest TEAM=FILE`
posts a team its own rules and incidents, to the Slack-style incoming webhook
whose URL is in the file, and a team nothing happened to hears nothing.
`--notify-webhook-file` gets `digest.ready`, for everyone and for each team, with
the numbers as JSON and the report as `text`, for any chat or script to pass on.
`text` is the report as written, from rule names and incident titles others
wrote, so escape it for where it goes; `slack_text` is it as the Slack digest
posts it, with mentions and links escaped. The other fields that name things
(`noisy_rules`, `spare_rules`, `sources`, `notifications.most_for_nothing`,
`notifications.by_receiver`) are as written too: escape them as well. Only
Slack's markup is escaped in what Lapilli posts to a Slack-style webhook
(`<!channel>`, `<@U…>`, `<URL|text>`): a chat that takes one and reads markup
of its own, such as Mattermost, Rocket.Chat or Google Chat, may read a
mention (`@channel`, `@all`, `@here`) or a Markdown link (text in
brackets, a URL after it in parentheses) in a rule's name or an
incident's title as one, notifying a channel or linking elsewhere. Post there through something that escapes it for that chat.
Over twelve weeks, the digest also names the rules that are spare.

Reports go on schedules people set themselves, on the Reports page or with
`/api/v1/reports`: `daily@09:00`, `weekly mon@09:00`, `monthly 1@09:00` (or
`last`), or a cron expression such as `0 9 * * 1-5`, hourly at most, in the
ledger's zone or the team's. A preset covers the day, the week from Monday or
the month before it; a cron expression, the time since the one before, or a
span given with it (`24h`, `7d`). A report is for everyone or one team, and
holds the sections picked: `summary` (beside the span before), `noise` (and
what turned noisy), `missed`, `woken`, `notifications`, `replay`, `sources`,
`tasks`, `content`, for a week or longer, `suggestions`, and `firings`
(below). It goes to
destinations by name, never a URL typed in a page: `--report-dest NAME=FILE`
(repeatable, a Slack-style incoming webhook; in the chart,
`serve.reports.destinations`), `slack`, `team:NAME`, and `webhook`, which gets
`report.ready` with each section's numbers as JSON and the text as `text` and
`slack_text`; the fields that name things are as written, so escape them as
`digest.ready`'s. Each report is sent once for each span, through the outbox,
when the ledger has the span, or with what there is, saying so, once it fell
due six hours ago or half the time to the one before, whichever is less;
after a restart, only the last one due, and only while less time has passed
since it fell due than between it and the one before. A time daylight saving
skips falls due when the clocks jump; a repeated time, the first time.
Admins make and change reports; everyone sees them and a preview of each.
A team's editors, and editors, make a team's, sent to the team's own
destination (`team:NAME`).

`firings` lists the span's firings one by one, for a receiver that makes
something of its own of each: a review item for each alert that keeps
coming back, say. In `report.ready`, `numbers.firings` is `items`, the
firings that started in the span, each as `/api/v1/report`'s `firings[]`
has it ([fields](numbers.md#the-reports-fields)) with its rule's `severity`
and `team` as the CSV's columns have them, in order of their start, then of
their rule; `total`, how many there were; and `truncated`, true when it
lists only the first of them. It lists 5,000 at most, and `items` is 1 MiB
of JSON at most, whichever comes first: receivers often cap what a post
may carry at a few MiB, and a post one refuses is tried again, ten times
over about eight hours, then not at all, so `report.ready` keeps well under
that. A team's report lists its own rules' firings alone, judged by the
incidents it owns. Its line in the text says only how many, and how many
it lists when it was cut; the rules' names and the labels in `items` are as
written, so escape them as `digest.ready`'s.

The weekly digest is shown first among them. The flags define it until an
admin changes it on the Reports page or with `PUT /api/v1/reports/digest`
(`{"schedule": "weekly tue@10:00", "dests": ["slack", "ops"], "enabled":
true}`): from then on the ledger does, `--digest` isn't used (the log says
so at start), and the flags only name where it can go, now any destination,
a `--report-dest` too. It stays weekly, of the week from Monday, and its
suggestions are worked out at its time whether it is sent or not. A week
sent isn't sent again, before or after the change, to old destinations or
new; a week that falls due while it is off is passed over; and moving its
day never sends an older week. `DELETE /api/v1/reports/digest`, or "Back to
the flags", gives it back to the flags; see
[docs/design/reports.md](design/reports.md) and
[docs/design/serveops.md](design/serveops.md).

What happens (a task made, assigned, done or overdue, an incident written
down, a quiet window kept, a comment) can be told to a webhook, as JSON,
with `--notify-webhook-file`, and to Slack, as a line in `--lang`, with
`--notify-slack`. A tracker, a chat or a script takes the webhook; with
`--notify-webhook-secret-file`, each post to it is signed, so the receiver
can tell it came from this Lapilli
([Signed posts to the webhook](#signed-posts-to-the-webhook)). To keep
tasks in step with issues in GitHub, GitLab, Jira or Linear, run the
tracker's script in [recipes/trackers](../recipes/trackers) every few minutes
with `--tasks-token-file` (below). Notices are written in the
same transaction as what they tell of and sent after, so a receiver that
is down holds nothing up and misses nothing it comes back for.

Whether Lapilli itself earns its keep is on the Outcomes page (`/outcomes`),
worked out
from the same ledger. Each change to the rules it reviewed is measured: the
replay of the week before was its prediction, and the four weeks either
side are what came of it, as firings a week, incidents caught, firings
fewer since than at the rule's rate before (a rule written counts as
more), and whether it came out as predicted. Beside them: the weeks before
`lapilli serve` began and the latest (precision, firings and ● a week,
incidents missed); whether the record is kept up (two weeks without an
incident written down is flagged, since every firing then reads as ●;
impact windows to decide; failed runs; rules not replayed); and whether it
is used (edits and by how many people,
suggestions acted on, tasks done). The overview sums it up, the digest has
a line of it, `/api/v1/outcomes` has it as JSON, and `/metrics` has
`lapilli_serve_firings_avoided`, `lapilli_serve_rule_changes`,
`lapilli_serve_incident_last_timestamp_seconds` and `lapilli_serve_edits`,
so an alert can say when the record goes quiet. Nothing is sent anywhere,
and reading a page isn't counted.

The UI is checked against WCAG 2.2 AA (axe-core over every page, and tests
for each page's structure, contrast and charts, whose numbers are in tables
too), and works at 320px and 200% zoom, with no script.

The UI speaks English, Spanish, Japanese and Korean: the browser's
language, or the one picked at the top of the page; `--lang` picks the
language of what `serve` posts to Slack and in its reports. Each language is
one file of translations, `internal/serve/i18n/LANG.json`; a test fails if a
text has none, so a new language can't be left half done. The Spanish and
Japanese were written with a machine's help, in the plain words SREs use, and
want a native speaker's review: corrections are welcome as pull requests.

It listens on `127.0.0.1:8080`. To serve it at a URL people open, sign them
in, one way of three ([docs/design/signin.md](design/signin.md)):

- `--oidc-issuer`, `--oidc-client-id` and `--oidc-client-secret-file`:
  lapilli signs people in itself with any OpenID Connect provider, as
  Grafana and Argo CD do, and keeps a session in a signed cookie;
  `POST /auth/logout` ends it. Register `--external-url`'s `/auth/callback`
  as the client's redirect URI. An API client or an agent sends an ID token
  the provider gave this client as a bearer token.
- `--proxy-jwt google-iap`, `aws-alb` or `jwks`: the JWT a sign-in in front
  signs (Google Cloud's Identity-Aware Proxy, an AWS Application Load
  Balancer's sign-in, oauth2-proxy and the rest), checked against its keys
  on every request.
- `--user-header`: the plain header a proxy that signs people in sets, so
  changes are recorded under it; the proxy must then be the only way in.

With the first two, every page and API call but the health checks needs
someone signed in, and only those a role names come in: an identity
provider signs in whoever it knows, which for a public one (Google's for
any account, GitLab.com, GitHub through Dex, a multi-tenant Entra ID app)
is anyone. `--viewers` names who may look; `--editors` and `--admins` who
may change. `--user-claim` (`email`) and `--groups-claim` (`groups`) name
the person and their groups, and `--oidc-hosted-domain` checks Google's
Workspace domain. `--read-only` takes no changes. With `--editors` and
`--admins`, people or groups (`--groups-header`, `--groups-claim`), only
editors may write and only admins may change the settings and the teams;
the rest may only look. Lists take `*@DOMAIN`, everyone whose name ends in
`@DOMAIN`, and `*`, everyone signed in, which with a sign-in of lapilli's
own gives a role only once `--viewers` says who comes in. Write
`user:NAME` or `group:NAME` where someone could pick a name that is a
group's. Sessions are signed with `--session-key-file`'s key, or one made
for the process; with an http `--external-url` (this machine only), give
it a name of its own under `.localhost`, as cookies aren't kept to a
port.

With no roles, who may change what depends on where it listens. On
loopback alone (`127.0.0.1`, `localhost`, the default), everyone who
reaches it is an admin: that is this machine's users. Beyond it (`:8080`,
as the chart listens, a pod's address), everyone may only look, since every
pod or machine that shares its network can reach it; give roles behind the
proxy, or `--everyone-admin` to let everyone change anything anyway. The
log says at start which applies. (Before 0.28.1, everyone was an admin
wherever it listened.) With `--oidc-issuer` or `--proxy-jwt`, loopback
makes no one an admin: everyone signed in may only look until roles say
otherwise, and `--everyone-admin` is refused.

With `--admins`, admins can also give roles on the Roles page (from the
Teams page) or with `/api/v1/roles`, kept in the ledger: an editor's, of
every team or of one, to `user:NAME` or `group:NAME`. A team's editors
change that team's tasks, quiet windows and reports, notes on the rules it
owns, and comments on what is its, and nothing of another team's or of
everyone's; the ledger checks each in the write. The UI never makes an
admin or an approver, and can't take away a role the flags give: those stay
`--admins`, `--editors` and `--approvers`. Only admins see who holds a
role. A team removed takes its editors with it. A team's name is unique in
any letter case. With `--config`, the roles it declares (`grants`) are
taken away there alone.

`--proposals-expire 30d` closes a proposal nobody approved within thirty
days as expired, so its rule can have another; off by default.

With `--user-header`, the proxy must be the only way in. The chart's
`serve.networkPolicy` lets in only the proxy (or the ingress controller,
the Gateway, the load balancer in front), and Prometheus only to
`/metrics` on a port of its own (`--metrics-listen`). With
`serve.userHeader`, the chart refuses to go without it. The chart serves
the UI at a URL with `serve.service` (a LoadBalancer or NodePort),
`serve.ingress` or `serve.httpRoute` (Gateway API), and `serve.signIn`;
its README has each cloud's load balancer. A proposal's backtest takes up
to `--timeout`; a proposal for many rules starts none after twenty seconds
and says which weren't tried. Where the proxy in front gives a request less
than `--timeout` (30 seconds by default for some load balancers' backends),
raise it for `POST /proposals` and `POST /rules/bulk`, or lower
`--timeout`.

Posts from other sites are refused, and the pages run no scripts. Only host
names it is reached by are answered, so a page elsewhere can't point a name
of its own at it (DNS rebinding):

- an address, `localhost`, and names under `.svc`, `.cluster.local` or
  `.internal`;
- the name in `--external-url`;
- what `--allowed-hosts` adds, such as the name a proxy passes on.

Others get 421.

In the chart, `mode: serve` runs it as one replica with the ledger on a
PersistentVolumeClaim, behind a ClusterIP Service:

```sh
helm install lapilli oci://ghcr.io/lapilli-project/charts/lapilli -n monitoring \
  --set mode=serve --set prometheus.url=http://prometheus:9090 \
  --set 'rules.configMaps={alert-rules}' --set 'match={service}'
kubectl -n monitoring port-forward svc/lapilli 8080
```

A release that changes the ledger's schema upgrades the ledger as it
starts, and the releases before can't open it after: keep a copy of the
ledger before such an upgrade, to go back to
([Going back after a schema upgrade](../charts/lapilli/README.md#going-back-after-a-schema-upgrade)).

## Before it is deployed

`lapilli preflight` takes serve's flags, as serve reads them, and says
what serve will meet, before it is deployed: whether it reads the rules
and how many it can replay, whether it reaches the history and signs in to
it, whether Grafana's token reads what serve reads, what the files and the
tokens hold, the ledger it would open, and how many requests an hourly
run, the backfill and what is asked beside them will make, how long they
take under `--max-rate`, over how many nights of `--catch-up-hours`, and
how much memory to give the pod (docs/design/preflight.md). Each item is
ok, a problem or unknown, with why, and for a problem what to change:

```sh
lapilli preflight --rules alert-rules/ --prometheus http://prometheus:9090 \
  --max-rate 0.5 --catch-up-hours 01:00-06:00 --catch-up-rate 0.5
```

```
lapilli preflight: 12 items: 10 ok, 0 problems, 2 unknown; 4 requests asked
ok       rules.read          203 alert rules (Prometheus), 40 recording rules, from --rules
ok       rules.replayable    203 can be replayed
ok       history.reach       [--prometheus] answered vector(1) in 38ms, with no credentials
unknown  history.alerts      no firing alert in the last day, or no ALERTS kept: memory is the caps'
                             bound
...
ok       ledger.sqlite       a new ledger: serve makes it, at schema 019
Estimates
ok       hourly run          about 253 requests a run, the most 430, 6,054 a day: at --max-rate 0.5
                             a run takes 8m at least, the most 14m; once caught up, with what is
                             asked beside them, 14% of --max-rate
ok       backfill            12,040 requests over 28d, 28 pieces of a day at most, the most 430: at
                             least 6h41m at 0.5 a second
                             why: each day's in 14m at most, within --timeout 50m
ok       nights              2 nights at least, in 01:00-06:00 at 0.5 a second, 9,000 of the
                             backfill's requests a night
ok       memory              no firing read: the caps' bound, 5.2 GB (8 x 10,000,000 x 64 bytes,
                             beside 64 MB of caches): a request of 5.2 GB, a limit of 10.5 GB
```

It writes nothing and replays nothing, held by the code: every request it
sends goes through an allowlist that refuses any other before it is sent
(the history's `vector(1)` and a day's count of ALERTS; the rules, the data
sources and the notifications Grafana is read for; the sign-in's discovery
and keys; the clouds' credentials; and, with `--changes`, what a token or a
cloud's identity may write: Grafana's list of the token's actions, IAM's
policy simulator, Cloud Resource Manager's `testIamPermissions`, Azure's
permissions, deny assignments and locks, OCI's policies), the ledger is
read through a probe that
opens SQLite read-only and Postgres in read-only transactions, and no rule,
`--impact` nor `--guard` is evaluated. What the clouds' credential chains
do of their own is outside the allowlist: Google's metadata server (and
whether it is there, off Google Cloud), the Azure CLI (which may write its
token cache), OCI's instance and resource principals and OKE's workload
identity (its auth service's POST among them), and the processes AWS's
`credential_process` and Google's executable credentials start. What it prints is redacted as a
pull request's backtest comment is. It exits 0 with no problem, 1 with
one (or an unknown, with `--fail-unknown`), 2 when the flags are wrong;
`--json` gives the report with ids and fields that stay the same
(docs/numbers.md). Run it where serve runs, with its service account and
volume, as a Job or `kubectl run` of the image; in CI, `--offline` asks
nothing, reads no secret's file nor `--ledger-url-file`, and works out what
needs no credential: the flags, the rule files, `--config` and the
estimates of requests and time. With `--changes`, it asks what
`--changes-grafana-token-file`'s token and each `--changes-*` identity may
write, of an API that says so without anything being written
(`grafana.write`, `changes.aws`, `changes.azure`, `changes.google`,
`changes.oci`): AWS's needs `iam:SimulatePrincipalPolicy` on the role's own
ARN, Azure's `Microsoft.Authorization/permissions/read`,
`denyAssignments/read` and `locks/read`, and OCI's that the user may
inspect groups, users, compartments and policies in the tenancy, or the
item is unknown; an OCI policy with a condition is never counted as a
grant. A Mimir's or a Cortex's `rules:write` can't be known without
writing: unknown. With `--backtest-listen` ([below](#pull-request-backtests)),
it reads `--backtest-trust` as serve reads it at start and says what serve
does with it (`backtest.trust`: the entries by their names, CIs and the
claims they pin, never what a claim is pinned to; or why serve stops), asks
each issuer the file names for its discovery document and keys, once, as a
token's first check would (`backtest.issuers`; GETs of those alone, the
keys at the issuer's own origin or the entry's `jwks_url`), and estimates
the lane: the hourly runs timed at `--max-rate` less `--backtest-share`
too, as while a backtest runs, and how many requests a backtest, and how
many days of ten of the rules changed, fit in `--backtest-timeout`
(`estimate.backtests`). The trust file needs no credential, and is read
with `--offline` too; a plain token's file isn't, then.

## The ledger in Postgres

A SQLite file is one disk's. To keep the ledger where you keep the rest, with
its backups, point in time recovery and failover, give `lapilli serve` a
Postgres database, 15 or later (docs/design/postgres.md):

```sh
umask 077   # the file only you can read
cat >ledger-url <<'URL'
postgres://lapilli:PASSWORD@db.example:5432/lapilli?sslmode=verify-full
URL
lapilli ledger copy --ledger lapilli-ledger --to-url-file ledger-url   # once, with serve stopped
lapilli serve --ledger-url-file ledger-url --rules rules/ --prometheus http://prometheus:9090
```

The connection string is read from the file, as pgx reads one, and said
nowhere: not in a flag, a log, an error, a page, `/metrics` or an answer to
an agent. To another machine, say `sslmode=verify-full`: a string that says no
`sslmode`, or pgx's default `prefer`, which checks no certificate and falls
back to none, is refused at start; `require` or `disable` is taken at its word,
and serve warns. Give the database to the ledger's role alone (`REVOKE CONNECT
... FROM PUBLIC`). Every page, report and answer is what the
SQLite ledger gives from the same writes, and the tests run on both.
`lapilli ledger copy` copies a ledger into an empty one, a SQLite ledger
into Postgres or one back, in one transaction, and reads both back to
check; it is refused while serve holds the ledger. In the chart,
`serve.ledger.postgres.secret` names the Secret holding the string, and no
PersistentVolumeClaim is made.

## Several replicas

On a Postgres ledger, `lapilli serve` runs as several replicas
(docs/design/replicas.md). One leads, on a lease the database keeps, and
runs what must run once: the runs and the backfill, the jobs, the digest
and the reports, the outbox, changes carried out, the rules watched, the
on-call feeds. Every replica serves the UI, the API, MCP, sign-in and the
ports that take alerts and incidents, and writes what it is sent. A
follower takes the lease within some 17 seconds of the leader being killed
or cut off (15 s measured, the leader `SIGKILL`ed), within 2 when it
stops; the old leader stops first, by its own clock, and what it would
still write as the leader is refused in the database. Notices are claimed
one at a time, so two leaders for a moment send none twice. A write on one
replica shows on the next request to any other. In the chart:

```sh
helm upgrade lapilli ... --set serve.ledger.postgres.secret.name=lapilli-ledger --set serve.replicas=3
```

They roll one at a time, with a PodDisruptionBudget; `/readyz` and
`lapilli_serve_leader` say which pod leads. More than one replica needs
Postgres: the chart refuses `serve.replicas` above 1 on a SQLite ledger,
and a sign-in of lapilli's own needs the session key from a Secret of your
own, which every pod mounts. From 0.35 or before, upgrade with one
replica first: its pod holds the ledger alone, and refuses new ones until
it stops.

## Pull request backtests

A CI job can backtest a pull request's rules through serve, with serve's
access to the history and its record, and post the same comment
`lapilli verdict --compare --changed-only --markdown` writes: no metric
credential in CI, the incidents and impact windows serve keeps judging the
firings, and one quota for both (docs/design/backtests.md). Off unless
given:

```sh
lapilli serve --rules rules/ --prometheus http://prometheus:9090 --max-rate 2 \
  --backtest-listen :8084 --backtest-trust /etc/lapilli/backtest/trust.yaml
```

The port takes `POST /v1/backtests`, and `GET` and `DELETE
/v1/backtests/{id}`, and nothing else (the API is in
[docs/design/backtests.md](design/backtests.md), "Flags and API").
[recipes/backtest](../recipes/backtest) sends it the rules in its serve
mode (`LAPILLI_SERVE`, and the action's `serve-url` and `audience`),
reads the report back, and posts it as its own. The trust
file says who may ask, each entry
a CI's issuer of ID tokens or a plain token, and is read at start: one
that can't be used stops serve from starting.

```yaml
trust:
  - name: alerts                 # what the log, /metrics and Run history call it
    issuer: https://token.actions.githubusercontent.com
    audience: https://lapilli-ci.example.com
    claims:                      # each exactly this, as a string
      repository_id: ["123456789"]
      event_name: pull_request
    days: 14                     # the most one of its backtests replays
    hourly: 30                   # backtests it may start in an hour
  - name: monitoring-gitlab
    issuer: https://gitlab.example.com
    ci: gitlab                   # known from the issuer for gitlab.com
    audience: https://lapilli-ci.example.com
    claims:
      project_id: "42"
      pipeline_source: merge_request_event
  - name: monitoring-bitbucket
    issuer: https://api.bitbucket.org/2.0/workspaces/example/pipelines-config/identity/oidc
    audience: https://lapilli-ci.example.com
    claims:                      # as Bitbucket writes them, in braces
      repositoryUuid: "{1a2b3c4d-2222-4333-8444-5555555555ee}"
      workspaceUuid: "{4b1d9c2e-7a3f-4e5b-8c6d-0e1f2a3b4c5d}"
  - name: monitoring-azure
    token_file: /etc/lapilli/backtest/azure-token   # 32 characters at least
```

- **An ID token** is checked as a bearer token of `--oidc-issuer` is: its
  signature, by a key of the issuer's (from its discovery document's JWKS
  at the issuer's own origin, or `jwks_url`), by an algorithm of a key
  pair, never `none` or HMAC, and an RSA key of 2048 to 4096 bits; `aud`
  the entry's `audience` and nothing beside it (but, of a Bitbucket
  entry's, its workspace's own, below); `exp` and `nbf`; `iat`
  within the last hour, whatever `exp` says; each claim pinned exactly,
  a string as it is, an integer by its decimal. `jti`, or with none (as
  Forgejo's) the hash of what it says as it was signed, never how it was
  written, starts one backtest, with the body it first came with: sent
  again, with that body or another, 409, its body unread and no key given
  again (two at once, the first not yet kept when the second was read: the
  same body is answered with the same backtest, 200, without its key).
- **The audience** is required, and should be the port's URL. GitHub's
  default (the repository owner's URL), Bitbucket's (a workspace's,
  `ari:cloud:bitbucket:...`), one at the issuer's own origin, `sigstore`,
  and the audiences of AWS STS, Microsoft Entra's token exchange and
  Google Cloud's workload identity federation are refused: a token made
  for one of them could be sent here, or one made here sent there.
- **Pins.** For GitHub Actions (`ci: github`, known from its issuer, an
  enterprise's on GitHub Enterprise Cloud, `.../<slug>`, and GitHub
  Enterprise Server's, `https://HOST/_services/token`), Forgejo Actions 16
  or later, whose tokens carry `repository_id` (`ci: forgejo`, known from
  an issuer ending in `/api/actions`), and GitLab CI (`ci: gitlab`, known
  for `https://gitlab.com`, given for a GitLab of one's own), the entry must pin the
  repository's id (`repository_id`, or GitLab's `project_id`), which may be
  a list, one of them enough; and the event, `event_name` (GitLab's
  `pipeline_source`), to `pull_request` or `push` (`merge_request_event`
  or `push`): another can run a fork's pull request in the repository's
  own context. For Bitbucket Pipelines (`ci: bitbucket`, known from its
  issuer, a workspace's,
  `https://api.bitbucket.org/2.0/workspaces/WORKSPACE/pipelines-config/identity/oidc`),
  the entry pins `repositoryUuid`, which may be a list, and
  `workspaceUuid`, each lowercase in braces as Bitbucket writes them, and
  no event, as its tokens say none. Every other pin is one value,
  `repository_owner_id` among them, which narrows, never replaces, the
  repository's. An issuer known
  neither by its URL nor by `ci:` must pin `sub` and the event, one value
  each, or serve doesn't start; and under every issuer, an event pinned
  is one of those, never `pull_request_target`, `workflow_run` or
  `schedule`.
- **Forks.** A fork's pull request isn't backtested because its CI gives
  its run no token, not because of a pin: a run of a fork's pull request
  in the repository carries the repository's own `repository_id`, as a
  branch's does. GitHub gives a fork's `pull_request` run a read-only token
  and no ID token, unless, on a private repository, "Send write tokens to
  workflows from pull requests" is on: then a fork's run gets one no claim
  tells from the repository's own. Keep that setting off; a workflow's
  `if:` keeps no one out, as the pull request can change it. Forgejo's
  reference says `enable-openid-connect` "is disabled for `pull_request`
  events from forked repositories". GitLab names a fork's merge request by
  the fork's own project (`project_id`), which the entry doesn't pin.
  Bitbucket says "Pull requests from a forked repository don't trigger the
  pipeline"; a fork's own pipelines run in the fork, another
  `repositoryUuid`, and, in another workspace, under another issuer.
- **Bitbucket's audiences.** Bitbucket puts its workspace's own audience
  (`ari:cloud:bitbucket::workspace/UUID`) in every token, beside what the
  step asks for under `oidc: audiences:`. A Bitbucket entry takes a token
  naming its `audience` alone or beside that of the workspace it pins,
  and nothing else: never a third. Such a token is one any service that
  trusts the workspace's own audience takes too, so the step that asks
  for serve's audience asks for no other, and serve is reached over TLS;
  and the other services that trust the workspace (an AWS role made as
  Bitbucket's guide makes it, and the like) should trust an audience of
  their own, which the steps that need them ask for under `oidc:
  audiences:`, rather than the workspace's: a token serve is sent is then
  of no use to them.
  Its token has no `jti`: it is known by what it says, as Forgejo's is.
  It names no pull request, so a Bitbucket backtest takes no other's
  place.
- **A plain token**, for a CI with no ID token serve takes (Azure
  Pipelines, whose token is issued by the Microsoft Entra tenant, an
  issuer every app of it shares; Gitea; Jenkins), is read
  for each request, and refused while it is the same as
  another token serve takes. It is a long-lived secret: whoever holds it
  backtests as that entry until it is rotated.

The replica that leads runs the queue, one backtest at a time, beside the
runs and not in them, the oldest of the entry that started one least
lately first. While it runs one, its share of `--max-rate`
(`--backtest-share`, a quarter by default) is kept for it: the runs ask at
the rest, and the lane at its share, which nothing else takes, so the two
never pass the rate together; once it runs none, the runs have the whole
rate again. Without `--max-rate`, a quarter of `--parallel` at once, one
at least. A backtest that couldn't finish within `--backtest-timeout` at
its share is refused before it waits, saying how many requests it needs
and what period would fit. An entry's `hourly` is taken before the body is
read. What can be told without the body is refused before it is read: the
queue with no room for the entry, the reads full, the ledger not ready,
each its hour given back; and a token that started a backtest already
(409, whatever body it comes with: no key is given twice), which keeps its
hour (since 0.63.2), as its job has its backtest. Once the body is read, a
request refused keeps its hour: what it cost to read and check is what
`hourly` bounds. An entry has one body read at a time, two of all
entries at once, each within 5 seconds of its headers. 20 wait at most
(`--backtest-queue`), 3 of an entry, one of a pull request the token's
`ref` names (a newer commit's takes the place of the one waiting for the
same repository, by the repository's id as the token says it, since
0.69.1: renamed meanwhile, it is still itself; a number the job said takes
none's: GitLab's ID token names a merge request's
source branch, not its number, and Bitbucket's no pull request, so their
backtests don't take each other's place); past them, 429 with
`Retry-After`. One the leader stopped while running is run again by the
next, twice at most. A job reads its backtest by its id and key, told from
the keys each replica holds: those it queued, and the ledger's, listed one
at a time, a second apart at least. A pair known is read, whatever else
comes from the job's address, which behind an ingress is everyone's. A
pair not known waits up to 2 seconds for a listing begun after it came, so
that a backtest queued through another replica is found on its first poll,
and is a 404 if still not known; 20 of an address wait at once, one more
each second, and past them one is a 503 with `Retry-After` at once, and a
listing begun, so that the job's next poll finds its backtest. 4,096
addresses are kept so, and as many backtests' reads: past them, only
those whose waits are all back are let go, and while none can be, one not
kept is answered as one past its own (since 0.63.2; before, all were let
go at once). One that
guesses reads no backtest's row, and the ledger is listed a second apart
at least, however many guess. With replicas, the queue and each entry's
hour are the ledger's: any replica takes a backtest and answers for it.

What the pull request's PromQL can do: it runs as range queries, with
serve's access, so it can read whatever that access reads, a count at a
time in the firings the comment says. Give serve's history credential what
the rules need and no more. The port takes rules in Prometheus's format;
Grafana-managed rules, as `--rules` reads a provisioning file, only on the
data sources the rules serve runs read already: each of a rule's queries
names, by its uid exactly, a data source whose queries a rule in effect's
replay asks, and asks it as that rule does (PromQL of `--prometheus`, or
LogQL through `--grafana`'s data source proxy, or `--loki` without it),
never one of Grafana's own (`grafana`, `-- Mixed --`, `-- Dashboard --`),
so that a pull request makes serve read nothing with its credentials that
it doesn't read for its own rules; and a Loki ruler's rules, from a file
the job marks as one (`"loki": true`, the recipes' `LOKI_PATHS`), only
where serve has `--loki` and its rules in effect hold one, asked with
serve's tenant. A rule refused for what it reads is said to read "a data
source no rule in effect reads", whatever why, naming nothing of serve's.
50 that differ at most, a `for` or `keep_firing_for` of 48 hours and
a group interval of an hour at most; each selector one metric's, by its
name (`{job="api"}` and `{__name__=~"..."}` are refused); what a rule
reads back, its range and offset added to those of every subquery around
them and its group's `query_offset`, 8 days at most in all (`rate(x[1d] offset 7d)` fits,
`rate(x[7d] offset 7d)` doesn't); a subquery of 11,000 points at most, its
step written in it (`[1d:1m]`, not `[1d:]`: without one, Prometheus steps
at its evaluation interval, which Lapilli doesn't know, so the points
can't be counted; add the step); ranges and offsets written as durations;
and no `@`; no `info()`, which reads `target_info` beside what it names
(since 0.63.2); comments read past, as Prometheus does; PromQL that every
history `--prometheus` takes reads alike (since 0.63.2): outside its
strings, ASCII names, numbers and durations as Prometheus writes them,
operators and brackets alone, no `\r`, an `offset` or `@` right after a
selector, a range or a subquery, no `\` in a label's name in quotes, and
none of MetricsQL's own (`WITH`, `or` in a selector's braces,
`keep_metric_names`), which VictoriaMetrics would read past the bounds; each range query with a timeout of a minute; days 1 to
14, and the entry's `days`. A Grafana rule has 8 queries of data sources
and 16 queries and expressions at most, each query's `relativeTimeRange`
8 days back at most, and each query the replay asks held as a rule's, its
relativeTimeRange's end added to what it reads back, and for a data source
query a day more, as the replay asks it over the day before when it
returns nothing (since 0.63.2), and in PromQL a group's interval of 8
seconds at least, as that day is a subquery at it. A query in LogQL is
a metric query, never one of log lines; each stream selector names its
streams by a label matched with `=` to a value (`{app=~".+"}` alone is
refused); a range with its offset reaches 8 days back at most, with no
subquery and no `@`. The lane asks a Loki through a client of its own, at
its share of that Loki's rate, kept for it while a backtest asking that
Loki runs, and gives up on each request after a minute (since 0.63.2), as
a Prometheus is told to. What it learns comes a bit at a time, as a rule
fires or doesn't: an entry's `hourly` is what bounds how fast. Keep it as
low as the pull requests need; a day's cap, of the rules that differ
times the backtests, is the `hourly` times 24 times 50 at most. `hourly`
bounds what an entry costs the runs too: the share is kept for the lane
for the whole of each backtest, so one entry asking back to back can hold
serve's runs at three quarters of `--max-rate` (at the default share) for
as long as its backtests last, and a backtest whose queries hang keeps the
share while asking little, until `--backtest-timeout`. The lane asks a
quarter of `--parallel` at once: with a high `--max-rate` and a history
that answers slowly, it asks fewer a second than its share, and a
backtest the port counted as fitting in `--backtest-timeout` can run out
of it, which the comment says.

The comment's heading says lapilli serve's record judged it. It holds the
rules' names and labels as the pull request wrote them, the counts, and
the incidents the change catches differently, by their ids: their titles,
written in a tracker, only for an entry that says `incident_titles:
true`. No label of a series a rule read, no word of what the history
answered, nothing of where it is; the JSON leaves out each side's
incidents, the thresholds and hours it was judged by, and the impact
signals. The Run history page
lists each backtest (when, the entry, the repository, the rules that
differ, the requests, how long, how it ended; no rule's name), and
`/metrics` has `lapilli_serve_backtests_total{trust,result}` and
`lapilli_serve_backtests_waiting{trust}`. A backtest is kept a day after it
ends, 200 at most. In the chart, `serve.backtest`
([its README](../charts/lapilli/README.md)).

## Firings recorded elsewhere

A cloud's own alarms that aren't written in PromQL can't be replayed, but
what they fired can be kept and judged with the rest. With
`--firings-token-file FILE`, the port `--incidents-listen` opens takes
firings too, from a job holding that token, its own: a token that may copy
incidents can't write firings, and the reverse, and neither may be the
notifications token, which alert managers keep in webhook URLs.

```sh
export SINCE=2026-11-08T00:00:00Z UNTIL=2026-11-09T00:00:00Z
recipes/firings/cloudwatch.sh > firings.jsonl &&
  LAPILLI_TOKEN_FILE=firings-token recipes/firings/put.sh http://lapilli:8082 cloudwatch-prod < firings.jsonl
```

`put.sh` sends them as `PUT /api/v1/firings/SOURCE?from=&to=`, one a line
as `--firings` reads them. The source is a name the job picks
(`cloudwatch-prod`, `azure-shared`), and the period replaces: the source's
firings that start in `[from, to)` are those sent, the way a run replaces
the firings that start in its period. Sent again as they are, nothing
changes and nothing is judged again, so a job can send the last day every
hour, and everything the cloud keeps every week, which ends the alarms that
were still on. A firing without an end is still on until one is sent with
its end. A write is 31 days, 16 MB and 100,000 firings at most, nothing in
it starts or ends more than an hour ahead of lapilli's clock, and names have
no character that doesn't show. The ledger keeps 50 sources, 5,000 rules
from each and 20,000 from all, and 500,000 alerts from each; alerts with no
firing left go with `--keep`. The token writes twenty periods at once, for a
first copy of what the cloud keeps, then one each five seconds. A write
judges again as far as a period past its period: an alert's next firing
after one that ran on longer is judged again when a write or a run reaches
it.

Such a rule is named as its source names it, and told apart when another
rule has the name (`checkout-5xx [cloudwatch-prod]`); a replayed rule that
comes later takes the name. Its firings are judged as any are, beside the
replayed rules: stones, precision, what it adds, incidents written down
later, teams, hours, quiet windows, tasks, the digests, `/metrics` and the
agent's reading tools. What needs a rule's condition stays with the
replayed ones: change reviews, longer `for`s, near misses, `compare`. Its
page says where it was recorded and why. The Run history page lists the
sources,
`/api/v1/sources` has them, and `/metrics` has
`lapilli_serve_source_last_timestamp_seconds{source}`, so an alert can say
when a job stopped copying. Lapilli says so itself, too: a source nothing has
been copied in from for `--source-quiet` (26 hours unless given; give a
weekly job's 8d) is told of once, to the webhook and, with `--notify-slack`,
to Slack. The weekly digest says which sources firings were copied in from,
how many each brought that week, and which has gone quiet.

Grafana's own history is copied in by `lapilli serve` itself. With
`--grafana`, each rule it holds that can't be replayed is judged from what
Grafana's alert state history says it fired (as
[`lapilli verdict`](verdict.md) judges it): each round, after the runs,
the history up to where they got is copied in under the source `grafana`,
a day a write, the `--settle` before each written again. The rule and its
history are one rule, under the rule's name, as a cloud's alarm read with
`--alarms` and its history are: its page says so, and its record is what
the copies cover. No job may write to `grafana`: the port refuses it. A
day the history can't be read for (refused, more than is read at once, or
no longer kept) is left a gap, logged, never taken for a quiet day, and
the copy goes on. A rule the history can't say all of (another rule of its
title, changes of it passed over, a firing a write can't hold) is left out
of the day alone: the other rules' day is written, and the rule has no
record for the period, on its page and wherever its record is read, also
once it is judged again;
`--grafana-history=false` turns it off
([docs/design/grafanajudged.md](design/grafanajudged.md)).

With the firings token, `--rules` and `--prometheus` are optional: a team
with only cloud alarms runs `lapilli serve --incidents-listen :8082
--firings-token-file FILE`, nothing is replayed, and what comes in is
judged as it comes. `--impact` still needs `--prometheus`. In the chart,
`serve.firings.tokenSecret` names the Secret.

`lapilli serve -h` lists the rest: how often to run (`--every`), how far to
replay on the first start (`--backfill`, 28 days) and how long to keep
firings (`--keep`, 400 days).

## Tasks kept in step with a tracker

A task says who does what by when; the work is tracked where the team tracks
the rest. With `--tasks-token-file FILE`, the port `--incidents-listen` opens
lets a job holding that token, its own, read tasks and set their link and
status, and nothing else. [recipes/trackers](../recipes/trackers) has a script
for each of GitHub, GitLab, Jira, Linear, Azure Boards and Shortcut, alike: each run makes an issue
for each open task without one, with a link back to the task's page, and
puts the issue's URL on the task; then a task whose issue was closed is
closed, `done` or `dropped` as the issue was, and one closed that way is
opened again with its issue. With `CLOSE_ISSUES=true`, an issue whose task
someone closed in Lapilli is closed too, unless the issue changed since.
Only issues the script's own account made for a task are taken for it,
followed or closed, by the marker line the script ends them with: anyone
may write a task's marker in an issue, or in a task's text, and an editor
may link a task to any issue. Who closed a task, and when, comes from who
set its status (`status_by`, below), not from whoever last edited it. A
task the tracker refuses is said and skipped, and the run fails once the
rest are done; one whose issue was deleted is said and left as it is. A
GitHub App's or Actions' token can't say who it is: set `GITHUB_LOGIN`
(`NAME[bot]`). With `COMMENTS=true`, comments are kept in step too, both
ways, each once: people's comments on a task are posted to its issue,
quoted, and the issue's comments copied to the task, shown as the
tracker's, with the token's `GET` and `POST /api/v1/tasks/{id}/comments`;
an edit after that isn't followed.

```sh
GITHUB_REPO=example/alerts GITHUB_TOKEN_FILE=gh-token \
  LAPILLI_TOKEN_FILE=tasks-token recipes/trackers/github.sh http://lapilli:8082
```

The job reads tasks with `GET /api/v1/tasks?linked=&status=&updated_after=&after=&limit=`
(`updated_after`, an RFC 3339 time, leaves out the tasks not changed since),
each with `status_by` and `status_at`: who set the status it has, and when,
as the changes kept say (a later edit of anything else doesn't count;
neither is given once the change that set it is pruned). A
link is an `https` URL of 2,048 characters at most, set once; the page
shows it with `rel="noopener noreferrer"`. The token can't make, delete or
retitle a task, read anything else, or hold the same value as another token.
What it writes is noted as `tasks-token` and isn't counted as anyone's use.
In the chart, `serve.trackers` names its Secret and, with `tracker`, runs the
script as a CronJob.

## Who is told what

By default, every event (a task made, a proposal approved, a goal missed, a
source gone quiet) goes to `--notify-webhook-file` and, with
`--notify-slack`, to Slack. Routes in `--config`, under `notify:`, send
each by its kind, its team and its labels instead, to destinations the
flags name: `webhook`, `slack`, `team:NAME` (`--team-digest`), a
`--report-dest`'s name, or a JSON webhook of its own,
`--webhook-dest NAME=FILE`, signed with `--webhook-dest-secret NAME=FILE`.

```yaml
notify:
  routes:
    - kinds: [task.*, proposal.*]
      teams: [payments]
      to: [team:payments]
    - labels: {severity: page}
      to: [oncall-hook]
  default: [slack]
  templates:
    team:payments: "*{{.Event}}* {{.Title}} <{{.Link}}|open>"
```

The first route that takes an event decides, unless it says
`continue: true`; what none takes goes to `default`, which routes need, so
nothing is dropped. A route names a team as it is written, or in another
letter case where one team alone has the name; of two teams a ledger kept
from before whose names differ only in letter case, it takes the one it
names, never the other. A template is text and fields (`{{.Event}}`,
`{{.Line}}`, `{{.Link}}`, `{{.Rule}}`, `{{.Team}}`, `{{.Title}}`,
`{{.ID}}`, `{{.By}}`, `{{.Labels}}`, `{{.At}}`), each value escaped for
where it goes. The Settings page shows the routes in force and how each
destination's notices went; they are changed in the file
([design/notifications.md](design/notifications.md#who-is-told-what)).

## Signed posts to the webhook

Whoever learns the webhook's URL can post to it, and a receiver that turns
what comes into issues, pages or review items would take what they post.
With `--notify-webhook-secret-file FILE`, beside `--notify-webhook-file`,
every post to the webhook is signed, of every event (`task.*`,
`incident.written`, `digest.ready`, `report.ready` and the rest), in the
shape Grafana signs what it sends Lapilli in ([Notifications as they were
sent](notifications.md)), with headers of its own:

| Header | |
|---|---|
| `X-Lapilli-Timestamp` | when it was sent, in Unix seconds |
| `X-Lapilli-Signature` | `sha256=` and the HMAC-SHA256, with the key, of the timestamp, a `.` and the body, in lowercase hex |

The body is signed as the bytes sent. A post tried again is signed again,
at its own time, with the same body and `id` (and `Idempotency-Key`). The
key is what the file holds, less one line break at its end (`\n` or
`\r\n`), 32 bytes at least; `serve` refuses to start with one shorter, one
it can't read, or without `--notify-webhook-file`. It is read for each post,
so it can be rotated, and it and the signatures are never shown or logged.
A key that can't be read later fails the post, which is tried again: nothing
is sent unsigned. Without the flag, a post carries neither header, as
before. A webhook routes send to by a name of its own, `--webhook-dest
NAME=FILE`, is signed the same way with its own key,
`--webhook-dest-secret NAME=FILE`.

To check a post, read its body as it came, before parsing it:

1. Work out the HMAC-SHA256, with the key, of `X-Lapilli-Timestamp`, a `.`
   and the body, and compare `sha256=` and its hex with
   `X-Lapilli-Signature` in constant time (Python's `hmac.compare_digest`,
   Go's `hmac.Equal`, Node's `crypto.timingSafeEqual`), not with `==`.
2. Refuse a timestamp more than five minutes from your clock, either side,
   so that a post someone saw can't be sent again later.
3. Within those five minutes, keep the `id` of each post taken: the same one
   again is a post tried again, or sent again by someone else.

```python
import hashlib, hmac, time

def verify(key: bytes, timestamp: str, signature: str, body: bytes) -> bool:
    mac = hmac.new(key, timestamp.encode() + b"." + body, hashlib.sha256)
    return (timestamp.isdigit() and abs(time.time() - int(timestamp)) <= 300
            and hmac.compare_digest("sha256=" + mac.hexdigest(), signature))
```

To rotate the key, have the receiver take either while the file changes:
each post is signed with what the file holds as it is sent, and one the
receiver refuses is tried again. In the chart,
`serve.notify.webhookSecret.signingKey` names the key of the webhook's
Secret that holds it.
