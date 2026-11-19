# Metrics

`lapilli serve` gives Prometheus metrics at `/metrics`, in Prometheus's text
format. It answers there on `--listen`, beside the pages, and with a
sign-in (`--oidc-issuer`, `--proxy-jwt`) or `--viewers` only to those a
page lets in. `--metrics-listen ADDR` also serves `/metrics`,
`/healthz` and `/readyz` on a port of their own, and nothing else, with no
sign-in, so a NetworkPolicy can let Prometheus reach that port alone. In
the chart that is `serve.metricsPort` (9091), which the probes use;
`serve.serviceMonitor.enabled` makes a ServiceMonitor for it, and
`dashboard.enabled` a Grafana dashboard of these metrics. `lapilli serve`
pushes nothing: it takes no `--push`, and Prometheus scrapes it instead.

Two kinds of series are in it:

- **What the ledger holds**, worked out from it: the stones, incidents,
  goals and the rest. The rules' and the incidents' are over the last seven
  days (today, from midnight in `--tz`, and the six days before), as
  `lapilli verdict --metrics` would give them over that week; others say
  their own span below. Every replica of one ledger gives the same.
- **What the process counts**: its runs, its requests to the history, the
  notices it sent. Counters (`_total`) start at 0 when the process starts.
  On several replicas, what the leader does is counted in the leader's pod
  alone ([Several replicas](serve.md#several-replicas)).

A series is there only once there is something for it, as each table says:
a rule that didn't fire in the seven days has no `lapilli_rule_firings`.
Every metric is a gauge unless its type says counter.

The metrics [The numbers](numbers.md#the-metrics) lists keep their names,
labels and meaning from release to release: one renamed is given under both
names for at least one minor release first, and a label goes only with its
metric's name ([What stays the same](numbers.md#what-stays-the-same)). The
rest aren't held to that, and may change in a minor release, said in the
changelog: the `lapilli_serve_history_*` counters,
`lapilli_serve_runs_held`, `lapilli_serve_notices_total`,
`lapilli_serve_notices_unsent`, `lapilli_serve_notifications_refused_total`,
`lapilli_rule_replay_check`,
`lapilli_serve_replay_checked_to_timestamp_seconds`,
`lapilli_rule_content_problems`, and the replicas' five.

## Rules

Each rule's series are labelled `rule`, its name; `severity`, when the rule
has one; and `team`, when a team owns it (by a note, a name pattern or
labels: [Keep judging](serve.md)). What a white and a black stone are, and
the rest of the words, is in [The numbers](numbers.md#a-white-or-a-black-stone).

| Metric | Labels | What it counts |
|---|---|---|
| `lapilli_rule_firings` | `rule`, `severity`, `team` | Times the rule fired in the seven days, less the firings a quiet window excused |
| `lapilli_rule_caught` | `rule`, `severity`, `team` | Of them, firings that caught an incident (white stones) |
| `lapilli_rule_nothing` | `rule`, `severity`, `team` | Firings with no incident (black stones); those with [no record](numbers.md#no-record) among them |
| `lapilli_rule_refired` | `rule`, `severity`, `team` | Firings that started within `--refire` (1h) of the same alert resolving |
| `lapilli_rule_incidents` | `rule`, `severity`, `team` | Incidents the rule caught |
| `lapilli_rule_incidents_first` | `rule`, `severity`, `team` | Incidents the rule caught before any other rule; a noisy rule's catch doesn't count beside another's |
| `lapilli_rule_incidents_alone` | `rule`, `severity`, `team` | Incidents no other rule caught, counted the same way |
| `lapilli_rule_episodes` | `rule`, `severity`, `team` | The rule's firings counted as [episodes](numbers.md#episodes): one problem, its alerts of the same match labels overlapping or re-firing within `--refire`, a day at most; none while the ledger hasn't counted the seven days' episodes yet |
| `lapilli_rule_episodes_caught` | `rule`, `severity`, `team` | Of them, episodes with a firing that caught an incident |
| `lapilli_rule_nothing_by_hours` | `rule`, `severity`, `team`, `hours` | Black stones by when they came: `asleep`, `off_hours` or `at_work`, by `--work-hours`, `--sleep-hours`, `--tz` and `--shifts`, or the hours of the team that owns the rule |
| `lapilli_rule_refired_weeks` | `rule`, `severity`, `team` | Whole weeks, of `lapilli_weeks_counted`, in which a firing of the rule re-fired; rules with one at least |
| `lapilli_rule_notifications` | `rule`, `receiver`, `reason` | Notifications sent for the rule in the seven days, as the alert manager sent them to `--notify-listen`, by receiver and why: `first`, `new`, `resolved`, `repeat` or `unknown` |
| `lapilli_rule_replay_check` | `rule`, `way` | The replay beside what really fired over the seven whole days before today: firings `alike`, only in the replay (`replayed`), and only in what really fired (`real`) |
| `lapilli_rule_content_problems` | `rule`, `check` | Problems with what the rule says when it fires, or how it is written, by check: facts and what was measured, not the guesses |

`lapilli_rule_incidents_first` and `lapilli_rule_incidents_alone` are left
out when only some of the rules are judged, as what a rule adds to the
others can't be said then. `lapilli_rule_notifications` has only the
ledger's rules, and a rule's ten busiest receivers, the rest as
`receiver="other"` ([Notifications as they were sent](notifications.md)).
`lapilli_rule_replay_check` is there with the check on (`--check-replay`,
on unless `--check-replay=false`) once a day of the seven is checked; rules
that share an alert name, which what really fired doesn't tell apart, each
carry the counts of them all ([Is the replay right?](check-replay.md)).
`lapilli_rule_content_problems` has a series for each rule and check with a
problem ([What an alert says](check-content.md)).

## Incidents

| Metric | Labels | What it counts |
|---|---|---|
| `lapilli_incidents` | `state`, `source` | Incidents in the seven days, by `state`: `caught`, `noisy_only` (caught only by noisy rules) or `missed`; and `source`: `written` (written down) or `impact` (a window `--impact` found). All six series, 0 among them |
| `lapilli_incident_detection_delay_seconds` | `incident` | From each caught incident's start to the first alert of a rule that isn't noisy; negative when the alert came first. None for an incident missed or caught only by noisy rules |
| `lapilli_incident_state` | `incident`, `source`, `signal`, `state` | 1 for each incident of the seven days, in its state, by the rules' stones in its week. `signal` is on a named signal's windows |
| `lapilli_incident_lead_seconds` | `incident`, `source`, `signal` | How long before each caught incident's start the first alert of a rule that isn't noisy came, negative when after: the delay the other way round ([Lead](numbers.md#lead)) |
| `lapilli_incident_team` | `incident`, `team` | 1 for each team that owns an incident of `lapilli_incident_state` (one of its rules caught it, or it has a value of a label the team owns by), and one series without `team` for an incident no team owns |
| `lapilli_impact_windows` | `signal`, `state` | With named impact signals: each signal's windows in the seven days, by `state` as above |
| `lapilli_impact_signal_start_timestamp_seconds` | `signal` | With named impact signals: from when each keeps the record of the firings it could have seen, in Unix seconds. None for a signal that keeps no record, or whose data a selector has none of |

`lapilli_incident_state` and `lapilli_incident_lead_seconds` have the
latest 500 incidents at most, and `lapilli_incident_team` 500 series at
most, the latest incidents first. A dashboard picks one team's incidents
by joining `lapilli_incident_state` with `lapilli_incident_team` on
`incident`. Named signals are in
[Incidents nobody wrote down](impact.md) and
[design/signals.md](design/signals.md).

## Re-fires, deploys and marked windows

| Metric | Labels | What it counts |
|---|---|---|
| `lapilli_weeks_counted` | | Whole weeks, of the twelve before this one, the ledger has all of (not the one its first firing fell in): what the two below count of |
| `lapilli_refired_weeks` | `team` | Of those weeks, the ones in which a firing re-fired: everyone's without `team`, and a series for each team's rules |
| `lapilli_rule_firings_during_deploys` | `rule`, `severity`, `team` | Firings judged that came during a deploy of what they are about ([Firings during deploys](numbers.md#firings-during-deploys-lapilli-serve)); in `lapilli_rule_firings` too |
| `lapilli_rule_caught_during_deploys` | `rule`, `severity`, `team` | Of those, the ones that caught an incident; in `lapilli_rule_caught` too |
| `lapilli_serve_deploys_last_timestamp_seconds` | | When the latest deploy the ledger holds started, of those that have started, in Unix seconds |
| `lapilli_serve_deploys_read_timestamp_seconds` | | When the deploys file (`--deploys`) was last read, and what it held kept, in Unix seconds |
| `lapilli_rule_firings_marked` | `rule`, `severity`, `team`, `kind` | Firings judged that came during a marked window of the kind (a sale, a launch); in `lapilli_rule_firings` too. A series for each rule and kind with firings |
| `lapilli_rule_caught_marked` | `rule`, `severity`, `team`, `kind` | Of those, the ones that caught an incident; in `lapilli_rule_caught` too |
| `lapilli_marked_on` | `kind` | 1 while an occurrence of a marked window of the kind is on, 0 otherwise: each kind the ledger keeps, for a dashboard to shade by |
| `lapilli_serve_marked_read_timestamp_seconds` | | When the marked file (`--marked`) was last read, and what it held kept, in Unix seconds |

The deploys' series are there once the ledger holds a deploy, and
`lapilli_serve_deploys_read_timestamp_seconds` with `--deploys` once the
file is read. `lapilli_rule_firings_marked` and
`lapilli_rule_caught_marked` are there once a rule has firings marked in
the seven days, `lapilli_marked_on` once the ledger keeps a kind, and
`lapilli_serve_marked_read_timestamp_seconds` with `--marked` once the
file is read. Deploys are no kind of marked window:
`lapilli_rule_firings_during_deploys` says theirs. See
[design/noc.md](design/noc.md) and [design/marked.md](design/marked.md).

## Goals

The goals `--config` declares under `goals:`
([design/goals.md](design/goals.md)), each of its last whole week, from
Monday in `--tz`, as the leader worked them out after its last round:
everyone's without `team`, a team's with it. 500 series of each at most,
the goals missed first.

| Metric | Labels | What it counts |
|---|---|---|
| `lapilli_goal_known` | `goal`, `team` | 1 when the week is known, met or missed; 0 when it isn't (no record for a goal that needs one, a share with nothing to divide, a week the ledger doesn't hold whole) |
| `lapilli_goal_met` | `goal`, `team` | 1 when the goal was met, 0 when it was missed; no series when the week isn't known |
| `lapilli_goal_value` | `goal`, `team` | The goal's measure over the week, when it is known: a share from 0 to 1, a count, or seconds |
| `lapilli_goal_weeks_met` | `goal`, `team` | Whole weeks, of the twelve before this one, in which the goal was met: beside `lapilli_weeks_counted` |

A goal over a period (`each: KIND`) is of its latest occurrence that ended,
and `lapilli_goal_weeks_met` counts its occurrences met that ended in those
twelve weeks, so it can be over `lapilli_weeks_counted`
([The numbers](numbers.md#the-metrics)).

## What Lapilli came to

The Outcomes page's numbers ([Keep judging](serve.md)), from the ledger.

| Metric | Labels | What it counts |
|---|---|---|
| `lapilli_serve_firings_avoided` | | How many fewer times the changed rules fired since each reviewed change than at their rates before it |
| `lapilli_serve_nothing_avoided` | | How many fewer black stones the changed rules made since each reviewed change than at their rates before it |
| `lapilli_serve_rule_changes` | `outcome` | Reviewed changes to rules, by how they came out against the prediction: `as_predicted`, `otherwise`, or `measuring` while they are still measured |
| `lapilli_serve_incident_last_timestamp_seconds` | | When the latest incident written down started (from the incidents file, the UI or the API, impact windows confirmed among them), in Unix seconds; 0 for none |
| `lapilli_serve_edits` | | Edits made in the UI or the API in the last four weeks |

## Runs and the history

| Metric | Type | Labels | What it counts |
|---|---|---|---|
| `lapilli_serve_runs_total` | counter | `kind`, `result` | Runs since the process started, by `kind`, `hourly` or `backfill` (one whose period ends before now), and `result`, `ok`, `failed` or `held` |
| `lapilli_serve_run_duration_seconds` | gauge | | How long the last run that ended `ok` took; 0 before one |
| `lapilli_serve_cursor_timestamp_seconds` | gauge | | How far the ledger has firings up to, in Unix seconds; there once a run is recorded |
| `lapilli_last_run_timestamp_seconds` | gauge | | When the latest entry of the run history finished, in Unix seconds: a run (held and failed ones among them), a review of a change to the rules, or a digest |
| `lapilli_serve_replay_checked_to_timestamp_seconds` | gauge | | How far the replay has been set beside what really fired, in Unix seconds; there with the check on (`--check-replay`), once a day is checked |
| `lapilli_serve_runs_held` | gauge | `reason` | 1 for why this round's run waited for a later one, `catch_up_hours`, `guard` or `yield`, and 0 for the others; all 0 when it didn't wait |
| `lapilli_serve_history_requests_total` | counter | `api`, `answer` | Requests sent to each history API, by the class of the answer: `2xx`, `3xx`, `4xx`, `5xx`, or `none` (refused, dropped, timed out) |
| `lapilli_serve_history_throttled_total` | counter | `api` | Answers of HTTP 429 from each history API |
| `lapilli_serve_history_retries_total` | counter | `api` | Requests asked again after an answer that said to (HTTP 429, 5xx) or a connection dropped |
| `lapilli_serve_history_wait_seconds_total` | counter | `api`, `reason` | Seconds requests waited before they were sent, summed: `api` for a wait the API asked for (`Retry-After`) and before asking again, `rate` for a turn by `--max-rate` or `--catch-up-rate` |
| `lapilli_serve_history_yields_total` | counter | `api` | Times the runs yielded to the API, as it answered HTTP 429 (`--yield-on-429`) |
| `lapilli_serve_incidents_passed_over` | gauge | | Incidents in the incidents file whose ids the ledger holds from people, and so not read from it |

The `lapilli_serve_history_*` counters are there once there is a history to
ask, not with firings copied in alone, with `api` `prometheus`, `loki`
with `--loki`, and `grafana` with `--grafana` (its data source proxy, for
its rules on Loki). They count what this process asked since it started,
the leader's work and what people and agents asked for on it, to set
beside the evaluator's own errors ([design/quota.md](design/quota.md)).
`lapilli_serve_runs_held` is there with `--yield-on-429`, `--guard` or
`--catch-up-hours`, and `lapilli_serve_incidents_passed_over` with
`--incidents`, on the replica that read the file.

## Pull request backtests

With `--backtest-listen` alone:

| Metric | Type | Labels | What it counts |
|---|---|---|---|
| `lapilli_serve_backtests_total` | counter | `trust`, `result` | Pull request backtests since the process started, by entry of `--backtest-trust` and how they ended: `done`, `failed`, `cancelled`, `superseded`, or `refused` before they were queued |
| `lapilli_serve_backtests_waiting` | gauge | `trust` | Pull request backtests waiting, by entry, as the ledger keeps them |

`trust` is an entry's name, which whoever runs serve wrote: never a
repository, a branch or a rule. What the backtests ask of the history is
in the `lapilli_serve_history_*` counters too.

## Firings copied in

With `--firings-token-file`, or once firings have been copied in from a
source ([Firings recorded elsewhere](serve.md#firings-recorded-elsewhere)):

| Metric | Labels | What it counts |
|---|---|---|
| `lapilli_serve_source_last_timestamp_seconds` | `source` | When firings were last copied in from each source, in Unix seconds |
| `lapilli_serve_source_firings` | `source` | Firings in the period last copied in from each source |
| `lapilli_serve_source_quiet_seconds` | | How long a source may go without firings copied in before it is told of (`--source-quiet`, 26h unless given) |

## Notifications and notices

| Metric | Type | Labels | What it counts |
|---|---|---|---|
| `lapilli_serve_notifications_since_timestamp_seconds` | gauge | | When the first notification kept came, in Unix seconds: `lapilli_rule_notifications` counts from then |
| `lapilli_serve_notifications_last_timestamp_seconds` | gauge | `source` | When the latest notification taken came, by where it came from, in Unix seconds |
| `lapilli_serve_notifications_refused_total` | counter | `source` | Notifications the notifications port was sent and couldn't keep, since the process started; the rest of a request is kept, and why is logged |
| `lapilli_serve_notices_total` | counter | `to`, `result` | Notices since the process started, by where they went (`webhook`, `slack`, `team`, `report`) and whether they were `sent` or `failed` |
| `lapilli_serve_notices_unsent` | gauge | | Notices not sent yet, or given up on |

The first two are there once a notification is kept.
`lapilli_serve_notifications_refused_total` is there with `--notify-listen`,
a series at 0 for each of the port's sources: `alertmanager`,
`alertmanager-events`, `grafana`, `sns`, `azure`, `google` and `oci`
([Notifications as they were sent](notifications.md)). The notices' two are
there when notices have somewhere to go: `--notify-webhook-file`,
`--slack-webhook-file`, `--team-digest` or `--report-dest`.

## Replicas

On a Postgres ledger, which replicas share
([Several replicas](serve.md#several-replicas),
[design/replicas.md](design/replicas.md)):

| Metric | Labels | What it counts |
|---|---|---|
| `lapilli_serve_leader` | | 1 in the pod of the replica that leads, running what is run once, 0 in the others' |
| `lapilli_serve_replicas` | | The replicas sharing the ledger that are live, this one among them, as last counted |
| `lapilli_serve_session_keys` | | With a sign-in of lapilli's own (`--oidc-issuer`): how many keys the live replicas sign sessions with, more than one while a rollout brings in a new key |
| `lapilli_serve_ledger_writes_waiting` | | 1 when this replica's last write gave up waiting for the ledger's writers' turn, which another session holds, and none has taken it since; 0 otherwise |
| `lapilli_serve_jobs_round_timestamp_seconds` | | In the leader's pod: when it last finished a round of its jobs, in Unix seconds |

## From `lapilli verdict`

`lapilli verdict --metrics` writes the report as metrics instead of text,
for node_exporter's textfile collector. `--push URL` also puts them on a
Pushgateway, with a PUT that replaces what the last run put in the same
group: `job="lapilli"`, or the group a URL ending `/metrics/job/...` names,
as it should when more than one lapilli pushes there. The chart's weekly
CronJob ([Every week](every-week.md)) pushes to `report.pushgateway`, in
the group `job="lapilli"`, `instance="NAMESPACE-RELEASE"`. They are over
the report's period, the week before for the chart, and all gauges:

| Metric | Labels | What it counts |
|---|---|---|
| `lapilli_rule_firings` | `rule`, `severity` | Times the rule fired in the period |
| `lapilli_rule_caught` | `rule`, `severity` | Firings that caught an incident (white stones) |
| `lapilli_rule_nothing` | `rule`, `severity` | Firings with no incident (black stones) |
| `lapilli_rule_refired` | `rule`, `severity` | Firings that came soon after the same alert resolved |
| `lapilli_rule_incidents` | `rule`, `severity` | Incidents the rule caught |
| `lapilli_rule_incidents_first` | `rule`, `severity` | Incidents the rule caught before any other rule |
| `lapilli_rule_incidents_alone` | `rule`, `severity` | Incidents no other rule caught |
| `lapilli_rule_nothing_by_hours` | `rule`, `severity`, `hours` | Black stones by when they came: `asleep`, `off_hours`, `at_work` |
| `lapilli_rule_episodes` | `rule`, `severity` | The rule's firings counted as [episodes](numbers.md#episodes) |
| `lapilli_rule_episodes_caught` | `rule`, `severity` | Of them, episodes with a firing that caught an incident |
| `lapilli_incidents` | `state`, `source` | Incidents by state and source, as `lapilli serve` gives them |
| `lapilli_incident_detection_delay_seconds` | `incident` | From each caught incident's start to the first alert of a rule that isn't noisy |
| `lapilli_last_run_timestamp_seconds` | | When this run wrote them, in Unix seconds |
| `lapilli_impact_windows` | `signal`, `state` | With named impact signals: each signal's windows by state |
| `lapilli_impact_signal_start_timestamp_seconds` | `signal` | With named impact signals: from when each keeps the record |

They mean what the same names mean in `lapilli serve`'s `/metrics`, above,
over the period instead of the seven days, without `team`: only
`lapilli serve` knows teams. A rule that can't be replayed has no series.
`--rule` leaves out `lapilli_rule_incidents_first` and
`lapilli_rule_incidents_alone`, and `--redact` gives the names in them
aliases, as in the text ([Showing a report outside](redact.md)).

## Alerts on Lapilli itself

A few rules a team's own Prometheus can keep on `lapilli serve`. The
`max` and `sum` take the pods of several replicas as one.

```yaml
groups:
  - name: lapilli
    rules:
      # No incident written down for two weeks, once the ledger holds two
      # whole weeks: the gauge is 0 before the first incident.
      - alert: LapilliRecordQuiet
        expr: >-
          time() - max(lapilli_serve_incident_last_timestamp_seconds) > 14 * 86400
          and on() max(lapilli_weeks_counted) >= 2
      # A job stopped copying firings in from a source, past --source-quiet.
      - alert: LapilliSourceQuiet
        expr: >-
          time() - max by (source) (lapilli_serve_source_last_timestamp_seconds)
          > on() group_left max(lapilli_serve_source_quiet_seconds)
      # A goal missed last week.
      - alert: LapilliGoalMissed
        expr: max by (goal, team) (lapilli_goal_met) == 0
      # The history answered HTTP 429 in the last hour.
      - alert: LapilliThrottled
        expr: sum by (api) (rate(lapilli_serve_history_throttled_total[1h])) > 0
```
