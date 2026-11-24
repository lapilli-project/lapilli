# How it judges

Each number below is defined as the code counts it, with how to count it
again yourself from the report, in [docs/numbers.md](numbers.md).

**Firings** come from one of three places:

- `--rules FILE`: replay alert rules against history, so it works on rules you
  just wrote and on history from before you had Lapilli. It reads Prometheus
  rule files, `PrometheusRule` resources, Grafana alerting provisioning files,
  each cloud's managed rule groups and its alarms written in PromQL (see
  below), ConfigMaps holding any of them, and directories of them, below them too; below the top, files that
  don't parse, such as a Helm chart's templates, are passed over. Grafana rules are evaluated the way
  Grafana evaluates them: each query fetched as PromQL, then reduce, threshold
  and math, where a comparison gives 1 or 0 and two sets of series pair up by
  their labels; classic conditions too, every reducer and evaluator, as
  Grafana's own code has them. What Grafana does when a query returns nothing
  (`noDataState`) and when one series of several goes is replayed with it,
  in Grafana's own states; query errors can't be
  ([docs/design/states.md](design/states.md)). `for`,
  `keep_firing_for`, a group's `interval` and `query_offset`, and a Grafana
  query's window that ends before now are honoured. `--rules FILE@TIME` gives
  the rules as they stood from a time on; see "Rules that changed" below.
- `--loaded-rules`: replay the alert rules the API at `--prometheus` has
  loaded, as `GET /api/v1/rules` lists them: Prometheus, Thanos, Mimir,
  vmalert, Amazon Managed Service for Prometheus. Each group is evaluated at
  the second of its interval it last ran at, as the API says, where
  Prometheus placed it.
- `--grafana URL`: replay the alert rules a Grafana holds, made in its UI
  or not, read through its provisioning export API with the service account
  token in `--grafana-token-file`; and the scrape interval each data source
  says it has (`jsonData.timeInterval`), which Grafana puts
  `$__rate_interval` in with. A Viewer's token reads both.
- `--ruler URL`: replay the alert rules a managed Prometheus ruler holds,
  as written (the expression as it was written, not printed again), read
  through the API that writes them: a Mimir's or a Cortex's configuration
  API (`http://mimir:8080/prometheus/config/v1/rules`,
  `http://cortex:9009/api/v1/rules`), with a token in `--ruler-token-file`
  and, for one with tenants and no gateway, `--ruler-tenant`, which
  `--prometheus` is sent too unless `--prometheus-tenant` gives the
  history a tenant of its own; Amazon Managed
  Service for Prometheus's control plane
  (`https://aps.REGION.amazonaws.com/workspaces/ID`, with `--auth aws`); or
  Azure's Prometheus rule groups in a subscription or a resource group
  (`https://management.azure.com/subscriptions/SUB`, with `--auth azure`).
  Rules of one name in two namespaces are told apart by both:
  `ScrapeSlow [payments/shop]`, a `/` within either written `%2F`
  (`ScrapeSlow [alloy%2Fpayments/shop]`). A group the ruler gives in a form
  that can't be read (Mimir 3.2.4 writes an expression whose first line
  starts with spaces so) is passed over and logged, and the rest read.
