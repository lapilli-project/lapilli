# Pull request backtests in CI

On a pull request that changes alert rules, the backtest can comment its
report on the pull request, and edit that comment on each push: what fired
before the change and what fires after, rule by rule, over the period you
name. [recipes/backtest](../recipes/backtest) has a script for each code host,
GitHub, GitLab, Bitbucket, Azure DevOps and Gitea (or Forgejo), one
library under them; and, for GitHub Actions, an action:

```yaml
on:
  pull_request:
    paths: ['alerts/**']
permissions:
  contents: read
  pull-requests: write
jobs:
  backtest:
    if: github.event.pull_request.head.repo.full_name == github.repository # never a fork's
    runs-on: ubuntu-latest # one that can reach your Prometheus
    steps:
      - uses: actions/checkout@v4
        with: {fetch-depth: 2, persist-credentials: false}
      # Pinned by the commit of the release's tag, which a tag moved can't
      # change; the action installs that release of lapilli.
      - uses: lapilli-project/lapilli/recipes/backtest@COMMIT-OF-THE-vX.Y.Z-TAG # vX.Y.Z
        with:
          paths: alerts
          prometheus-url: ${{ vars.PROMETHEUS_URL }}
          since: 14d
```

Each installs a pinned release, checked against its `SHA256SUMS` (the
action, the release it is at, which Dependabot keeps up to date with the
`uses:` line); reads the
rules of the pull request and of the commit it goes into, as files
(`paths`) or as a command renders them in each (`render: helm template
monitoring charts/monitoring`), and the incidents from the commit it goes
into, never the pull request's (`incidents`), and the times a PromQL
expression says users were hurt (`impact`, `match: service`); runs `lapilli verdict
--compare --changed-only --markdown`; and posts one comment, found again by
a hidden first line. When nothing that decides when a rule fires changed
and there is no comment yet, it posts none. When something fails, the comment says
what, and the job succeeds unless it is asked to fail. The recipes' README
has each CI's job, and what to keep from pull requests from forks: their
rules become queries sent with your credentials, and a render command runs
what they hold.

`--markdown` writes the report for a pull request or a merge request, at
most 64,000 bytes, under the 65,536 characters GitHub takes, cut short
with a note when longer:

```
[//]: # (lapilli-compare {"nothing_changed":false,"changed":1,"added":1,"removed":0,"same":42,"alike":0,"unreplayed":0,"recording_changed":0,"firings_before":148,"firings_after":42,"incidents_given":true,"incidents":3,"incidents_changed":1,"no_record":false})
### Lapilli backtest, 2026-10-26 00:00 → 2026-11-02 00:00 Asia/Seoul (+09:00)

Rules compared: **1 changed, 1 added, 0 taken away**, 42 the same.

| | Before | After |
|---|--:|--:|
| Firings | 148 | 42 |
| Severity `critical` | 125 | 16 |
| Severity `warning` | 23 | 26 |
| Incidents with no alert, of 3 | 1 | 0 |

#### Rules that differ

| Rule | Severity | Fired | ○ Caught | ● Nothing | Precision | Refired |
| --- | --- | --: | --: | --: | --: | --: |
| `ExportJob5xx` | `critical` | 118 → 9 | 0 → 0 | 118 → 9 | 0% → 0% | 41 → 2 |
| `DiskFillingUp` | – → `warning` | – → 3 | – → 1 | – → 2 | – → 33% | – → 0 |

#### Incidents caught differently

| Incident | Start | Before | After |
| --- | --- | --- | --- |
| `INC-43` `Batch stuck` | 2026-10-31 02:40 | ✗ nothing fired | `DiskFillingUp` +4m |
```

When rules that differ couldn't be replayed, a line under the totals says
how many, as `unreplayed` counts them, since the numbers leave them out;
they are listed, with why, under the table. Those that differ but fire as
they did are folded in `<details>`.

A recording rule that differs is a change too, listed apart: a replay reads
what a recording rule recorded, from the history, so the alerts that read
one are replayed against what it recorded, not what the change would
record, which the report says. Recording rules are read wherever
Prometheus's groups are (files, a `PrometheusRule`, a ConfigMap, Google
Cloud's `Rules`, an Amazon rule groups namespace, a Mimir, Cortex or Loki
ruler's file), from an Azure rule group, alone or in an ARM template, and
from a Grafana provisioning file, where a recording rule (one with
`record`) is no alert rule. A cloud's own alarms record nothing.

