# Grafana's rules Lapilli can't replay, judged from Grafana's own history

Status: built, on the branch after v0.67.0. It changes which firings a
rule that can't be replayed has, not how a firing is judged, and keeps
nothing new in the ledger (no schema), so it is written down beside the
build, as one pull request. It reads with the reader of
[grafanahistory.md](grafanahistory.md), and keeps what it reads as
[recorded.md](recorded.md) keeps firings copied in.

## Why

A Grafana rule whose condition reads a data source that is neither PromQL
nor LogQL (MySQL, PostgreSQL, Elasticsearch, InfluxDB, CloudWatch, Azure
Monitor, Grafana's TestData) can't be replayed: a database keeps no
history of what a query returned. Such a rule was listed with the reason
and judged by nothing: no stones, no precision, and in `lapilli serve` no
record, so it was never noisy, never caught anything and never missed.
But Grafana keeps what it fired: its alert state history, which
`--check-replay` already reads, and whose every firing, in the recordings
kept as test data, was one `GRAFANA_ALERTS` had, starting and ending at the
same evaluation. What a cloud's alarm that isn't PromQL gets from its
history (recorded.md), such a rule can get from Grafana's.

## What was built

- **Which rules.** Read with `--grafana`, a rule that can't be replayed for
  a reason of its own (`rules.Rule.FromHistory`): its condition's data
  source, or a way of writing the replay doesn't take. Not one paused,
  which fires nothing, nor one a replay gave no answer for, which another
  run may replay. Rules read from provisioning files aren't:
  `--check-grafana` names a Grafana for the check alone.
- **What is read.** The history as `--check-replay` reads it
  (`source.GrafanaHistory.Judge`), where Grafana serves one (annotations,
  its default from 12.4 on, or Loki), with Lapilli's own token. An
  annotation's tags name a rule by its title as `AnnotationName` writes
  it; the firings are given the name the rule is judged under. A name two
  of Grafana's rules share (one title in two folders) is neither's; a rule
  some change of which the history passed over, or whose alerts it doesn't
  tell apart, isn't judged by it.
- **`lapilli verdict`.** After the replay, the history is read from a day
  before the period, once it is known to hold a change from before its
  start, and the rules' firings are judged with the replayed ones. Each
  keeps its reason, as it isn't replayed, and gains the source `grafana`:
  the report counts it in its table, says under it that it was judged from
  the history, and `--json` has `source`. Where the history can't be read,
  holds nothing from before the period, or names the rule apart from none,
  the rule is listed as before, and stderr says why.
- **`lapilli serve`.** Each round, after the runs and the check, the
  history from where the copies got to, up to the runs' cursor, is copied
  in under the source `grafana`, a day a write, seven a round: the first
  read from a day before it, the rest from the `--settle` before their
  start, written again, so a change Grafana kept late is seen. Before the
  first, a day the history holds no change up to (it begins after it,
  kept so far back no more, or not yet) is passed over, not taken for a
  quiet one. An alert on
  since before a write, which the ledger has as on and the history says
  ended, is written again from its start with what the ledger has from
  there, within 31 days and 100,000 firings. The firings are kept as
  copied-in ones are: rows of `firings` under a recorded rule of the
  source. The rule and its history are one rule, tied by name, as a
  cloud's alarm and its history are ([alarms.md](alarms.md)): the names of
  the rules judged so are kept in the ledger's meta (`grafana.judged`)
  before each run, and each run, version and copy ties them. Its page says
  so, and its record is what the copies cover.
- **One flag.** `--grafana-history=false`, for both commands, lists them as
  before.

A tie made by a copy now carries the reason the run gave the rule, and its
group, from the empty row the run made for it: before, a cloud's alarm read
as replayed until the next run wrote them again.

## What isn't built

- **No backtest.** What needs the condition stays with replayed rules:
  change reviews and their outcomes, a longer `for`, near misses,
  `--compare`, drafts of what a rule is held to. Lapilli proposes no `for`
  or threshold for such a rule, as for a cloud's alarm that isn't PromQL.
- **Error alerts** aren't counted, as `GRAFANA_ALERTS{grafana_alertstate!="error"}`
  leaves them out.
- **What the history can't see** (grafanahistory.md): an alert that fired
  through all of what was read, changing state in none of it (in serve the
  first day alone, as later days carry the ledger's own); labels as an
  annotation's tags keep them (a colon an underscore, a label past 100
  characters left out), which `--match` and an incident's linked alerts
  are set beside.
- A rule deleted from Grafana while an alert of it is on keeps that firing
  on.

## Attacker's look

This reads what another system sends, the Grafana, and through it what
its data sources answered.

- **How much.** The reader's bounds, unchanged: 16 MiB an answer, 5,000
  requests, 256 MiB of answers, 200,000 changes of 50,000 alerts, their
  labels 64 MiB. A day past any is passed over, logged, and the copy goes
  on from the day after; so is a day of more than 100,000 firings, which
  no write holds. Only a failure worth asking again fails the round.
- **What is kept.** A firing a write can't hold (more than 32 labels,
  labels past 4 KB, a name that doesn't show) is left out before anything
  is made of its labels: the flipping alert of
  `TestCheckReplayGrafanaHistoryMisses`, 14,401 firings of 128 labels, is
  read and none of its labels is made into a key
  (`TestServeGrafanaJudgedFlipping` counts). An alert is compared by a hash
  of its labels. What is kept goes through the checks a copy through the
  port does, and where a copied-in firing goes.
- **Who writes.** Only serve writes the source `grafana`: the firings port
  refuses it, so a job's token can't write over Grafana's history, nor
  make a recorded rule that ties itself to a Grafana rule.
- **Names.** The history names a rule only by the title of one of the
  rules read from that Grafana; any other name's changes are passed over,
  never kept, so the history can't add a rule.

## Questions

- A rule whose severity label changed has two recorded rules of its name,
  and is tied to neither: keep its severity with its name, as an alarm's
  tie does?
- One title in two folders: the tags hold `grafana_folder`; tie by it too?
- Judge the rules `--check-grafana`'s Grafana runs, read from files, the
  same way?
- The source's writer shows as `firings-token`: a name of its own?
- A value in the chart for `--grafana-history=false` (today `extraArgs`).
