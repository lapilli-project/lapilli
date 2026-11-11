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
  the range check is off.
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
  an empty description is "no summary or description". A source with no
  severity at all (CloudWatch, Azure's activity log alerts) isn't faulted.
  A severity label is matched to `--severities` exactly, as an alert
  manager's routes match it; a cloud's own field, in any case. A runbook a
  template starts (`{{ $externalURL }}/...`) isn't judged. Two rules of
  one name and condition are one alert to an alert manager, so only rules
  of different names are the same condition; a rule is told of three at
  most. A rule turned off or paused fires nothing, and isn't checked.
- **Templates.** A function this version doesn't stub is taken as one a
  newer Prometheus or Grafana has, and the text parsed again (sixteen at
  most), so only what can't be parsed whatever the functions is an error.
  Google Cloud's text is checked for a `${` left open or empty. A template
  names a label as `$labels.x`, `index $labels "x"`, or `.Labels.x` where
  `.` is still the alert. A rule's own labels and `alertname` are never
  taken as missing.
- **Measured** labels are those of the replayed firings, which carry the
  rule's own labels too: a template naming one is never faulted, though
  Prometheus leaves it out of `$labels`. That errs towards saying nothing.
- **Heuristics.** "A label the expression can't give" is guessed only of an
  expression that is one aggregation that keeps the labels it is grouped
  by (`sum`, `min`, `max`, `avg`, `count`, `group`, `stddev`, `stdvar`,
  `quantile`), by labels, alone or compared to a number; of any other
  shape, nothing is guessed. Expressions are read by their tokens, not a
  full PromQL parse, and those over 64 KB not at all.
- **The scrape interval** is asked of Prometheus by the worker, once a day
  when it reads the rules, not by a page; a configuration with no
  `global.scrape_interval` is Prometheus's default, 1m.
- **Reports and proposals.** The reports' `content` section is built with
  [reports.md](reports.md): this gives it one line, the problems by check
  and the guesses apart, of the rules a report covers. The proposals of
  [changes.md](changes.md) read the findings by rule, each with its check,
  kind and message.
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