- `--alarms SOURCE=URL`: also read a cloud's own alarms through its API,
  beside any of the above or alone (repeatable): CloudWatch's
  (`https://monitoring.REGION.amazonaws.com`), Azure Monitor's alert rules
  in a subscription or resource group
  (`https://management.azure.com/subscriptions/SUB`), a Google Cloud
  project's alerting policies
  (`https://monitoring.googleapis.com/v3/projects/PROJECT/alertPolicies`),
  OCI Monitoring's alarms in a compartment
  (`https://telemetry.REGION.oraclecloud.com/20180401/alarms?compartmentId=OCID`).
  Each is read with its cloud's credentials, whatever `--auth` says.
  `SOURCE` names them, as their history is copied in; an alarm and a rule of
  one name are told apart by it: `HighCPU [cloudwatch-prod]`. PromQL
  alarms are replayed, the others listed (see
  [A cloud's own alarms](cloud-alarms.md)).
  `lapilli serve` reads them every `--alarms-every` (10m).
- `--alerts`: the `ALERTS` series Prometheus keeps for its own rules.
- `--firings FILE`: firings recorded elsewhere, one JSON object a line
  (`{"rule":"...","labels":{...},"start":"...","end":"..."}`), such as the
  history of a cloud's own alarms (see
  [A cloud's own alarms](cloud-alarms.md)), or a
  report's own `firings`, judged again: their `held_from` says when their
  condition held (see "An alert after the incident").

**Incidents** come from a file, from a signal of user impact, or both:

```yaml
incidents:
  - id: INC-41
    title: Checkout errors
    start: 2026-10-28T12:01:00+09:00
    end: 2026-10-28T12:40:00+09:00   # optional; an hour when missing
    labels: {service: checkout}      # optional, for --match; [a, b] for several
    alerts:                          # optional: those its tool linked to it
      - {rule: CheckoutErrors, labels: {service: checkout}}
```

`--impact PROMQL` turns every window where the expression returns something,
for at least `--impact-for` (5m), into an incident too. An error-rate SLI works
well: the incidents nobody wrote down are often the ones no alert caught. Each
series is looked at on its own and its windows carry its labels, so with
`sum by (service) (...)` an incident says which services were hit; windows
that overlap are one incident listing all of them. In `lapilli serve` an
admin can also set `--impact` and `--impact-for` in the UI, on the Settings
page's Incident criteria, and try them on the last four weeks first (see
[Keep judging](serve.md)). The report says how long
each incident lasted, and, of those nothing caught that were found over
several series, each series' own window (`spans` in `--json`): an incident
that lasted ten minutes may be two services' windows of five and ten.

A team rarely has one signal of user harm. `lapilli verdict` and `lapilli
serve` take several, each named, with its own `for`, the labels its windows
are matched on, and whether reading it keeps the record
(docs/design/signals.md):

```sh
lapilli verdict --rules alerts/ --prometheus http://prometheus:9090 --match service --match team \
  --impact 'errors=sum by (service, team) (rate(http_requests_total{code=~"5.."}[5m])) / sum by (service, team) (rate(http_requests_total[5m])) > 0.05' \
  --impact 'probe=probe_success == 0' --impact-for probe=2m --impact-match probe=service \
  --impact 'payments=sum(rate(payments_succeeded_total[5m])) == 0' --impact-for payments=1m --impact-record payments=no
```

A probe's window and a 5xx window at the same time are two incidents, each
matched on its own labels, not one that keeps only the labels both have.
Each signal keeps the record of the firings it could have seen, those of
the values it has seen over the read, from where the data it reads begins:
what its selectors' series have had (each counted by its match labels, one
request a selector, a 5xx selector through the requests with any code),
and what it returned. So a 5xx share that returns something only while
users are hurt still sees every service it measured, and a probe added
last Thursday holds back the firings of the services it probes alone; a
firing no signal could have seen waits for the latest start of all.
`--impact-file` says the same in a file, and which rules are judged
against which signals: the latency rules against a latency signal alone,
a load balancer's "no backend" rule matched on `backend` alone (see
`lapilli verdict -h`). Named and unnamed `--impact` don't go together. In
`lapilli serve`, the runs read each signal on its own, one that fails
missed and read from there by a later run; each keeps its own start in
the ledger, a firing is judged by the signals tied to its alert, and the
summary, `/metrics`, MCP and the Incidents page say each signal's windows
apart, and a rule's page what its record waits for. The chart's
`impact` takes the file's `signals` and `rules` as a mapping. The Settings
page lists them; changing them there comes in a later release.

Impact as short as `--impact-for` can be shorter than a rule fires on: a
rule with `for: 5m` evaluated every minute fires once its condition held for
six minutes, so a burst of errors under a minute, which `rate(...[5m])` shows
for exactly five, reads as an incident it missed. When some rules judged
can't fire on impact as short as `--impact-for` (their `for` and one
evaluation; with no `for`, as long as their evaluations are apart; and how
late their metrics arrive, by `--late`, a group's `query_offset` or a
Grafana query's time range), the
report says how many, and what `--impact-for` they can catch
(`impact_short` in `--json`); the incidents are judged as any others.

