# What an operations team asks of its alerts

Status: built, for v0.30.0. A team running an operations center measures
its alerting by goals such as these, and counts Slack messages by hand to
know where it stands:

- at least 80% of critical alerts needed action;
- no alert re-fires within an hour, any week;
- infrastructure alerts come within five minutes of the first sign, before
  the application's team notices;
- no failure path is silent.

Lapilli has most of what these need: every firing judged against what went
wrong, by rule and week, with incidents, impact windows and near misses. This
gives the rest, and says it where the team reads it: the UI, the digest and
reports, `/metrics` and the chart's Grafana dashboard, and an API for other
tools. One ledger schema change (017). No change to how a firing is judged.

## 1. Re-firing

**What.** A firing re-fired when it started within `--refire` (1h by
default, changeable in the UI) of the same alert last resolving: the same
rule and labels. The ledger has kept that stone since 0.6, and counted it by
day and week; what it didn't say is how it stands.

**Where.**

- `/rules`: the column says the share of the rule's firings that re-fired
  and in how many of the weeks shown one did; the rules can be put in order
  of re-fires (`?sort=refired`).
- A rule's page: its re-fires week by week over twelve weeks, and its five
  alerts (label sets) that re-fired most.
- The overview: a tile of the last whole week's re-fires, beside the week
  before, saying in how many of the whole weeks shown one came; the weekly
  table has a column for them.
- The digest and a report's `refired` section: how many re-fired, of how
  many rules, the alert that re-fired most, and how many of the last twelve
  whole weeks had one. `digest.ready` carries the numbers (`refired`).
- `/metrics`: `lapilli_rule_refired` (as before), `lapilli_rule_refired_weeks`
  by rule, and `lapilli_refired_weeks`, everyone's and each team's
  (`team`), with `lapilli_weeks_counted`, the whole weeks they are of.
- `/api/v1/summary`: by week.

"Weeks with a re-fire" means one thing everywhere (`tallies.whole`): of
the weeks a place shows, the whole ones the ledger has all of, so neither
the week under way nor the one its first firing fell in, how many had a
firing that re-fired, with how many were counted beside it. The overview
and a rule's page count the twelve weeks they show (eleven whole at most),
`/rules` the weeks picked, the digest and `/metrics` the twelve whole weeks
before this one. A goal of none a week reads 0.

**How it stays fast.** Schema 017 adds a partial index of re-fired firings
by rule, so a rule's alerts that re-fired most are read without its other
firings. Across rules (the digest, the summary), the week's firings are
read once by their start.

## 2. Firings during deploys

