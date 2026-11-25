# Words

What each word means in these pages and in the UI of `lapilli serve`, as
the code counts it, with a link to where it is explained.

### Actionable

The share of a rule's firings known to be useful or not that were useful:
`useful / (useful + not_useful)`, `–` with none known. It is beside
precision, with `--responses`, `--rollouts` or `--deploys`. See
[The numbers](numbers.md#what-people-did).

### Approval

What a person gives a proposal, on its page, in a browser, before the
change is made: `--approvers` names who may, the admins when it isn't
given, and no one approves their own proposal or one an agent made for
them. The API, MCP and tokens can't approve. See
[Changing rules](changing-rules.md).

### Asleep, off hours, at work

When a firing that caught nothing started, in `--tz`: while people slept
(`--sleep-hours`, 22:00-08:00), off hours, or at work (`--work-hours`,
mon-fri@08:00-18:00). Each minute is one or the other. The rules that
woke people for nothing come first. See
[How it judges](how-it-judges.md).

### Backfill

What `lapilli serve` replays on its first start: the last `--backfill`
(28d). See [Keep judging](serve.md).

### Backtest

A replay of a change to the rules, beside the rules as they are, over the
same period and against the same incidents: `--compare`, the comment on a
pull request, and the evidence of each proposal, over the twelve weeks to
now. See [Before a change is merged](compare.md) and
[Pull request backtests in CI](pr-backtests.md).

### Black stone

A firing that caught no incident: nothing was wrong, as far as the record
says. One with no record to judge it by is counted among the black stones
(● NOTHING) but not judged. See
[The numbers](numbers.md#a-white-or-a-black-stone).

### Caught

A firing catches an incident when it starts from `--lead` (15m) before the
incident to its end, and agrees with it on each `--match` label both have.
One that lacks a label the incident has catches it, marked `(no service)`,
unless `--match-strict`. A firing already on before that caught nothing: it was on anyway. See
[The numbers](numbers.md#a-white-or-a-black-stone).

### Caught only by noisy rules

An incident every rule that caught was noisy: little better than missed.
It has no lead. See
[The numbers](numbers.md#incidents-missed-and-caught-only-by-noisy-rules).

### Digest

The report `lapilli serve` posts to Slack once a week, by default on
Mondays at 09:00 (`--digest`): the week before, judged, with longer `for`s
for the noisy rules and the rules that nearly caught what nothing caught.
`--team-digest` posts a team its own. See [Keep judging](serve.md).

### Disposition

What people mean to do with a rule, kept in its note: keep, tune or
retire. The rules list shows it as a badge. See [Keep judging](serve.md).

### Excused

A firing that starts in a quiet window, agrees with it and catches
nothing: neither white nor black, it weighs on no rule's precision. One
that caught an incident still counts. See [Keep judging](serve.md).

### Firing

One alert: one rule and one label set, from when it fired to when it
resolved. Firings come from a replay of the rules, from Prometheus's
`ALERTS` series (`--alerts`), or recorded elsewhere (`--firings`, a cloud's
alarm history). See [How it judges](how-it-judges.md).

### Goal

A bound on one of the summary's numbers, for everyone, a team or each
team, declared in `--config` under `goals:`. Each whole week is met,
missed or not known, never met for a week nobody wrote anything down. See
[Keep judging](serve.md) and [design/goals.md](design/goals.md).

### Impact window

A window in which a signal of impact returned something for at least
`--impact-for` (5m): an incident nobody wrote down, with an id
`impact-1`, `impact-2` and so on, or a named signal's `impact-NAME-1`,
`impact-NAME-2`. One that overlaps an incident written
down, agreeing on its `--match` labels, is that incident. In `lapilli
serve` a person confirms it, which writes it down, or dismisses it. See
[The numbers](numbers.md#impact-windows).

### Incident

Something that went wrong: an id, a start, and an end, a title and labels
if known. Written down (the incidents file, the UI, the API, copied in from
another tool) or found by a signal of impact. One with no end is taken to
last an hour. See
[Getting started](getting-started.md#write-down-an-incident-or-two).

### Judged

A firing is judged when the record was kept at its time: an incident was
on at some time in the two weeks before it, a signal of impact was read
over its time, or it caught one. Any other is counted, not judged: it is in
no precision and makes no rule noisy. The UI marks it "? Not judged".
`lapilli serve` judges by week. See [The numbers](numbers.md#no-record).

### Lead

How long before an incident's start the first alert of a rule that isn't
noisy came; negative when it came after. `--lead` (15m) is something else:
how long before an incident a firing can start and still catch it. See
[The numbers](numbers.md#lead).

### Ledger

What `lapilli serve` keeps: each firing and its stones, each rule's by day
and by week, the versions of the rules, and what people write (incidents,
tasks, notes, comments, proposals). One SQLite file in `--ledger DIR`, or
a Postgres database. See [Keep judging](serve.md).

### Linked

A catch by one of the alerts the incident's tool linked to it (the
incident's `alerts`): it catches in the same span, whatever `--match` says.
With `--linked-only`, an incident that lists alerts is caught by those
alone. See [How it judges](how-it-judges.md).

### Marked window

A sale, a launch, a campaign: a window of a kind a team chooses. A firing
that starts during one and agrees with it is marked with its kind, and
every count is read inside the kind and outside it. Nothing is excused.
See [Keep judging](serve.md) and [design/marked.md](design/marked.md).

### Missed

An incident no firing caught (`✗ nothing fired`). One whose alert came
only once it was over is missed too, and the report names the rule
(`✗ nothing in time; after its end`). See
[The numbers](numbers.md#incidents-missed-and-caught-only-by-noisy-rules).

### Mute

An alert a silence or an inhibition kept from a notification, as
Alertmanager's event recorder tells of it: why a firing paged no one. See
[Why a firing paged no one](notifications.md#why-a-firing-paged-no-one).

### Near miss

For an incident nothing caught, or only noisy rules did, a rule whose
condition held but not for its `for`, or only long enough to fire after
the incident was over, or whose value came within a fifth of its
threshold. The report gives the `for` or the threshold that would have
caught it. See [How it judges](how-it-judges.md).

### No record

A rule none of whose firings is judged has no record: its precision is
`–`, and the rules list gives it the badge "? No record" (also when as
many of its firings aren't judged as a noisy rule fires). A report none of
whose firings is judged says so once. See
[The numbers](numbers.md#no-record).

### Noisy rule

A rule with 10 judged firings or more, under 20% of them catching anything
(`--noisy-firings`, `--noisy-precision`): it fires whatever happens, so
its catches are likely chance. They are marked `(noisy)`, not dropped. With
`--noisy-by useful`, the share is of the firings that were useful. See
[The numbers](numbers.md#noisy).

### Notification

What the alert manager sent for firings, kept by `lapilli serve` where the
alert manager also sends them to `--notify-listen`: how many each rule
sent, how many were repeats, came while people slept, or were for firings
that caught nothing. See [Notifications as they were sent](notifications.md).

### Outcomes

Whether Lapilli earns its keep, on the Outcomes page (`/outcomes`): each
change to the rules it reviewed, its replay of the week before set beside
the four weeks either side; whether the record is kept up; and whether it
is used. See [Keep judging](serve.md).

### Period

The span a report judges. `lapilli verdict`: the last `--since` (7d), or
`--from` to `--to`, holding every firing on at some time in it and the
incidents that started in it. `lapilli serve` counts weeks from Monday, in
`--tz`. See
[The numbers](numbers.md#which-firings-and-incidents-a-report-holds).

### Precision

The share of a rule's judged firings that caught an incident:
`white / (firings - unjudged)`, `–` with none judged. Of a week or of
everyone, the same sum over all their firings. See
[The numbers](numbers.md#precision).

### Proposal

A change to one rule, waiting for approval: a longer `for` for a noisy
rule, the threshold or `for` that would have caught a near miss, retiring
a spare rule or one that never fired in twelve weeks, or a fix to what it
says. A change to when it fires carries its backtest. A rule has one
waiting at a time. See
[Changing rules](changing-rules.md).

### Quiet window

A deploy, maintenance, a load test: a window whose firings that catch
nothing are excused, so expected noise doesn't weigh on a rule's
precision. It can repeat, by the day, week or month or a cron expression,
and be a team's, excusing only its rules' firings. See
[Keep judging](serve.md).

### Re-fired

A firing that started within `--refire` (1h) of the same alert, the same
rule and labels, last resolving. See [The numbers](numbers.md#re-fired).

### Replay

Evaluating alert rules against the history, as their evaluator would
have, with their `for`, `interval` and `query_offset`, to find when they
would have fired: new rules too, and history from before Lapilli. A replay
of a change is a backtest. See [How it judges](how-it-judges.md).

### Response

What was done about a firing, from the pages a paging tool sent, the
rollouts, rollbacks and scales of the alert's workload, and the deploys:
changed, acknowledged, unanswered, routine or none. See
[What people did when a rule fired](responses.md).

### Shifts and on call

Hours that take turns, for a team on call in shifts or across time zones:
`--shifts` gives each turn's hours, and `--on-call FILE` reads the turns
from on-call schedules' iCalendar feeds, so a firing is counted by the
hours of whoever's turn it came in. See [How it judges](how-it-judges.md).

### Signal of impact

A PromQL expression that returns something while users are hurt, such as a
5xx ratio over 1% (`--impact`). Its windows become incidents, and reading
it keeps the record from where its data begins. Several can be named, each
with its own `for` and labels. See
[Incidents nobody wrote down](impact.md).

### Silent

A rule with no firing in the weeks shown: the rules list's badge.
`lapilli verdict` counts them under its tables (`rules never fired`). See
[Keep judging](serve.md).

### Spare

A rule that caught two incidents or more, each of which a rule that isn't
noisy, and can still fire, caught as soon or sooner, at the same severity
or above. It is a question to ask, not a verdict. See
[How it judges](how-it-judges.md).

### Stand-in

Of a signal of impact, the series that say from when a selector that only
narrows another keeps the record: the wider one's series with each label
it narrows by, any value (`http_requests_total{code!=""}` for
`http_requests_total{code=~"5.."}` beside `http_requests_total`). A
service that served no 5xx had nothing to match, not nothing to read. See
[The numbers](numbers.md#no-record) and
[design/signals.md](design/signals.md). Of tests, a stand-in is a fake of
an API, answering as its documentation shows.

### Stone

What each firing gets: white (○) when it caught an incident, black (●)
when nothing was wrong, as Roman juries voted with white and black
pebbles. See [The name](../README.md#the-name).

### Task

Who does what by when: a team, an assignee, a due date, a status, and a
link to an issue in a tracker. A suggestion on the dashboard becomes one
with a press. See [Keep judging](serve.md) and
[Tasks kept in step with a tracker](serve.md#tasks-kept-in-step-with-a-tracker).

### Team

Owns rules, by a note, a name pattern or labels, in that order, and
incidents, so the dashboard, the rules and the tasks can be shown for one
team. A team can keep its own hours, time zone and shifts, and its own
editors. See [Keep judging](serve.md) and [design/work.md](design/work.md).

### Useful firing

A firing that caught an incident, or that a change on the alert's target
followed. One that a page nobody answered came of, or that someone looked at
and nothing changed, is not useful. See
[What people did when a rule fired](responses.md).

### Version of the rules

The rules as they stood from a time on. `lapilli serve` records each
change it reads as a new version, so each hour is replayed with the rules
of then; `lapilli verdict` takes `--rules FILE@TIME` and `--rules-history
DIR`. See [How it judges](how-it-judges.md).

### White stone

A firing that caught an incident. See
[The numbers](numbers.md#a-white-or-a-black-stone).
