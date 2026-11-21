# Is the replay right?

Every verdict rests on the replay, so check it on your own rules. With
`--check-replay`, `lapilli verdict` replays the rules and sets them beside the
series that say what really fired, instead of judging. It needs no incidents.

```
$ lapilli verdict --rules alert-rules.yaml --prometheus http://prometheus:9090 --check-replay
Replay beside ALERTS{alertstate="firing"}  2026-10-26 00:00 → 2026-11-02 00:00 Asia/Seoul (+09:00)
139 firings alike, 2 only in the replay, 7 only in what really fired: 94% alike.
21 rules fired alike, 2 differ, 38 fired neither way.

RULE          REPLAYED  REAL  ALIKE  ONLY REPLAYED  ONLY REAL  OFF BY
ExportJob5xx  118       122   116    2              6          30s
QueueDepth    9         10    9      0              1          1m0s

Really fired, but no rule given has the name: Watchdog.
```

Two firings are alike when they are of the same alert, agree on the labels both
have, and start within two evaluations of each other, since Prometheus places a
group anywhere in its interval. Labels a store adds to the series are passed
over, and so is a rule's label with a template in it. A firing with no match too
near the end of the period to tell isn't counted. A group's `interval` is taken
from the rules; a rule file that leaves it to the global `evaluation_interval`
doesn't say, so give that as `--step` if it isn't 1m.

What really fired comes from a series, or from Grafana's own history:

| Rules evaluated by | What really fired | |
|---|---|---|
| Prometheus | `ALERTS{alertstate="firing"}` | [documented](https://prometheus.io/docs/prometheus/latest/configuration/alerting_rules/) |
| Amazon Managed Service for Prometheus | `ALERTS{alertstate="firing"}` | [documented](https://docs.aws.amazon.com/prometheus/latest/userguide/troubleshoot-rule-evaluations.html) |
| Grafana, with `--grafana` or `--check-grafana` | Grafana's own [alert state history](https://grafana.com/docs/grafana/latest/alerting/set-up/configure-alert-state-history/), read through its API | kept in annotations, its default, from Grafana 12.4 on, or in Loki. Kept in Prometheus too, the series below while it holds anything |
| Grafana | `GRAFANA_ALERTS{alertstate="firing",grafana_alertstate!="error"}` | once state history is written to Prometheus. Its error alerts are left out: a query that failed can't be replayed |
| vmalert, writing to VictoriaMetrics (`-remoteWrite.url`) | `ALERTS{alertstate="firing"}` | [documented](https://docs.victoriametrics.com/victoriametrics/vmalert/); checked in CI (`e2e-compat`) |
| Thanos, over a Prometheus's sidecar | that Prometheus's `ALERTS{alertstate="firing"}` | checked in CI (`e2e-compat`) |
| Mimir, Azure Monitor, Google Cloud | ask for `ALERTS` and see | their docs don't say; `--check-series` names another series |

Grafana keeps its alert state history in annotations unless told
otherwise, and writes `GRAFANA_ALERTS` only when told to, so most
Grafanas have no series to check against. Given the Grafana, Lapilli
reads the history it keeps instead, with the token `--grafana-token-file`
holds, a Viewer's: `--grafana` for the rules it reads from there, and
`--check-grafana URL` for Grafana's rules read from files.

```
$ lapilli verdict --rules grafana-alerting/ --prometheus http://prometheus:9090 \
    --check-replay --check-grafana https://grafana.example.com --grafana-token-file token
Replay beside Grafana's alert state history (annotations)  2026-11-09 00:00 → 2026-11-16 00:00 UTC
```

The history is read from a day before the period, as Grafana keeps
changes of state, not states: an alert on since before the period is
seen as on at its start, and one that fired through all of it, changing
state in none of it, isn't seen. Its error alerts are left out and said,
as with the series. Annotations keep an alert's labels as Grafana writes
them as tags, a colon an underscore (`instance:10.0.0.1_9100`), spaces
trimmed, a label past 100 characters or past `[annotations] tags_length`
left out; the replay's are set beside them so. Grafana's rules aren't
checked, and the others are, when the history can't be read, holds no
change from before the period (it may keep none that far back: shorten
it), or tells a rule's alerts apart by nothing it keeps; each says why.
On Grafana 12.4 and 13.3, every firing read from the history was alike
with `GRAFANA_ALERTS`, starting and ending at the same evaluation
([docs/design/grafanahistory.md](design/grafanahistory.md)).
`--check-series` reads series instead.

When the series hold nothing for the period, or no rule could be replayed, it
says so and exits 1, since nothing was checked. If a rule replays differently from how it
really fired, please [report it](../CONTRIBUTING.md): it is the most useful report
there is.

`lapilli serve` does this itself, as it goes
([docs/design/fidelity.md](design/fidelity.md)). Each day the runs
won't replay again is set beside the same series, each firing counted on the
day it started, and kept. A rule's page says, over twelve weeks, how many
were alike, how many only in the replay and how many only in what really
fired, and lists the last of those; the rules list and the dashboard mark a
rule whose replay differed over the last week (five firings either way, fewer
than nine in ten alike, unless `--check-differs N/RATIO` says otherwise), and
`replay.differs` tells the webhook and Slack once a week while it does. The
Run history page says how far it has got, which series held nothing (a
store may not
keep `ALERTS`; `--check-series` names another), whether Grafana's own
history stands in for `GRAFANA_ALERTS`, a day it held nothing from before
it (Grafana may keep none that far back), more than is read at once, or
what couldn't be used, when Grafana's rules aren't checked that day and
the check goes on, and what really fired with no
rule given by that name. `/metrics` has `lapilli_rule_replay_check{rule,
way}` over seven days, MCP's `rules` and `rule` carry it, and the weekly
digest has a line of it. No stone changes. A cloud's own PromQL alarms
(CloudWatch, Google Cloud, Azure) keep no such series: read with `--alarms
SOURCE=URL`, their history copied in under `SOURCE` stands in, by each
firing's `alarm_id`, a day checked once the copies reach past it (waiting
two days at most); a CloudWatch alarm's replay is first put together as
CloudWatch keeps the alarm, as a whole. `lapilli verdict --check-replay`
takes the same from a recipe's output with `--check-firings FILE`. The
replay checked is the one judged, with `--late` alike: an alarm replayed
without it reads its metrics as filled in since, where the alarm saw them as
they arrived, and may differ for that. Rules that share an alert name
are counted together, as what really fired doesn't tell them apart. The
series are trusted as they are: a scraped target that exposes its own
`ALERTS` would be counted. It is on whenever there are rules
to replay; `--check-replay=false` turns it off, and so does the chart's
`serve.checkReplay.enabled`.