**What comes in.** `--deploys FILE` (the chart's `serve.deploys`), read
again each run as `--incidents` is, and owning the ledger's deploys as the
incidents file owns its incidents:

```yaml
deploys:
  # What recipes/versions/argocd.sh or flux.sh wrote for an Application:
  # REVISION TIME a line, each a deploy of what these labels name.
  - labels: {service: checkout, namespace: shop}
    history: checkout.txt
  # One written down, from start to end.
  - labels: {namespace: payments}
    start: 2026-11-10T10:00:00Z
    end: 2026-11-10T10:06:00Z
    revision: v1.2.3
```

The recipes 0.27 added write when each revision was deployed, for the rules'
own history (`git.sh`'s `DEPLOYS`); run on the Applications of the services
the alerts are about, with `DEPLOYS_KEEP`, they write these histories too.
A history is a regular file in the deploys file's directory, or below it,
named by a relative path (`filepath.IsLocal`) and read through `os.Root`:
not an absolute path, one climbing out, a link out, a FIFO or a device. What
is refused names the history as given, not where a link led. A history's
lines all share their entry's labels: the ledger keeps each label set once
(`deploy_labels`).

The file and its histories are read each run and hashed (sha256); when
they hash as the last read did, with the same times before and after, the
ledger reads and writes nothing more. When it was last read, and kept, is
`lapilli_serve_deploys_read_timestamp_seconds`: what the dashboard says the
job writing it stopped by.

**What is marked.** A firing came during a deploy when it started from
`--deploy-before` (0 by default) before the deploy's start to
`--deploy-after` (10m by default) after its end, each a day at most, and
agrees with the deploy on every label both have, one at least. A label
the firing hasn't stands for any value: a deploy of `{service: checkout,
namespace: shop}` marks checkout's firings and the namespace's own
(`{namespace: shop}`), not cart's in the same namespace. A deploy that
names no label is everything's. A firing a quiet window excused isn't
judged, and isn't marked. (Built first as "any label agrees", which marked
51% of a review's firings against 7% this way: every service in a
namespace whenever one of them deployed.)

**Kept, not dropped.** The mark is beside the stones and changes neither: a
firing during a deploy that caught an incident is still white. The ledger
keeps it on each firing (`firings.deploy`), and counts by day and week the
firings during deploys and those of them that caught something
(`deploy`, `deploy_white`), so precision is read both ways without reading
firings: with them, as everywhere else, and without them, beside it.

**Where.** A column and an order on `/rules`; a line on a rule's page; a
tile and a column on the overview; the digest and a report's `deploys`
section; `lapilli_rule_firings_during_deploys` and
`lapilli_rule_caught_during_deploys`; `/api/v1/summary`. Each shows only
when the ledger keeps deploys.

**Judged again where it changes.** Reading the file, the ledger keeps what
is new and drops what is gone, and marks again the firings each meets;
`--deploy-before` or `--deploy-after` changed since the last read marks
again every deploy's. The spans are pended in the same write, those less
than six hours apart made one and none past the newest firing (a run
judges its own period, deploys included), and judged a week's worth at a
time, as a quiet window's are: a long span a week at a time, short ones up
to 256 in a write, the list written back once a write; two seconds' worth
before the read returns.

**Bounds.** The file and each history 8 MB at most; 50,000 deploys; a deploy
a day long at most, starting no later than an hour after the file is read,
16 labels, 4 KB of them, and 8 MB of labels in all, counted for each
deploy; a revision and a source 200 bytes each; a history's lines only
`HEX TIME` as the recipes write them. What breaks a bound refuses the whole
file and the ledger keeps what it had, the status and the runs page saying
why. Deploys aren't pruned with `--keep`: the file owns them, and would
bring them back.

**Without `--deploys`.** A ledger that keeps deploys from when it was given
says so on the status, as their firings stay marked; giving `--deploys` a
file holding `deploys: []` once takes them away. `--deploy-before` and
`--deploy-after` without `--deploys` are refused.

## 3. User impact as incidents

`lapilli serve --impact PROMQL` has found impact windows since 0.6: each
series of the expression, while it returns something for `--impact-for`, is a
window with that series' labels, judged against as an incident is (and so
by `--match`), kept in the ledger, and confirmed or dismissed in the UI.
What was missing is reading them as the team does:

- `/incidents` picks written incidents or impact windows (`?source=`), and a
  team's (`?team=`: one of its rules caught it, or it has a value of a label
  the team owns by); it says, of those judged against, how many were caught,
  caught only by noisy rules, or missed, and the median lead; each line, the
  lead of its first alert of a rule that isn't noisy ("5m before", "3m
  after"), and for one not caught, the rule that nearly caught it.
- `/metrics`: `lapilli_incident_state` and `lapilli_incident_lead_seconds`,
  a series for each incident of the last seven days (the latest 500), with
  `source` written or impact; and `lapilli_incident_team`, a series for each
  team that owns one (500 in all at most), without a team label for one no
  team owns, which a dashboard joins on `incident`.

An incident's state is the same everywhere: a catch by a rule noisy in the
incident's week counts as only noisy, on the pages, in the summary and in
`/metrics` alike (`/metrics` judged it by the seven days to now before the
review, so the same incident could be caught on `/incidents` and only noisy
on the dashboard).
- `/api/v1/summary`: `impacts` apart from `incidents`, by week and team.

**Lead** is how long before the incident's start the first alert of a rule
that isn't noisy came: positive before, negative after. It is
`lapilli_incident_detection_delay_seconds` turned round, which stays as it
was.

## 4. The summary API

`GET /api/v1/summary?from=&to=&team=&critical=` (and MCP's `summary` tool):
a row for each week, from Monday in `--tz`, and for everyone or a team.

- `from`, `to`: a day (2026-11-02) or an RFC 3339 time; the weeks they fall
  in and those between, 53 at most. By default the four whole weeks before
  this one, and this one (`partial`).
- `team`: a team's name, repeatable (each once; no more than there are
  teams), or `*` alone for everyone and every team; everyone's when not
  given. A team's row has its rules' firings and the incidents it owns.
- `critical`: the severities counted as critical, comma-separated
  (`critical` by default), 32 at most, each 64 bytes at most. A rule whose
  severity differs across versions, or rules of one name
  (`critical/warning`), counts when any of them is: its firings aren't told
  apart by severity in the tallies.

A summary of every team, or of more than 53 rows, is heavy: it takes the
one turn there is for such a request (as a report with every firing does),
held until its answer is written, which has 30 seconds; another waits up
to 30 seconds and is then refused (503). MCP's `summary` reads the same
way, with the same bounds. A request given up stops at the next row.

```json
{
  "schema": 1,
  "from": "2026-10-26T00:00:00Z", "to": "2026-11-09T00:00:00Z", "zone": "UTC",
  "settings": {"lead": "15m", "refire": "1h", "deploy_before": "0s", "deploy_after": "10m", "match": ["service"],
               "noisy_at": {"firings": 10, "precision": 0.2}, "critical": ["critical"], "hours": true, "deploys": true},
  "ledger_to": "2026-11-03T12:00:00Z",
  "rows": [{
    "from": "2026-10-26T00:00:00Z", "to": "2026-11-02T00:00:00Z", "partial": false, "team": "",
    "firings": 20, "caught": 1, "precision": 0.05, "excused": 0,
    "critical": {"firings": 8, "caught": 1, "share_caught": 0.125},
    "by_severity": {"critical": {"firings": 8, "caught": 1, "share_caught": 0.125}, "warning": {"firings": 12, "caught": 0, "share_caught": 0}},
    "refired": {"firings": 7, "share": 0.35, "rules": 1, "worst": {"rule": "Flap", "labels": {"service": "search"}, "refired": 7}},
    "off_hours": {"firings": 3, "nothing": 3}, "asleep": {"firings": 9, "nothing": 8},
    "during_deploys": {"firings": 1, "caught": 1, "precision_without": 0},
    "incidents": {"total": 0, "caught": 0, "noisy_only": 0, "missed": 0, "median_lead_seconds": null},
    "impacts": {"total": 1, "caught": 1, "noisy_only": 0, "missed": 0, "median_lead_seconds": 300}
  }]
}
```

**Versioned.** `schema` is 1. A field may be added without changing it; one
changed in meaning or taken away changes it, and the API's path stays. A
share is null with nothing to divide by, `off_hours` and `asleep` null with
no hours given, a median null with nothing caught. `TestSummaryAPI` holds
the fields by name.

**Cost.** It reads the weekly tallies, the hours each week came to (kept
per week as the pages keep them), the weeks' incidents, each set apart by
week once, and each week's re-fired firings once; it is cached, as the
other answers are, until the ledger changes (up to 2 MB). Fifty-three weeks
for 57 teams over 200,000 firings takes 0.16 s, in `TestPagesScale`'s
budget.

## 5. The Grafana dashboard

The chart's dashboard (`dashboard.enabled`) is rebuilt around the
evaluation: the last seven days' critical firings that caught something,
precision, firings, re-fires and the whole weeks with one, firings during
deploys and precision without them, what caught nothing by when it came;
impact windows and incidents by what caught them, and the median lead of
each; each incident; week to week, the stones, re-fires and firings during
deploys; each rule's scorecard; and how long since each source last
brought something (firings copied in, notifications, the deploys file, the
ledger's runs), the sources gone quiet past `--source-quiet`, and failed
runs.

**Variables.** Data source, instance, team, severity, and the severities
counted as critical (a custom variable, several at once, matched as
`(.+/)?($critical)(/.+)?` so a rule whose versions' severities differ is
counted, as the summary counts it). The rule metrics carry `team` when a
team owns the rule, so a team picked reads its own; the chart's
ServiceMonitor honours them (`honorLabels`), so a `team` target label
doesn't rename them, and bounds a scrape (`sampleLimit`). The incident
panels join `lapilli_incident_state` to `lapilli_incident_team` on
`incident`, so each incident is counted once however many teams picked own
it. "Weeks with a re-fire" shows the whole weeks counted beside it, and
says what it shows: everyone's with every team picked, a team's own, and
with several, the most any of them had. "Since the last run" and the other
stats show none (not the year 1) where there is nothing: before the first
run, no time is given.

**Checked against real servers.** `test/e2e/dashboard.sh` runs a real
`lapilli serve` replaying rules written on time alone over seventeen days against
a real Prometheus 3.5.0, with an impact expression, incidents, deploys, a
team, firings copied in and a notification taken, has the Prometheus scrape
its `/metrics`, loads the dashboard into a real Grafana 13.3.1, which must
take it, and asks Grafana (`/api/ds/query`) each panel's every query, with
every variable at all, and again for the team: each must return data. It
runs in CI, as `loki.sh` does, not as a required check.
`TestDashboardMetrics` holds, in every test run, that each metric the
dashboard reads is one `/metrics` gives and each variable it uses is
defined.

## 6. Runbook annotations

The content check looked for a runbook in `runbook_url`, or else `runbook`.
`--runbook-annotations` (the chart's `serve.checkContent.runbookAnnotations`)
names where to look, the first a rule has: `runbook_url`, `runbook`,
`playbook_url` and `playbook` by default. A rule keeps its other
annotations, 32 at most, `runbook_url`, `runbook`, `playbook_url` and
`playbook` first, to read them from; a name past the 32 falls back to the
source's own (`runbook_url` or `runbook`, always read). A cloud's alarm,
which has none, keeps the link its source gives.

A runbook kept beside the rules, as a path in their repository with an
anchor or not (`docs/runbooks/disk.md#full`), is a runbook: names joined by
a slash, or a file's name with its extension, at least one with a letter
or a digit, before any anchor, with no `..`. What teams write in a
runbook's place isn't one, in any letter case: `N/A`, `TODO.`, `none`,
`TBD#`, `...`, `-/-`, so `runbook_url: N/A` is said again to be no link.
It is shown as a path, or, with `--runbook-base URL` (the chart's
`serve.checkContent.runbookBase`: https, no user name, query or anchor), as
a link under that URL, on its host alone.

A runbook link with a user name (`https://wiki.example.com@evil.example/`)
is never a link: it looks like one site's and goes to another's. The
content check says so (`runbook_not_url`), for every runbook annotation.

## Reviewed as an attacker would

- **The deploys file** is whoever runs `serve`'s, as the incidents file is.
  The histories it names stay in its directory (above), regular files read
  with a size limit. What it holds is bounded, its labels by deploy and in
  all, refused whole when anything breaks a bound, and an unchanged file
  costs a read and a hash. Its labels and revisions are shown escaped, as
  the rest of what comes from outside. Nothing is fetched.
- **The summary** is a read, of 53 weeks at most, each team once, 32
  critical severities; a heavy one takes the one heavy turn, the API's and
  MCP's alike. Before the review, `team=` repeated 5,600 times (in the 64 KB
  of headers allowed) made one request of 293 MB of JSON and a heap of more
  than 1 GB, outside the turn and too big to cache, so every such request
  did it again.
