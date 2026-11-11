# What an alert says, checked

Status: proposed, for v0.22.0. It reads what Lapilli doesn't read today,
and shows it on every page a rule has, so it is written down first, under
[GOVERNANCE.md](../../GOVERNANCE.md). The companion to [serve.md](serve.md),
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
