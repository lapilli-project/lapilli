# What an alert says

An alert that fires at the right time can still cost the person woken: a
summary that prints `{{ $labels.instance }}` with no instance, no runbook, a
severity nobody routes on. With `--check-content`, `lapilli verdict` checks
what each rule says when it fires, and how it is written, instead of judging
([docs/design/content.md](design/content.md)):

```
$ lapilli verdict --rules alert-rules.yaml --check-content --severities critical,warning --scrape-interval 1m
What 4 rules say: 3 problems in 3 rules, 3 guesses apart.
Not measured: give --prometheus to replay the rules, for the labels their alerts have, and --incidents for which are noisy.

RULE          KIND  PROBLEM                         DETAIL
DiskFull      fact  no runbook link
QueueDepth    fact  a template that doesn't parse   line 1: unclosed action
ExportJob5xx  fact  a severity not in --severities  crit

Guesses, which may be wrong:
RULE          GUESS                                  DETAIL             WHY
ExportJob5xx  a label its expression may not give    instance           read from its aggregation; it hasn't fired to tell
ExportJob5xx  a range shorter than two scrapes       rate[1m] < 2 × 1m  the scrape interval is the global one, and a job may have its own
TargetGone    absent() with no for: a restart pages                     a series gone for a moment may be what it is there for
```

| Check | Kind |
|---|---|
| No summary or description; no runbook, or one that isn't a link: an http or https URL, or a path in a repository | fact |
| A template that doesn't parse, in its source's own language | fact |
| No severity; one not in `--severities` | fact |
| The same expression, `for` and labels as another rule | fact |
| A `for` as long as the range its condition counts events over, or longer: one event alone can never fire it | fact |
| A template label no firing over the period had (with `--prometheus`) | measured |
| Noisy, with no `for` (with `--prometheus` and `--incidents` or `--impact`) | measured |
| A template label after an aggregation by other labels, for a rule that hasn't fired | heuristic |
| A severity fewer than two rules use, among ten or more, without `--severities` | heuristic |
| `rate`, `increase`, `irate` or `delta` over less than two scrapes | heuristic |
| `absent()` with no `for` | heuristic |