- **`/metrics`.** The incident series are the last seven days' 500 latest,
  and which teams own them 500 series in all: before the review, an incident
  every team owned was a series for each team, so 40 teams made 20,000.
  Their ids and team names come from the incidents file, the UI, copies and
  the teams admins name; label values are escaped.

## Builder's decisions

- **A deploy and a firing agree on every label both have** (settled in
  review): a namespace's own alert is during a service's deploy in it,
  another service's isn't.
- **The mark is kept, not worked out when read.** As quiet windows'
  excuses are: every page, report, metric and the summary then read tallies.
- **`--deploy-before` and `--deploy-after` are flags, not settings in the
  UI** (settled in review). Changing one marks every deploy's firings again
  on the next read; the UI's settings are those that judge stones.
- **Lead in the new places, delay where it was.** The UI and the summary say
  lead (positive before); the old metric keeps its delay.
- **The rule metrics gained a `team` label.** A query summing by `rule` and
  `severity` is unchanged; one matching a rule's series on every label sees
  a rule's series change when its team does.

## Settled in review

Two reviews (as an attacker, and for correctness) of the first build; what
they found is fixed as described above, each with a test that fails
without it. Asked and answered: a deploy's window opens before its recorded
time by `--deploy-before` (0 by default: Argo CD's `deployedAt` is when a
sync finished); a repository path becomes a link with `--runbook-base`;
`dashboard.sh` runs in CI beside `loki.sh`; `--deploy-after` stays a flag.

## Questions for review

- Should the recipes write Argo CD's `deployStartedAt` too, so a deploy has
  an end and a start of its own, rather than a time before it for every
  deploy?