**A firing catches an incident** when it starts between `--lead` (15m) before
the incident and its end, and agrees with it on any `--match` labels both
carry: `--match service` keeps an alert about one service from catching an
incident in another just because they came at the same time. An incident that
lists several values agrees with any of them, and an alert without the label
(a database or a node) is not held to it. An alert that was already firing before that caught nothing: it was on
anyway. A firing that comes within `--refire` (1h) of the same alert resolving
is counted as refired.

**The alerts its tool linked.** An incident may list the alerts its tool
linked to it (`alerts`, which the PagerDuty, incident.io and Rootly recipes
write; [Incidents kept elsewhere](incidents.md)). A firing of one, its rule
the alert's (or that rule told apart from others of its name,
`HighErrorRate [severity=critical]`) and its labels agreeing with the
alert's on those both carry (one of the alert's it lacks, such as an
external label, passed over, as `--match` does),
catches the incident when it starts between `--lead` before it and its
end, whatever their labels say on `--match`: the link is better evidence
than the labels. The report marks the catch linked (`(linked)`, `linked`
in `--json`). Other firings still catch it by time and labels, unless
`--linked-only`: then an incident that lists alerts is caught by those
alone, and one that lists none as above. A firing it lists that came after
its end is its alert too late (below), labels or not. Which rules are
noisy and the record are worked out as ever; near misses are looked for by
the link too (below).

**Noisy rules.** A rule that fired 10 times or more in the period, with under
20% of its firings catching anything, fires whatever happens; when it lands on
an incident that is likely chance. Its catches are shown marked `(noisy)`, not
dropped, since a noisy rule can also fire for the right reason. An incident
caught only by noisy rules is reported as such, and its detection delay is
taken from the first rule that isn't noisy. `--noisy-firings` and
`--noisy-precision` move the line.

**No record, no verdict.** A firing that caught nothing caught nothing
written down. It is judged only when the record was kept at its time: an
incident (written down, or a window `--impact` found) was on at some time
in the two weeks before it, or it caught one. One with no end is taken to
last an hour, as long as it is caught for. Given `--impact`, the
signal is read from two weeks before the period, and keeps the record
from where the data of every selector it reads begins, whatever it finds:
it says whether users were hurt; the firings are judged against the
windows it finds in the period, as they were. A selector with no data
keeps none, and one whose data begins later holds the record back; but
one that only narrows another the expression reads is read whenever that
one is, so long as the label it narrows by is there
(`http_requests_total{code!=""}` for the 5xx of a service that served
none, beside all its requests). The report says from when it keeps the
record, when that is within the period.
Any other firing can't be told from one nobody wrote down: it is counted
(in FIRED and ● NOTHING), and not judged: it is in no precision, and makes
no rule noisy. A rule none of whose firings is judged has a precision of
`–`; a period none of whose firings is, says so once, and suggests no
longer `for`; one only some of whose are, says how many. `--json` has
`"no_record"` on the report and on each rule none of whose firings is
judged, and `"unjudged"` counts the rest. A rule replayed that fired as
often as a noisy rule with no record says instead what those firings were:
of a condition that counts events (below), each firing is an event it
counted, a count that went up; of another whose bounds decide it, how many
had the values its condition compares go well past the bounds that decided
them (by more than a fifth of each, for its `for`), not a flicker at an
edge: one bound, a range (`x > 1 and x < 5`), or comparisons joined by
`and`, `or` and `unless` (or, in Grafana, `&&` and `||`), not a value
beside them under `or`, a bound of nothing, nor a range narrower than two
fifths of its ends (docs/design/serve.md, "With no record: which
conditions"); and the alerts that fired most. Those happened; whether they mattered is for a
person to write down (`"unrecorded"` in `--json`). lapilli serve judges a
firing by its week instead: a week whose record is kept, an incident on in
it or in the two weeks before it, or `--impact` read over all of it, and,
with a team picked, by the incidents the team owns.

**What to change.** Each noisy rule replayed from `--rules` is replayed again
with a longer `for` (5m, 10m, 15m, 30m, 1h, 2h). The shortest that would have
caught every incident the rule caught, firing at most half as often, is
suggested. Rules with a `severity` label show it, and `--metrics` carries it.
A rule whose condition counts events is never suggested a `for` as long as
the range it counts them over: `increase`, `changes`, `resets` or `rate` of
a counter's range or a subquery of one, summed or maxed by some labels or
not, times or over numbers or not, compared with a number no event passes
and one event does (`increase(x[30m]) > 0.5`, `rate(x[10m]) * 300 > 0`).
`delta` is a gauge's; `idelta` and `irate` hold for about a scrape after an
event; and a counter of seconds or bytes, or a histogram's `_sum`, counts an
amount, not events: none of these is one. A Grafana rule is one when its
condition is a threshold of one number (`gt`, `gte`), or a math expression
that is one comparison of a ref with a number, of its one PromQL instant
query's value, as it is or through reduces that leave it so (`last`,
`max`, `min` or `mean` of its one value), its PromQL read as above; not a
range query, whose points one event can hold for as long again as its
range, nor one that fires on no data (`noDataState` `Alerting` or
`KeepLast`), and a firing on no data is no event. A rule written in LogQL
(a Loki ruler's, or a Grafana rule's one instant Loki query, read as
above) is one when its value is `count_over_time`, `rate`,
`bytes_over_time` or `bytes_rate` of a stream's lines over a range, the
range after the pipeline, summed or maxed or not, times or over numbers or
not, compared with a number no line passes and one does: each line is an
event, and a line of `bytes_over_time` one byte at least
(`count_over_time({app="shop"} |= "error" [5m]) > 0`). A pipeline that
unwraps a label counts its values, not lines: no event. After one restart, `increase(restarts[30m]) > 0.5` holds for
30 minutes at most: with `for: 1h`, one restart can never fire it, only
restarts that keep coming for an hour. The report says so of a rule whose
`for` is that long already: "Its for (1h) is as long as the 30m its
condition counts over, or longer: one event alone can never fire it; only
events that keep coming for 1h will."

