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

"Weeks with a re-fire" is of the whole weeks before this one, twelve at
most, from the ledger's first firing: a goal of none a week reads 0.

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
A history's path is from the file's directory.

**What is marked.** A firing came during a deploy when it started from the
deploy's start to `--deploy-after` (10m by default, a day at most) after its
end, and has the value of a label the deploy names, any of them: the same
service, or the same namespace. A deploy that names no label is
everything's. A firing a quiet window excused isn't judged, and isn't marked.

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
is new and drops what is gone, and marks again the firings each meets, from
its start to `--deploy-after` past its end; `--deploy-after` changed since
the last read marks again every deploy's. The spans are pended in the same
write and judged a week at a time, as a quiet window's are, two seconds'
worth before the read returns. A run judges its own period as ever, deploys
included.

**Bounds.** The file and each history 8 MB at most; 50,000 deploys; a deploy
a day long at most, 16 labels, 4 KB of them, a revision and a source 200
bytes each; a history's lines only `HEX TIME` as the recipes write them.
What breaks a bound refuses the whole file and the ledger keeps what it had,
the status and the runs page saying why. Deploys aren't pruned with
`--keep`: the file owns them, and would bring them back.

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
  a series for each incident of the last seven days (500 at most), once for
  each team that owns it, `source` written or impact.
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
- `team`: a team's name, repeatable, or `*` for everyone and every team;
  everyone's when not given. A team's row has its rules' firings and the
  incidents it owns.
- `critical`: the severities counted as critical, comma-separated
  (`critical` by default). A rule whose severity differs across versions
  (`page/critical`) counts when any of them is.

```json
{
  "schema": 1,
  "from": "2026-10-26T00:00:00Z", "to": "2026-11-09T00:00:00Z", "zone": "UTC",
  "settings": {"lead": "15m", "refire": "1h", "deploy_after": "10m", "match": ["service"],
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
per week as the pages keep them), the weeks' incidents, and each week's
re-fired firings once; it is cached, as the other answers are, until the
ledger changes. Fifty-three weeks for every team over 200,000 firings is in
`TestPagesScale`'s budget.

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
counted as critical (a regular expression). The rule metrics carry `team`
when a team owns the rule, so a team picked reads its own. An incident can
belong to two teams: its series are one per team, and the panels count
them by incident (`max by (incident)`), so with every team picked each is
counted once. `lapilli_refired_weeks` has everyone's series beside each
team's, and the panel takes the most of those picked: everyone's for all.

**Checked against real servers.** `test/e2e/dashboard.sh` runs a real
`lapilli serve` replaying rules written on time alone over ten days against
a real Prometheus 3.5.0, with an impact expression, incidents, deploys, a
team, firings copied in and a notification taken, has the Prometheus scrape
its `/metrics`, loads the dashboard into a real Grafana 13.3.1, which must
take it, and asks Grafana (`/api/ds/query`) each panel's every query, with
every variable at all, and again for the team: each must return data.
`TestDashboardMetrics` holds, in every test run, that each metric the
dashboard reads is one `/metrics` gives and each variable it uses is
defined.

## 6. Runbook annotations

The content check looked for a runbook in `runbook_url`, or else `runbook`.
`--runbook-annotations` (the chart's `serve.checkContent.runbookAnnotations`)
names where to look, the first a rule has: `runbook_url`, `runbook`,
`playbook_url` and `playbook` by default. A rule keeps its other
annotations, 32 at most, to read them from; a cloud's alarm, which has none,
keeps the link its source gives. A runbook kept beside the rules, as a path
in their repository with an anchor or not (`docs/runbooks/disk.md#full`),
is a runbook: shown as a path, not as a link to follow, since Lapilli
doesn't know the repository's address. A word alone isn't one.

## Reviewed as an attacker would

- **The deploys file** is whoever runs `serve`'s, as the incidents file is,
  and so are the paths it names. What it holds is bounded (above), read with
  a size limit, refused whole when anything breaks a bound, and its labels
  and revisions are shown escaped, as the rest of what comes from outside.
  Nothing is fetched.
- **The summary** is a read, of 53 weeks at most, cached; its team names are
  matched against the ledger's, not used in a query.
- **`/metrics`.** The incident series are the last seven days' 500 latest,
  each once for each team that owns it: their ids and team names come from
  the incidents file, the UI, copies and the teams admins name; label values
  are escaped. The rules' `team` label is the team's name.

## Builder's decisions

- **A deploy names what it deployed by labels, matched by any.** "The same
  service or namespace" reads as any label the deploy names; a deploy written
  `{cluster: prod}` marks every firing in the cluster, which the deploys file
  says. The other reading, every label the firing has agreeing, would leave
  out a namespace's alerts during a service's deploy.
- **The mark is kept, not worked out when read.** As quiet windows'
  excuses are: every page, report, metric and the summary then read tallies.
- **`--deploy-after` is a flag, not a setting in the UI.** Changing it marks
  every deploy's firings again on the next read; the UI's settings are
  those that judge stones.
- **Lead in the new places, delay where it was.** The UI and the summary say
  lead (positive before); the old metric keeps its delay.
- **The rule metrics gained a `team` label.** A query summing by `rule` and
  `severity` is unchanged; one matching a rule's series on every label sees
  a rule's series change when its team does.

## Questions for review

- Should a deploy's window open before its recorded time too? Argo CD's
  `deployedAt` is when a sync finished; a rollout's first pods start
  earlier. A `--deploy-before` would cover it; the recipes could also write
  `deployStartedAt`.
- Should a repository path runbook become a link, with a base URL
  (`--runbook-base`)?
- Should `dashboard.sh` run in CI, beside `loki.sh`? It pulls Grafana and
  Prometheus and takes a minute.
