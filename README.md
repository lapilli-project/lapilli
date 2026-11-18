<picture>
  <source media="(prefers-color-scheme: dark)" srcset="assets/lapilli-lockup-dark.png">
  <img alt="Lapilli" src="assets/lapilli-lockup.png" width="300">
</picture>

[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/16891/badge)](https://www.bestpractices.dev/projects/16891)

**Which of your alerts are worth their page?** `lapilli verdict` judges every
firing against what actually went wrong: a white stone when the alert caught an
incident, a black stone when nothing was wrong. Count a rule's stones over a
week and you know whether it earns its place; an incident with no stone is one
your alerts missed. `lapilli serve` keeps judging, every hour, in a ledger with
a dashboard, where teams follow the noisy rules and missed incidents up
([Keep judging](docs/serve.md)). It reads by default; turned on, it also makes the
changes it proposes, each once a person has approved it, as a pull request
to where the rule is kept ([Changing rules](docs/changing-rules.md)).

```
$ lapilli verdict --rules alert-rules.yaml --prometheus http://prometheus:9090 \
    --incidents incidents.yaml --impact 'sum(rate(http_5xx[5m])) / sum(rate(http_total[5m])) > 0.01'
Lapilli  2026-10-26 00:00 → 2026-11-02 00:00 Asia/Seoul (+09:00)
148 firings, 3 incidents written down, 1 more found by --impact

RULE                SEVERITY  FIRED  ○ CAUGHT  ● NOTHING  PRECISION  REFIRED
ExportJob5xx        critical  118    0         118        0%         41
CheckoutLatencyP99  warning   14     2         12         14%        3
ReadReplicaLag      warning   9      1         8          11%        0
NodeNotReady        critical  4      0         4          0%         0
DBPrimaryCPU        critical  3      2         1          67%        0

Noisy rules that a longer "for" would quiet, still catching every incident they caught:
RULE                FOR       FIRED     INCIDENTS  CAUGHT LATER BY
ExportJob5xx        5m → 15m  118 → 31  0          –
CheckoutLatencyP99  5m → 10m  14 → 6    1          up to 4m

What each rule adds: the incidents it caught, those it caught before any other rule, those no other rule caught:
RULE                INCIDENTS  FIRST  ALONE
CheckoutLatencyP99  1          0      0
DBPrimaryCPU        1          1      1
ReadReplicaLag      1          1      1
A noisy rule's catch doesn't count beside one by a rule that isn't noisy.

38 rules never fired.

INCIDENT                         START             CAUGHT BY
INC-41 Checkout errors           2026-10-28 12:01  DBPrimaryCPU +2m, CheckoutLatencyP99 +6m (noisy)
INC-42 Search timeouts           2026-10-30 09:15  ReadReplicaLag -3m
INC-43 Batch stuck               2026-10-31 02:40  ✗ nothing fired
impact-1 user impact (--impact)  2026-11-01 21:27  ✗ nothing fired

✗ 2 of 4 incidents had no alert.
```

> Status: early, with one maintainer and room for more
> ([MAINTAINERS.md](MAINTAINERS.md)). Checked against one production Grafana:
> all 71 of its alert rules replay, and a four-week replay of the rules as
> they stood then found 177 firings of the noisiest rule, the number of
> notifications its team had counted by hand in their alert channels. Weekly
> counts of critical alerts came within 10% of theirs in three of the four
> weeks.

## Why

Alert rules pile up. One is added after each incident, few are taken away, and
nobody can say which ones still earn their page. The way teams find out is by
hand: read a week of alert channels next to the incident notes, mark each alert
as useful or noise, and look for incidents that nothing caught. It is slow, so
it rarely happens, and a rule that has paged for nothing a hundred times keeps
paging.

Lapilli does that reading. It works from what you already have: the rules, the
history in Prometheus, and whatever record of incidents you keep, even a thin
one.

## The name

> *mos erat antiquus niveis atrisque lapillis,*<br>
> *his damnare reos, illis absolvere culpa*
>
> It was the old custom to judge with white and black pebbles: the black to
> condemn the accused, the white to acquit them.
>
> — Ovid, *Metamorphoses* XV.41–42

*Lapilli* are small stones. Each firing is a vote; the urn is emptied once a
week and the stones are counted.

## Docs

Each subject has a page in [docs/](docs/README.md), the same pages as
[lapilli.dev/docs](https://lapilli.dev/docs/) and the
[wiki](https://github.com/lapilli-project/lapilli/wiki):

- Start here: [Getting started](docs/getting-started.md),
  [Words](docs/glossary.md), [Questions](docs/faq.md)
- Judging a period: [How it judges](docs/how-it-judges.md),
  [Use `lapilli verdict`](docs/verdict.md), [The numbers](docs/numbers.md),
  [A cloud's own alarms](docs/cloud-alarms.md),
  [Rules on logs: Loki](docs/loki.md),
  [Is the replay right?](docs/check-replay.md),
  [What an alert says](docs/check-content.md)
- Before a change: [Before a change is merged](docs/compare.md),
  [Pull request backtests in CI](docs/pr-backtests.md)
- Incidents: [Incidents kept elsewhere](docs/incidents.md),
  [Incidents nobody wrote down](docs/impact.md)
- Reports: [Every week](docs/every-week.md),
  [Showing a report outside](docs/redact.md)
- Every hour, `lapilli serve`: [Keep judging](docs/serve.md),
  [Notifications as they were sent](docs/notifications.md),
  [Changing rules](docs/changing-rules.md),
  [Ask it from an agent](docs/mcp.md)
- Reference: [Commands](docs/commands.md), [The HTTP API](docs/api.md),
  [Metrics](docs/metrics.md), [Upgrading](docs/upgrading.md)
- [When something goes wrong](docs/troubleshooting.md),
  [What it doesn't do yet](docs/limits.md)

## Help build it

Lapilli has one maintainer and is looking for more, from more than one
organisation. [ROADMAP.md](ROADMAP.md) lists what it doesn't do yet, area by
area, with where each lives in the code. Several areas need what the current
maintainer doesn't have: an AWS or Azure workspace, Loki alerts, an incident
tool. To start small, an issue labelled
[good first issue](https://github.com/lapilli-project/lapilli/labels/good%20first%20issue) says where its change goes
and when it's done; one labelled
[help wanted](https://github.com/lapilli-project/lapilli/labels/help%20wanted) needs an account or a language
the maintainer doesn't have. [MAINTAINERS.md](MAINTAINERS.md) says how contributing in an area leads
to reviewing and maintaining it, [GOVERNANCE.md](GOVERNANCE.md) how decisions
are made, and [CONTRIBUTING.md](CONTRIBUTING.md) how to build, test and send a
change. The most useful report there is: a rule that replays differently from
how it really fired, which `--check-replay` finds.

Questions, ideas and what Lapilli found in your alerts go to
[Discussions](https://github.com/lapilli-project/lapilli/discussions): Q&A
for how to point it at your rules or read a verdict, Ideas for what it
should judge or read next, Show and tell for a report, which `--redact`
makes safe to show. A replay that differs, or anything else wrong, is an
[issue](https://github.com/lapilli-project/lapilli/issues/new/choose), with
a form for each; a vulnerability is reported privately (SECURITY.md).
Everyone taking part follows the [Code of Conduct](CODE_OF_CONDUCT.md).

## Install

```sh
go install lapilli.dev/cmd/lapilli@latest
```

or take a binary from the [releases](https://github.com/lapilli-project/lapilli/releases),
or the image `ghcr.io/lapilli-project/lapilli`. Each release's provenance is
attested, and, from v0.28.0 on, kept beside it as
`lapilli-vX.Y.Z.intoto.jsonl`, the seventh of its assets beside the four
archives, the chart and `SHA256SUMS`; SECURITY.md says how to check it,
offline too.

## License

Apache 2.0. The UI's font, Heebo, is under the SIL Open Font License 1.1
([internal/serve/static/OFL-Heebo.txt](internal/serve/static/OFL-Heebo.txt)).