A group's `labels` and `limit` are part of each of its rules, as
Prometheus 3 runs them: its alerts carry the group's labels under the
rule's own, and a change to either is a change to the rule. A limit isn't
replayed: past it, Prometheus drops all of a rule's alerts, which the
report says too. A rule's name, and in `lapilli serve` its versions,
history and proposals, are as they were before Lapilli read them: a rule
told apart from another of its name is told apart by its group and its own
labels. Not every server that takes Prometheus's groups applies these:
Prometheus 2 refuses a group's `labels`; Google Cloud's `Rules`,
`ClusterRules` and `GlobalRules` have neither field; and a Mimir, Cortex or
Amazon ruler may keep them without applying them, depending on its
version. Where they aren't applied, the replay's alerts carry labels the
real ones don't.

Rule
names, labels, incidents and the reasons a rule couldn't be replayed come
from whoever wrote the rules, and a pull request's author may be anyone:
each is written as a code span, in which no host reads Markdown, HTML, a
mention (`@user`, `@org/team`), a reference (`#123`) or a link, a `|`
escaped so it can't end a table's cell, a backtick in it making the span's
fence longer, and control and format characters (bidirectional overrides,
zero-width spaces), line separators and spaces other than the space shown
as `\u{...}`. A query that failed as the history couldn't be read (as
`--fail-unreplayed` counts it: anything but the history's answer to the
query as written) says only that in the Markdown, its whole reason going to
stderr, the job's log; what is left, the history's own words on a query it
refused, has where the history and the rules are read from taken out: the
`--prometheus`, `--loki`, `--grafana` and `--ruler` URLs, the user names
in them and their hosts, each as a whole word, any IP address and port,
the tenants and a `--gcp-project`.

What a tool reads is stable:

- The report of `--json` and `/api/v1/report`, the columns of `--csv`, and
  the names and labels of `--metrics` and `/metrics`: those
  [docs/numbers.md](numbers.md) lists keep their names and meaning; a
  minor release that must change one says so in the changelog and writes
  the old beside the new for a release first.
- The first line of `--markdown` is a link reference definition, which no
  host shows, holding the summary as JSON; `--json` gives the same object
  as `"summary"`, beside the rest of the report, whose fields are as they
  were. Every field is always there:

  | Field | |
  |---|---|
  | `nothing_changed` | nothing that decides when a rule fires differs, a recording rule included: nothing was replayed |
  | `changed`, `added`, `removed`, `same` | rules in both that differ, only in `--rules`, only in `--compare`, alike in both |
  | `alike` | rules that differ but, replayed on both sides, fire as often and catch as much as they did |
  | `unreplayed` | rules that differ that couldn't be replayed, on one side or both, for any reason: those of `--json`'s `"unreplayed"` list with a side, each counted once |
  | `recording_changed` | recording rules that differ, listed as `"recording"`: what the alerts that read them see, which no replay shows |
  | `firings_before`, `firings_after` | firings on each side; of the rules that differ alone with `--changed-only` |
  | `incidents_given` | incidents were given, `--incidents` or `--impact`; when false, nothing was judged: no firing caught anything, and `incidents` is 0. The firings were judged only when it is true and `no_record` false |
  | `incidents`, `incidents_changed` | incidents in the period, and those caught differently |
  | `no_record` | incidents were given, and none was on in the period or the two weeks before it (nor `--impact` read): nothing was judged, as without them |
  | `expected` | the rules that differ were held to expectations, `--expect` |
  | `expectations_broken` | expectations the change broke: met by the rules in place (or of a rule it adds), broken by those proposed |
  | `expectations_already_broken` | expectations broken on both sides, which aren't the change's |
  | `unexpected` | rules the change adds that no expectation holds |

- When nothing changed, `--markdown` writes that line and exactly:

  ```
  ### Lapilli backtest: nothing changed

  Nothing that decides when the 42 rules fire has changed: nothing to replay.
  ```

- The exit status is `lapilli verdict`'s, as it was: 0 when the report is
  written, whether or not anything changed; 1 when it failed, or a rule's
  replay didn't finish within `--timeout` (the report is still written),
  or, with `--fail-unreplayed`, the history couldn't be read for one or
  its labels on its series were too many to replay, or, with
  `--fail-expectations`, the change broke an expectation (and with
  `--expect-required`, it added a rule none holds); 2
  when the flags are wrong. Nothing changing has no exit status of its
  own: a CI step takes anything but 0 as a failure, and a pull request that
  changes no rule isn't one. Read `nothing_changed`.

The header names the time zone of its times with each offset it took in
the period, `Asia/Seoul (+09:00)`, `Europe/Berlin (+01:00 → +02:00)`, or
`UTC` by any of its names: `--tz` gives one by its IANA name, and without
it, the machine's own is named as `TZ` or `/etc/localtime` says, through
every link. A zone file copied rather than linked has no name: its
abbreviations are said, `CEST (+02:00) → CET (+01:00)`.