**What a rule adds.** Precision doesn't say whether anyone would miss a rule:
one that is right every time, but always after another, adds nothing. Each
rule gets the incidents it caught, those it caught before any other rule, and
those no other rule caught; a noisy rule's catch doesn't count beside one by a
rule that isn't noisy. A rule that caught two incidents or more is called
spare when, for each, a rule that isn't noisy and can still fire caught it as
soon or sooner, at the same severity or above (`critical`, `warning`, `info`;
other severities only cover their own). Of rules that caught it at the same
moment, the first by name stays, so every spare rule could go at once and each
incident would still be caught as soon. This is a question to ask, not a verdict:
it says only what happened in the period, and only among the rules that could be
replayed. With `--rule` or `--changed-only`, which judge some of the rules, only
how many incidents each caught is given.

**When they came.** A firing that caught nothing at three in the morning cost
more than one at three in the afternoon. The firings that caught nothing are
counted by when they started, in `--tz`: while people slept, off hours, or at
work. `--work-hours` (`mon-fri@08:00-18:00`) and `--sleep-hours`
(`22:00-08:00`) say when those are, or `none`. The defaults are close to
PagerDuty's, which also counts 21:00 to 22:00 as both; here each minute is one
or the other. Working hours may run past midnight, as a night shift's do
(`mon-fri@22:00-06:00`: the mornings after are worked). For a team on call in
shifts, or by turns across time zones, `--shifts` gives hours that take turns
instead: how long each turn is, when the first starts, and each turn's hours,
those it leaves out as above (`every=1w from=2026-11-02T09:00; tz=Asia/Seoul;
tz=Europe/Berlin sleep=23:00-07:00`, the pager changing hands on Mondays at
09:00). Turns of days or weeks change at the same time on the clock, the
clocks changing or not. The rules that woke people for nothing come first.

**Shifts from on-call schedules.** Real rotas swap, override and skip.
PagerDuty, Opsgenie, Jira Service Management, Grafana OnCall and IRM, and a
shared Google Calendar each export a schedule as an iCalendar feed, whose
turns name people, not hours. `--on-call FILE` names the feeds and the
people in them with their hours, and `--shifts on-call=NAME` (or a team's
shifts in `lapilli serve`) counts each firing by the hours of whoever's turn
it came in; between turns, and for someone the file doesn't name, by the
hours given otherwise:

