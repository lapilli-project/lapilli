# What an alert says, checked

Status: built, for v0.22.0 (see "As built" at the end). It reads what
Lapilli didn't read before, and shows it on every page a rule has, so it was
written down first, under [GOVERNANCE.md](../../GOVERNANCE.md), as a pull
request (#40); it was merged on 2026-11-11, before its week was out, by the
maintainer's decision, with no comments. The companion to [serve.md](serve.md),
and to [reports.md](reports.md), a section of whose reports it is.

## Why

Lapilli judges when an alert fires: its condition, replayed, against what
went wrong. It doesn't look at what the alert says when it does. That is
the other half of whether an alert is worth its page: the person woken
reads the summary, follows the runbook link, and sees the labels the
template filled in. An alert that fires at the right time but says
"{{ $labels.instance }} is down" with no instance, or has no runbook, still
costs the person woken.

Teams find these by reading their rule files, which nobody does. Lapilli
already has every rule, as it stood each time it changed.

## What it would answer

For each rule, the problems a team should fix in what it says, or in how
it is written:

| Check | Kind | How it is known |
|---|---|---|
| No summary or description | fact | the rule's annotations, or its alarm's description, are empty |
| No runbook link | fact | no `runbook_url` (or the source's own field); a link that isn't an http or https URL |
| A template that doesn't parse | fact | the summary or description is parsed as its source's template language |
| A template names a label the alert never had | measured | `{{ $labels.x }}` where no firing of the rule in twelve weeks had label `x` |
| A template names a label the expression can't give | heuristic | `{{ $labels.x }}` after an aggregation by labels without `x` (`sum by (service)`), for a rule that hasn't fired |
| No severity | fact | no `severity` label (or the source's own severity field) |
| A severity no other rule uses | heuristic | a value fewer than 2 rules use, or not in `--severities` when given |
| Noisy, with no `for` | measured | the rule is noisy over four weeks and its `for` is zero |
| The same condition as another rule | fact | two rules whose expression (spaces aside), `for` and labels are the same |
| A `for` one event can't outlast | fact | a condition that counts events (`increase`, `changes`, `resets` or `rate` of a counter's range or a subquery of one, summed or maxed or not, times or over numbers or not, compared with a number no event passes and one does; not a counter of seconds or bytes, nor a `_sum`) over a range as long as its `for` or shorter; of a Grafana rule, a threshold of one number (`gt`, `gte`) or a math comparison of one ref, of its one PromQL instant query's value as it is or through `last`, `max`, `min` or `mean` (not a range query, which one event can hold for as long again as its range, nor a reduce with `replaceNN`), and not firing on no data (`Alerting`, `KeepLast`); in LogQL, a Loki ruler's or a Grafana rule's one instant Loki query, `count_over_time`, `rate`, `bytes_over_time` or `bytes_rate` of a stream's lines over a range after its pipeline, summed or maxed or not, its pipeline unwrapping no label, compared as above (a line one, of bytes one at least; 0.70); an expression over 4 KiB, or 8 brackets or reduces deep, isn't read for it: `increase(restarts[30m]) > 0.5` with `for: 1h` fires only on restarts that keep coming for an hour (`rules.Rule.ForOutlasts`) |
| A range shorter than two scrapes | heuristic | `rate`, `increase`, `irate` or `delta` over a range shorter than twice the scrape interval, when the interval is known |
| `absent()` with no `for` | heuristic | an expression of `absent(...)` and a zero `for`: a restart pages |

**Facts** say what the rule is. **Measured** checks use what the ledger
judged. **Heuristics** guess, so they are kept conservative: each says why
it is only a guess, is shown apart from the facts, and none is in a report's
count of problems unless the report asks for heuristics too.

Nothing here changes a stone. A rule with no runbook is judged as before.

## What it reads

What the rules say is in the documents the ledger keeps for each version of
the rules (`versions.docs`), and in what `serve` reads each `--watch`. But
`internal/rules` reads only what decides when a rule fires: its condition,
`for`, labels. It must also read what each source says a rule says:

| Source | Summary | Description | Runbook | Severity |
|---|---|---|---|---|
| Prometheus rule files, `PrometheusRule`, ConfigMaps, `--loaded-rules` | `annotations.summary` | `annotations.description` | `annotations.runbook_url` | `labels.severity` |
| Grafana (provisioning files, `--grafana`) | `annotations.summary` | `annotations.description` | `annotations.runbook_url` | `labels.severity` |
| Azure Monitor's Prometheus rule groups | `annotations.summary` | `annotations.description` | `annotations.runbook_url` | `labels.severity`, or the rule's `severity` |
| Google Cloud's `Rules`, `ClusterRules`, `GlobalRules` | `annotations.summary` | `annotations.description` | `annotations.runbook_url` | `labels.severity` |
| Amazon Managed Service for Prometheus rule groups | `annotations.summary` | `annotations.description` | `annotations.runbook_url` | `labels.severity` |
| CloudWatch alarms (`describe-alarms`) | the alarm's name | `AlarmDescription` | a URL in the description | none: a label the team sets, or none |
| Google Cloud alerting policies | `documentation.subject` | `documentation.content` | `documentation.links` | `severity` |
| Azure Monitor alert rules (metric, log search, activity log) | the rule's name | `description` | a URL in the description | `severity` (`Sev0` to `Sev4`); activity log alerts have none |
| OCI Monitoring alarms | the alarm's `displayName` | `body` | a URL in the body | `severity` |

The rules package gains `Rule.Says`: summary, description, runbook and
the source's template language, read with the rest. What decides firing
(`Rule.Key`) doesn't change: a new summary isn't a change to review or to
judge again.

Two lists aren't read as rules today, and must be, every cloud alike:
Azure Monitor's own alert rules (`az monitor metrics alert list`, `az
monitor scheduled-query list`) and OCI's alarms (`oci monitoring alarm
list`). They are read as CloudWatch's metric alarms and Google Cloud's other
policies are: listed, not replayed, with why. Their firings come in through
[recipes/firings](../../recipes/firings) as now, and tie to them by name.

The check is of the rules as `serve` read them last, each `--watch`. The
ledger keeps versions only when what decides firing changes, so it needs no
copy of what each version said: what to fix is what the rules say now.

## Templates

Each source fills its text in its own way, and each is parsed as its own:

- Prometheus and the managed services that run its rules: Go templates with
  Prometheus's functions (`$labels`, `$value`, `humanize` and the rest),
  parsed with those names stubbed, never run.
- Grafana: Go templates with Grafana's (`$labels`, `$values`).
- Google Cloud: `${...}` variables, as its documentation lists them.
- CloudWatch, Azure Monitor and OCI: plain text, not parsed.

A template is parsed, never executed: no function in it runs, and nothing
is fetched. A link is checked to be an http or https URL, not visited.

## What it shows

- **A rule's page:** "What it says": its summary, description and runbook
  as its source has them, and each problem, facts first, then heuristics
  with why each is a guess.
- **The rules list:** a mark on rules with a problem of fact.
- **Reports:** a `content` section, of problems new since the report
  before ([reports.md](reports.md)).
- **`/metrics`:** `lapilli_rule_content_problems{rule, check}`, one for each
  problem, so a team can alert on a rule left with no runbook.
- **MCP:** `rule` carries what the rule says and its problems; `rules`
  carries how many each has. What a rule says was written by whoever writes
  the rules: the server tells agents it is data, never instructions, as it
  does of incident titles and notes.
- **`lapilli verdict --check-content`:** the same, for the rules given, as a
  table, without a ledger: what is measured needs `--prometheus` and a
  period.

Annotations are shown escaped, as every name from outside is, and only
those named above: others may hold anything.

## Options

- `--severities LIST`: the severities the team uses; others are a problem
  of fact. Without it, a severity fewer than two rules use is a heuristic.
- `--scrape-interval DUR`: the interval the range check counts with. Without
  it, Lapilli asks Prometheus's `/api/v1/status/config` once a day and uses
  `global.scrape_interval`; where that isn't served (most managed services),
  the range check is off. An answer over 2 MiB isn't parsed: YAML is
  parsed whole, some 90 times its size, and a hostile Prometheus's few KB
  on the wire, gzipped, can be MBs of it.
- `--check-content=false` turns it off.

## Alternatives

- **An LLM reading the annotations** for clarity. Lapilli doesn't run a
  model of its own (ROADMAP, "Not planned"). It hands what a rule says, and
  its problems, to an agent over MCP, which can judge clarity and propose a
  rewrite for a person to make.
- **Keeping what every version said,** to show how a summary changed. What
  to fix is what it says now; git has the rest.
- **Visiting runbook links** to find dead ones. That would send a request
  to every host a rule names, from inside the cluster, on the say of
  whoever writes rules: a way to make Lapilli reach anything. Not done.

## Questions for review

- Is a severity used by fewer than two rules a fair guess at a typo, or
  only noise on a small set of rules?
- Should the expression checks (duplicates, ranges, `absent()`) live here,
  or with the suggestions a noisy rule already gets?
- Grafana marks some annotations as its own (`__dashboardUid__`,
  `__panelId__`): should a rule with a dashboard link count as having a
  runbook?

## As built

In `internal/content` (the checks), `internal/rules` (`Rule.Says`, and
Azure Monitor's alert rules and OCI's alarms read as rules),
`internal/serve` and `lapilli verdict --check-content`. No schema change:
what the rules say is kept in memory, as read last, and what was measured
is read from the firings and tallies the ledger has. Where it differs from
the above, or the above left it open:

- **The questions for review,** answered as built: a severity fewer than
  two rules use is guessed only among ten rules or more that have one, and
  among those of one kind: a label's values aren't counted with a cloud's
  own words for severity (`Sev2`, `CRITICAL`). The expression checks live
  here. A Grafana rule's dashboard and panel aren't a runbook.
- **`/metrics`** counts facts and what was measured, not guesses: an alert
  on it shouldn't rest on a guess. The value is how many of that check the
  rule has (two labels no firing had is 2).
- **Facts kept facts.** A cloud's alarm is summed up by its name, so only
  an empty description is "no summary or description". OCI's alarm list
  (`oci monitoring alarm list`) gives no `body`: an alarm read from it
  isn't faulted for no description or no runbook, as neither is known; one
  given with a `body`, empty, is. A source with no severity at all
  (CloudWatch, Azure's activity log alerts) isn't faulted. An Azure
  Monitor alert's severity is read only as Azure gives it, a whole number
  from 0 to 4, and an alert with no name is passed over, as every source's
  is.
  A severity label is matched to `--severities` exactly, as an alert
  manager's routes match it; a cloud's own field, in any case. A runbook a
  template starts (`{{ $externalURL }}/...`) isn't judged. Two rules of
  one alert name and condition are one alert to an alert manager, whichever
  groups they are in and though they are named apart on the pages
  (`HighErrors [slo]`), so only rules of different alert names are the same
  condition; a rule is told of three at most. A rule turned off or paused
  fires nothing, and isn't checked: a Google Cloud policy turned off too,
  whatever its conditions are written in.
- **Templates.** A function this version doesn't stub is taken as one a
  newer Prometheus or Grafana has, and the text parsed again (sixteen at
  most), so only what can't be parsed whatever the functions is an error.
  Google Cloud's text is checked for a `${` left open or empty. A template
  names a label as `$labels.x`, `index $labels "x"`, or `.Labels.x` where
  `.` is still the alert. A rule's own labels and `alertname` are never
  taken as missing, measured or guessed. Nor is a label named only in an
  `if`'s or a `with`'s condition, or under one whose condition names it
  (`{{ with $labels.cluster }} on {{ . }}{{ end }}`): missing, it renders
  nothing.
- **Measured** labels are those of the replayed firings, which carry the
  rule's own labels too: a template naming one is never faulted, though
  Prometheus leaves it out of `$labels`. That errs towards saying nothing.
- **Heuristics.** "A label the expression can't give" is guessed only of an
  expression that is one aggregation that keeps the labels it is grouped
  by (`sum`, `min`, `max`, `avg`, `count`, `group`, `stddev`, `stdvar`,
  `quantile`), by labels, alone or compared to a number; of any other
  shape, nothing is guessed. Expressions are read by their tokens, not a
  full PromQL parse, and those over 64 KB not at all; their parentheses are
  matched once, so reading one costs time in proportion to its length,
  however deep it nests. One that can't be read (a range or a parenthesis
  left open) is checked for what can be.
- **Faults of the check's own.** A rule whose check fails, which would be
  a fault of Lapilli's and not of the rule, has no findings, and the log
  says so once for each rule; the other rules are checked as ever.
- **When it is worked out again.** What was found stands until what it
  reads changes: the rules as read, the scrape interval, the names the
  latest version gives them, the firings (a run, incidents or a quiet
  window judged again), how many firings make a rule noisy, and the hour,
  as its weeks move on. A write that changes none, such as a notification
  kept, works out nothing again. Asked by many at once (pages, `/metrics`,
  reports), it is worked out once; the others wait and take what it
  found.
- **The scrape interval** is asked of Prometheus by the worker, once a day
  when it reads the rules, not by a page; a configuration with no
  `global.scrape_interval` is Prometheus's default, 1m. Since
  [serveops.md](serveops.md), each job's own interval is read too, and a
  range over a selector naming its job counts with it; and with
  `--grafana`, the notification templates a Grafana rule is sent with are
  checked beside its own annotations.
- **Reports and proposals.** The reports' `content` section is built with
  [reports.md](reports.md): this gives it one line, the problems by check
  and the guesses apart, of the rules a report covers. It is the problems
  the rules have now, not those new since the report before, as was
  proposed above: what is still wrong is what to fix. Its numbers in
  `report.ready` are the problems and rules counted, by check
  (`by_check`), the guesses by check (`guesses_by_check`), and the line.
  A finding the text can fix (no summary or description, no runbook, a
  runbook that isn't a link) is offered on the rule's page as a form for a
  fix to what it says, a proposal of [changes.md](changes.md)'s kind
  `says`, which carries the finding, its check, kind, message and detail,
  as its evidence. Lapilli writes no text of its own: a person, or an
  agent, does, and a person approves it.
- **`lapilli verdict --check-content`** needs no history for `--rules`;
  with `--prometheus` it replays the rules over the period for the labels
  their alerts had, and with `--incidents` or `--impact` judges them for
  which are noisy. It exits 0 whatever it finds, and 1 when the replay ran
  out of `--timeout`.
- **What takes input from outside,** as an attacker would read it: what a
  rule says comes from whoever writes the rules, or a cloud's API. Each
  text is cut to 4 KB and a finding's detail to 200 bytes, with control
  characters and those that turn text around dropped. Templates are parsed,
  never executed; no link is fetched. Pages escape it all, and show a
  runbook as a link only when it is an http or https URL with nothing a
  template fills in, with `rel="noopener noreferrer"`; `/metrics` escapes
  the rule's name, and its checks are a fixed list; the terminal gets no
  control characters; MCP's instructions tell agents that what rules say,
  and the details found in it, are data, never instructions. The line a
  report gets has counts and the checks' own words, nothing from the
  rules.

## Alertmanager's templates

Status: built (0.70); the chart's `serve.checkContent.alertmanager` and
`lapilli preflight`'s `content.alertmanager`, for the next release.
[serveops.md](serveops.md) checks the notification templates a Grafana
rule is sent with. The other rules,
Prometheus's, a ruler's and Loki's, are sent by Alertmanager, whose
receivers' fields are templates too. This checks those the same way.

### What it reads

`--alertmanager-config`, for `lapilli verdict --check-content` and
`lapilli serve`:

- **A file:** `alertmanager.yml`, and the template files its `templates:`
  globs name, resolved against the file's directory, as Alertmanager
  resolves them. An absolute glob is Alertmanager's own path, not
  Lapilli's: `/etc/alertmanager/templates/*.tmpl` is looked for by its last
  element, `*.tmpl`, beside the file, where a mounted Secret or ConfigMap
  puts the templates. No file outside the file's directory, after its
  links, is read: a glob that leaves it (`../*`), or a link out of it, is
  passed over and said. A file in Mimir's, Cortex's or Amazon's format
  (`alertmanager_config` and `template_files`) is read as they keep it.
- **Mimir's or Cortex's API:** `GET /api/v1/alerts`, with the ruler's
  token (`--ruler-token-file`) and tenant (`--ruler-tenant`, as
  `X-Scope-OrgID`). It gives the configuration and every template file;
  Mimir loads them all, whatever `templates:` says, so all are read. A
  404 is a tenant with none. With `--ruler` and its token, the URL must be
  on `--ruler`'s scheme and host, or the flags are refused: the token is
  the ruler's, and a typo, or a plain Alertmanager's URL, mustn't be sent
  it. Without `--ruler`, the token is given for this alone.

Only what routing and the templates need is read: the route tree, each
receiver's name and its integrations' fields that hold `{{`, and the
template files. No field whose name says it is a secret or a connection's
(a password, a token, a key, a URL, a file, `http_config`, `tls_config`) is
parsed: none is a template a person reads, and what they hold isn't
Lapilli's to see.

### Which templates

The route tree is Grafana's policy tree, and is routed by the same code:
each route's `matchers` (a line may hold several, in braces or not,
quoted or not), `match` and `match_re`; `continue`; and the receiver a
route inherits from its parent. A rule's alerts are known to have
`alertname`, its labels and its group's; a label set as a template is
filled in from the series, and isn't known. A route matching a label the
rule doesn't set goes both ways, and a template counts only when every way
reaches a receiver that uses it.

A receiver uses each of its fields that is a template, the templates that
field names, and those they name in turn. A field an integration doesn't
set is sent with Alertmanager's default, `{{ template "slack.default.title"
. }}`: a template file that defines that name in its place is used. The
defaults themselves aren't followed, as Grafana's aren't: they show every
label and annotation there is, and fault nothing.

### What is checked

As Grafana's are, with the template's name in the detail (a field's is
`RECEIVER/slack_configs[0].title`):

| Check | Kind | When |
|---|---|---|
| A notification template shows an annotation the rule hasn't | fact | `.Annotations.x` or `.CommonAnnotations.x`, unguarded, and the rule has no `x`: any annotation, where the rule's are all known; a rule whose runbook is in `runbook` hasn't `runbook_url` |
| A notification template that doesn't parse | fact | a receiver's field, or a template a template file defines, that the rule is sent with |
| A notification template names a label the alert never had | measured | as Grafana's |
| A notification template names a label the expression can't give | heuristic | as Grafana's; not of a Loki ruler's rule |

A template that doesn't parse is a fact here and not for Grafana: Grafana
refuses one when it is saved. Alertmanager parses a receiver's field only
when it sends with it, and then sends nothing; `amtool check-config`
doesn't catch it. A template file that doesn't parse stops Alertmanager
loading the configuration at all: that is said once, on stderr and in the
log, and a rule sent with a name the file defines (as its text says) is
told so too.

### Bounds

The configuration, or Mimir's answer, is 8 MB at most, parsed as rule
files are, what its aliases make bounded. 256 template files at most, each
256 KB, 8 MB in all; 5,000 receivers; 20,000 fields that are templates,
the defaults a file defines in Alertmanager's place counted with them, 200
of a kind, each followed once a receiver; a field over 64 KB isn't read;
routes as Grafana's, 2,000 and 32 levels, 20,000 matchers in all. A glob
that leaves the configuration's directory isn't looked through, a glob's
first 1,024 files at most are, and the links out of it are counted, not
named. The route tree is read as Alertmanager reads it, with yaml.v2: a
value as the text it is written in (`1.10`, not 1.1), `continue: yes` true;
a matcher's `\.` kept, as Alertmanager keeps a backslash before anything
but `"`, `\\` and `n`. Past each, what is
past is passed over, and said. Routing has its own budget of work and
time, as Grafana's has. The request to Mimir goes to its host only,
follows no redirect, and is a read.

### When it is read

`lapilli verdict` reads it once; one that can't be read is an error, as
it is read for this alone. `serve` reads it once an hour, as Grafana's
templates; one that can't be read leaves the check as it was, and the log
says why, once for each why. What is passed over is said the same way.

### Not done

- **Grafana rules sent to an external Alertmanager.** Grafana's own
  routing is read with `--grafana`; a Grafana that sends to an
  Alertmanager instead isn't followed there.
- **A name no template defines.** `{{ template "x" . }}` with no `x` fails
  when Alertmanager sends, but Alertmanager's defaults, which differ by
  version, define many names this doesn't know: it isn't a check.
- **Alertmanager's API** (`/api/v2/status`) gives the configuration
  without its template files, and with secrets hidden: a rule's templates
  can't be followed from it.
- **`.CommonAnnotations` of a group whose alerts differ.** A summary
  templated by an alert's labels differs from one alert to the next, and
  is then not common: it shows nothing. Guessing it needs the group's
  alerts.

## A range over a selector that names no job

Status: built, for the next release. [serveops.md](serveops.md) counts a
range over a selector that names its job with that job's own scrape
interval. A selector that names none, the most of them, was counted with
the global interval: `rate(batch_runs_total[3m])` of a job scraped every
2 minutes holds one point, and wasn't faulted under a global 30 seconds.

### What it does

For each range over a selector that names no job, Lapilli asks the
history which jobs the selector's series come from: `count by (job)
(SELECTOR)` over the ten minutes before now, a point every five, as one
range query. The range is counted with the longest interval of those
jobs, since it must hold two points of each, and the finding names the
job, as for a selector that names it. A job Prometheus's configuration
doesn't have counts with the global interval.

- **Only where it can matter.** Not asked with `--scrape-interval`, nor
  when every job has one interval, nor when no job's is known (most
  managed services), nor of a range that holds two points of the longest
  interval known: the answer would change nothing. Nor of a Loki ruler's
  rule, whose ranges are of log lines.
- **The selector as one plain selector.** The query is written from the
  expression's tokens: a metric's name, and matchers each a name, an
  operator and a string, quoted again. A selector of any other shape
  isn't asked of: the query can be nothing but `count by (job)` of the
  selector the rule already reads. And only of one metric, named before
  the braces or by `__name__="..."`: a regular expression on `__name__`
  could be every series there is.
- **Bounded in time.** Thirty seconds in all each time, ten a query, in
  `lapilli verdict` as in `serve`.
- **Each once.** Asked once a run in `lapilli verdict`; kept a day in
  `serve`, asked again by the worker, as the rules are read, not by a
  page.
- **Bounded.** 200 selectors at most each time: in `serve` the rest at
  the next look. A thousand jobs at most a selector.

### When the query fails

The range is counted as before: with the global interval, or the one
every job has. `lapilli verdict` says on stderr how many weren't asked;
`serve` asks a failed one again an hour later, and the log says how many
failed. A selector with no series in the ten minutes (a counter only an
error makes) gives no job, and is counted as before too; `serve` asks it
again an hour later.

### Not done

- **The metric's targets** (`/api/v1/targets/metadata`), as
  [serveops.md](serveops.md) weighed: it knows nothing of recorded series,
  and isn't served by most stores that serve queries.
- **A series recorded by a recording rule** keeps the job it was recorded
  from, and is counted with that job's scrape interval, not its rule's
  evaluation interval, as before.