What each source says is read from its own fields: Prometheus's
annotations, as the managed services that run its rules keep them,
Grafana's, CloudWatch's `AlarmDescription`, a Google Cloud policy's
documentation, Azure Monitor's alert rules and OCI's alarms (`az monitor
metrics alert list`, `oci monitoring alarm list`, now read as rules too:
listed, not replayed). A rule's runbook is the first of
`--runbook-annotations` it has (`runbook_url`, `runbook`, `playbook_url`,
`playbook`); a cloud alarm's, the first link in its description; a source with no severity (CloudWatch, Azure's activity log
alerts) isn't faulted for having none. Templates are parsed, never run, with
Prometheus's and Grafana's functions stubbed; a function this version
doesn't know is taken as one a newer Prometheus has. Links are checked to be
http or https URLs without a user name, or paths in the rules' repository
(`docs/runbooks/disk.md#full`; not `N/A` or `TODO`), never visited. The range check counts with
`--scrape-interval`, or else with those `/api/v1/status/config` gives, when
it is served: a range over a selector that names its job
(`rate(x{job="batch"}[3m])`, or `job=~` the longest of the jobs it can be)
with that job's own interval, by the job label `/api/v1/targets` says its
targets end up with (under the Prometheus Operator, a ServiceMonitor's job
is its service's name); a range over a selector that names none
(`rate(batch_runs_total[3m])`) with the longest interval of the jobs its
series come from, asked once, `count by (job) (batch_runs_total)` over the
last ten minutes; any other, and one whose query fails, with the global
one. The finding says whose: `rate[3m] < 2 × 2m (job "batch")`. With `--grafana`, the
notification templates each Grafana rule is sent with are checked too
(below). `--json` writes it all, what each rule says included.

A Grafana rule's notification says what its contact point's templates say.
With `--grafana`, Lapilli reads Grafana's notification templates, contact
points and notification policies through the provisioning API, with the
same token (a Viewer's reads them), and works out which templates each rule
is sent with: its own contact point (`notification_settings`), or the ones
the policies route it to by its labels, `alertname` and `grafana_folder`. A
policy matching a label the rule doesn't set may match its series or not,
so both ways are followed, and a template counts only when every way uses
it. Each is checked as a rule's annotations are: an annotation it shows
(`.Annotations.summary`, `description`, `runbook_url`, or
`.CommonAnnotations.`) that the rule hasn't, as a fact; a label it names
(`.Labels.x`, `.CommonLabels.x`, `.GroupLabels.x`) that no firing had, as
measured, or that the expression can't give, as a guess. Under an `if` or a
`with` naming it, nothing is faulted. A rule whose routing would take
more than its share of the check's work or time isn't checked against
templates, and its page says why. Nothing is written to Grafana.

The other rules, written in Prometheus's templates, are sent by
Alertmanager, whose receivers' fields are templates too. With
`--alertmanager-config`, Lapilli reads Alertmanager's configuration and
works out, by the route tree (each route's `matchers`, `match` and
`match_re`, `continue`, the receiver a route inherits), which receivers a
rule's alerts reach, by the labels it and its group set and its
`alertname`; as with Grafana, both ways are followed where a route matches a
label the rule doesn't set. Each field of those receivers that is a
template, and the templates it uses (`{{ template "slack.text" . }}`, and a
file's own `slack.default.title` for a Slack integration with no title of
its own), is checked as Grafana's are, and more: an annotation it shows that
the rule hasn't, by its own name (`.CommonAnnotations.dashboard` too, and
`runbook_url` for a rule whose runbook is in `runbook`); and a template
that doesn't parse, as a fact. Alertmanager parses a receiver's field only
when it sends with it, so `amtool check-config` doesn't catch that one: the
page goes out with nothing. It reads:

- `--alertmanager-config alertmanager.yml`: the file, and the template
  files its `templates:` globs name, beside it as Alertmanager resolves
  them. An absolute glob, which is Alertmanager's own path
  (`/etc/alertmanager/templates/*.tmpl`), is looked for by its last element
  beside the file, where a mounted Secret or ConfigMap puts them; no file
  outside the file's directory is read. A file in Mimir's, Cortex's or
  Amazon's format (`alertmanager_config` and `template_files`, as `aws amp
  describe-alert-manager-definition` gives it, decoded) is read as they
  hold it.
- `--alertmanager-config http://mimir:8080/api/v1/alerts`: a Mimir's or a
  Cortex's configuration for its tenant, with `--ruler-tenant` and
  `--ruler-token-file`, every template file in it, as Mimir loads them all.

Alertmanager's own defaults aren't followed: they show every label and
annotation there is, and fault nothing. No field whose name says it holds a
secret, or where a request goes (`api_url`, `routing_key`, `http_config`,
anything `_file`), is read. A template file that doesn't parse is said on
stderr, and in `serve`'s log: Alertmanager won't load the configuration. In
`lapilli verdict` a configuration that can't be read is an error; in
`serve` it is read once an hour, and one that can't be read leaves the check
as it was, the log saying why once. Nothing is written to Alertmanager.

`lapilli serve` checks the rules as it read them last, each `--watch`, so a
new summary shows at once without making a new version. Each rule's page has
"What it says" (its summary, description and runbook, escaped, the runbook a
link only when it is an http or https URL with nothing a template fills in)
with the problems found, facts first and guesses apart, each with why it is
one; the rules list marks a rule with a problem of fact;
`lapilli_rule_content_problems{rule, check}` in `/metrics` counts the facts
and what was measured, not the guesses, so a team can alert on a rule left
with no runbook; and MCP's `rule` carries what a rule says and its problems,
`rules` how many each has. What is measured comes from the ledger: the
labels a rule's alerts had over twelve weeks, and whether it was noisy over
four. Prometheus is asked for its scrape intervals once a day, and Grafana
and `--alertmanager-config` for their notification templates once an hour.
`--check-content=false` turns it off, and so does the chart's
`serve.checkContent.enabled`; `serve.checkContent.severities` and
`scrapeInterval` give the rest.