```yaml
schedules:
  - name: payments
    url_file: /secrets/payments-ics   # PagerDuty's webcal URL, its key in it
  - name: platform
    url: https://api.opsgenie.com/v2/schedules/platform.ics
    token_file: /secrets/opsgenie
    auth: genie                       # bearer (default), genie or basic
    who: '^(.+?) \(user\) is on call' # the person, out of each turn's title
people:
  - {name: Jane Doe, also: [jane@example.com, jdoe], tz: Asia/Seoul, sleep: '23:00-07:00'}
  - {name: Ali Kaya, also: [ali@example.com], tz: Europe/Istanbul}
```

A turn's person is an attendee's email that is someone's in the file, else
what `who` takes out of its title, or its title when that is someone's,
else the one attendee's email, else its title (a person added to the file
later, by any of these, is found in the turns kept before; a name is cut
to the ledger's 200 bytes). Two people in the file as an event's
attendees, or two events that start together, are said; a turn that starts
during another holds until it ends. An event that can't be read is passed
over and said, and the turns kept of whoever it names stay as they were.
Times are read in UTC, floating (the schedule's `tz`), or with a `TZID`:
an IANA name of a place, such as `Europe/Berlin`; else the feed's own
`VTIMEZONE` of that name, as Outlook and Exchange write one for `Pacific
Standard Time` (it wins over IANA's `EST`, a fixed -0500, when it keeps
summer time); else, with none, IANA's name or a Windows name by CLDR's
table. A zone's own rules are yearly, a day each year. A time the clocks
show twice is the first, as RFC 5545 has it. A time they skip is read by
the offset before (02:30 is 03:30), as RFC 5545 reads a `DTSTART`; a
repeat's day there is kept, and counted, where RFC 5545 (3.3.10) also
says it is ignored: Lapilli does so on purpose, as a turn dropped would
leave no one on call. Daily, weekly, monthly and yearly repeats are
expanded on the clock they are written on, so a weekly 09:00 turn stays
at 09:00 when the clocks change: with `INTERVAL`, `COUNT`, `UNTIL`,
`BYDAY` (numbered for monthly and yearly, as `1MO` or `-1FR`),
`BYMONTHDAY`, `BYMONTH`, `BYSETPOS` and `WKST`, a day a month doesn't have
skipped, with `EXDATE` and occurrences replaced by `RECURRENCE-ID`. The
dates `RDATE` adds are occurrences too, with or without an `RRULE`, as RFC
5545 has them: times (in UTC, floating, or with a `TZID`, read as
`DTSTART`'s), days, or periods (`VALUE=PERIOD`) of a time and its end or
its length, which give that occurrence its own end; each start once, and
`EXDATE` leaves any of them out, `DTSTART` too. What can't be read
(`BYWEEKNO`, `BYYEARDAY`, `BYHOUR`, a repeat more often than daily, a zone
none of these is; an `RDATE` of days beside a `DTSTART` of a time, which
doesn't say when in the day, or the other way round; a period of days, of
more than a year, or that doesn't end after it starts) is passed over and
said. A feed's URL holds its secret, so it is
read from its file, never shown or logged, fetched over https only, with
no redirect followed, 8 MB and 30 seconds at most. `lapilli serve` reads
each feed every `every` (30 minutes by default) and keeps the turns in its ledger past what the feed
looks back (PagerDuty a month, Grafana OnCall 15 days), and its Run
history page says when each was last read and whom its turns name that the
file
doesn't. A person's hours changed in the file count for every turn they
had.

**An alert after the incident.** A black firing can be an incident's own
alert, come too late: its condition held in the incident, but its `for`
and how late its metrics arrive (`--late`, `query_offset`) ran past the
incident's end, and it fired once the incident was over, its labels
agreeing. It stays black, and the incident missed by its rule, as nothing
came in time; the report says so all the same. The incident's row names the
rule and how long after its end it fired (`✗ nothing in time; after its
end: LB5xx +3m`), and a line under the rules counts the black firings that
came after an incident, by rule (`late` of a rule, `fired_after` of an
incident, `after` of a firing in `--json`). When a firing's condition held
is its start, less the least `for` and lag of its rule as it stood in the
period: a rule with no `for`, on time, held only once it fired. It is worked
out from the rules replayed, with no step after the replays, so `--extras`
doesn't cut it. A firing says when its condition held (`held_from`), so a
report's firings, read back with `--firings`, say the same; firings from
elsewhere, with no `held_from`, came after nothing, and a `held_from` after
its firing's start is read as none. `lapilli serve` says it too (0.70),
worked out as its ledger is read, of the incidents that count, every one it
has, as its stones are given: `/api/v1/report`, the digest and the reports
it sends (`fired_after` of a missed incident), the Incidents page and an
incident's own, a rule's page (how many of its firings that caught nothing
came after one) and MCP (`fired_after`, `late`), its rules as they stood
over what is shown.

**What nearly caught it.** For an incident nothing caught, or only noisy rules
did, each rule replayed from `--rules`, as it stood then, is looked at about
then: one whose condition held, but not for its `for`, or only long enough to
fire after the incident was over, nearly caught it. The longest shorter `for`
that would have caught it is given, with when the rule would then have fired.
Of the rest, one whose condition compares a value with a number (`errors >
0.9`, `0.9 < errors`, or a Grafana threshold or math expression with one
number) nearly caught it when the value came within a fifth of the number, for
the rule's `for`: the round number short of what it reached that would have
caught it is given (`> 0.9 → 0.87`, in steps of a tenth of the number's
leading figure), with when the rule would then have fired. A condition with
several numbers, a Grafana range (`within_range`, `outside_range`) or PromQL
joining comparisons with `and`, `or` or `unless`, has each looked at, the
others as they are. The first comparison of an `and` or `unless` returns its
own values where the rest holds, so they are read as one comparison's are; for
any other, the rule is replayed with nearer round numbers, the furthest within
a fifth first and, when the rule fires during the incident with that, halving
the way back to the nearest that does (`rate(requests[5m]) > 10 → 8`). A
Grafana math expression joining comparisons with `&&` or `||` has each
looked at, read where the rest lets it decide (`$A > 0.9 → 0.87`), and so
has each number of classic conditions, by the value its reducer gives
(`avg(A) > 5 → 4.5`); a classic range is replayed with nearer numbers. A comparison on the right of `unless`
kept the rule quiet, and its number moves the other way (`unless requests <
10 → 9`). For the first ten
tried, the report says how often the rule would have fired over the period
with that, and how often for nothing. A noisy rule, or one with a `for` longer
than a week, or with neither a `for` nor such a number, isn't looked at. It is
bounded: twenty incidents, five rules for each, 2,000 questions of the
history, within the period and `--timeout`; the report says when the time ran
out. The rules looked at for an incident are those whose labels agree with
it, as a firing's must to catch it; of one that lists the alerts its tool
linked to it, a rule it lists is looked at whatever `--match` says, a
stretch or a value of it nearly catching it where its labels agree with the
alert's, and with `--linked-only` only the rules it lists are.

**The steps after the replays**, a longer `for` for the noisy rules, near
misses, and what fired with no record, ask the history again, and can take as
long as the replays did. The progress line says which step a run is at and
how far it is (`then looking for rules that nearly caught the incidents
nothing caught, 120 of at most 2000 questions`). `--extras DUR` lets them take
that long at most, and `--extras 0` skips them; the report says when they
were skipped or cut short (`extras` in `--json`). A rule whose value never came within a fifth says nothing here: what a
change further than that would do is for `--compare`.

**Rules that share a name**, in different groups or with different labels
(a warning and a critical one, say), are told apart:
`HighErrorRate [severity=critical]`, `DiskFull [node-alerts]`.

**Rules that changed.** Rules change: one is added after an incident, a
threshold is raised. Today's rules, replayed over last month, judge a month
that ran on other rules. Give each version with the time it took effect, and
each is replayed while it was in effect:

```sh
lapilli verdict --prometheus http://prometheus:9090 --since 30d --incidents incidents.yaml \
  --rules rules-before.yaml \
  --rules rules-v2.yaml@2026-10-12T14:00:00Z \
  --rules rules-v3.yaml@2026-10-21T09:30:00Z
```

Give every file in effect from a time with that time, and the rules in effect
before the first time without one. Where a version takes over, alerts carry
over the way they would have: Prometheus keeps a rule's alerts through a
reload when its group, name and labels stay the same, pending or firing, and
applies the new `for`; Grafana keeps them when the rule (same uid) is
unchanged, and resolves them and starts afresh when it was edited or paused.

Rules kept in git give their versions. `recipes/versions/git.sh` writes them
as a directory that `--rules-history DIR` reads, one subdirectory a version,
named by when it took effect, holding the rule files as they stood then:

```sh
REPO=~/src/monitoring PATHS=alerts/ SINCE=2026-10-01 OUT=history recipes/versions/git.sh
lapilli verdict --rules-history history --prometheus http://prometheus:9090 --since 30d --incidents incidents.yaml
```

It is each version given as `--rules DIR@TIME`. The recipe takes each commit
on the branch that changed `PATHS` (a merge at the merge's time) and the
rules as they stood at `SINCE`; with `RENDER`, a command such as `helm
template`, it writes what that renders from each commit instead, for rules
rendered from a chart. A version's name is its commit's time, or, with
`DEPLOYS`, when it was deployed: `recipes/versions/argocd.sh` (an Argo CD
Application's sync history, from its API or `kubectl get application -o
json`) and `flux.sh` (a Flux Kustomization's history, 2.7 on, or a
HelmRelease's whose chart version ends `+SHA`) write the revisions deployed
and when, and the versions are the rules each deploy put in place, from the last
deploy at or before `SINCE`, a rollback too, never dated before its
commit. Commits deployed together are one version; one not deployed yet
isn't in effect. Failed reconciles aren't deploys. Argo CD keeps ten syncs and Flux five: run them
each time, with `DEPLOYS_KEEP` to keep what earlier runs saw. Any tool
can write such a directory, or a person by hand: a name is an RFC 3339 time,
`2026-10-12T14:00:00Z`, or `20261012T140000Z`.

`lapilli serve` takes the same directory, beside `--rules`, for what came
before its first start: its first backfill then replays each day with that
day's rules, rather than all of it with today's. A ledger with versions of
its own keeps them and passes the directory over, so the flag can stay. In
the chart, `serve.rulesHistory` clones the repository the rules are kept in
and runs the recipe in an init container on each start; when git can't be
reached, it leaves the directory empty rather than hold the pod up, and
`serve` starts with today's rules. A version that can't be read (no rules,
none `--rules-select` keeps, a file that doesn't parse) is passed over,
saying which, and the one before it stays in effect; a version from after
now, a commit dated ahead of the clock, is passed over, and so is the whole
directory once the ledger has versions of its own. A version is files: one
holding a link to outside it is refused. With `verdict`, `--rules` beside
`--rules-history` are the rules as they are now, the newest version.

Rules rendered from a chart are read out of the whole rendering: of what
`helm template` or `kustomize build` writes, only the documents of rules are
kept (ConfigMaps of rule files or Grafana provisioning files,
`PrometheusRule`s), and the rest, Deployments and Secrets, passed over and
counted. `--rules-select`, a label selector as `kubectl -l` takes one, keeps
only the labelled objects, such as the ConfigMaps Grafana's sidecar loads:

```sh
REPO=~/src/platform PATHS=charts/monitoring/ SINCE=2026-10-01 OUT=history \
  RENDER='helm template monitoring charts/monitoring' \
  RENDER_PREPARE='helm repo add grafana https://grafana.github.io/helm-charts >&2 && helm dependency build charts/monitoring' \
  recipes/versions/git.sh
lapilli verdict --rules-history history --rules-select grafana_alert=1 --prometheus http://prometheus:9090 --since 30d --incidents incidents.yaml
```

`RENDER_PREPARE` gives each commit what it lacks, such as dependencies older
commits don't vendor; `UNRENDERED=skip` passes over a commit that still
can't be rendered, saying which and why. `lapilli serve` given
`--rules-history` without `--rules` follows the directory: its newest version
is the rules in effect, and each version `recipes/versions/follow.sh` adds
for a new commit is recorded from the commit's time, the hours since then
replayed again. The chart does this with `serve.rulesHistory` and no
`rules.configMaps`, fetching the branch every five minutes.
