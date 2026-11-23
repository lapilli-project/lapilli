# Grafana's rules Lapilli can't replay, judged from Grafana's own history

Status: built, on the branch after v0.67.0. It changes which firings a
rule that can't be replayed has, not how a firing is judged, and kept
nothing new in the ledger (no schema), so it is written down beside the
build, as one pull request. It reads with the reader of
[grafanahistory.md](grafanahistory.md), and keeps what it reads as
[recorded.md](recorded.md) keeps firings copied in. 0.69.2 gave it the one
table it keeps, `left_out` (schema 030): a rule the history can't judge is
left out alone, remembered.

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
  it; the firings are given the name the rule is judged under. A change
  is taken for a judged rule only where the history says it is that
  rule's (0.69.2): kept in Loki, by the rule's uid each line carries; in
  annotations, by the rule's id in Grafana's database (`alertId`), the
  one Grafana's provisioning API lists the rule's uid with
  (`/api/v1/provisioning/alert-rules`), read once a day copied when a
  rule needs its own, within what the day's read of the history leaves of
  the reader's bounds, and 64 MB and 50,000 rules at most. Grafana asks
  of a token the same to list the rules as to export them, which Lapilli
  reads its rules from; the export holds no id. A rule it lists no more,
  lists twice, lists with an id it lists another rule with too, or lists
  under another title than the rules read (renamed since) is unsure. A
  change of another rule of the title is passed over and its alerts told
  apart from the judged rule's, so one title in two folders judges both,
  however many such
  rules there are, and a rule of the title in a folder the token doesn't
  read takes nothing. A rule some change of which the history passed
  over, or whose alerts it doesn't tell apart, isn't judged by it: a
  change passed over leaves out the rule it says it is of alone, not
  every rule of its title. Where the history says no rule's uid or id (a
  Loki line without `ruleUID`, or with one past 256 bytes; Grafana's
  annotations of alerts always carry `alertId`), a rule is judged by its
  name, and a name two of Grafana's rules share is neither's; such a
  change passed over, or any change of a name judged so, leaves out every
  rule of the name.
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
  in under the source `grafana`, a day a write, seven a round: the first,
  and the first after a gap, read from a day before it, each alert on at
  its start from then; the rest from the `--settle` before their start,
  written again, so a change Grafana kept late is seen. A day the history
  holds no change up to (it begins after it, kept so far back no more, or
  not yet) is passed over, asked each day; so is a day it is of no use
  for: refused, more than is read at once, more firings than a write
  holds, or none of the rules it can say all of. Each is left a gap,
  logged, never taken for a quiet day, and nothing already copied in is
  written over: neither by such a day, nor for a rule judged so no more
  (paused, deleted, renamed), whose firings a write keeps as they are. A
  firing ended, written again from its start, is written within the spans
  the copies cover, each apart, whichever round after a gap it ends in, so
  each gap stays one; what the history says began in a gap is left out
  with it, but an alert still on where the copies cover again, written
  from there in every round, as the first after the gap writes it,
  whether or not a later round reads where it began: one a read holds no
  change of stays on as it is, and one it says ended is ended from where
  it was written. An alert on since before a write, which the ledger has
  as on and the history says ended, is written again from its start with
  what the ledger has from there, within 31 days and 100,000 firings; one
  that started in a span the ledger let go, of the 64 it keeps, is ended
  where it is, in a write of the second it started in that isn't taken as
  covered again. One the history has begin before a write, which the
  ledger has from within it, on or ended (written from where the copies
  covered it again), is written from where the ledger has it, as the
  history has it: before 0.69.2, one ended before the next round read was
  taken away by that round, which reads the settle again from after it
  began, and the period read as quiet, with `--every 1h` and `--settle
  1h`. The firings are kept as copied-in
  ones are: rows of `firings` under a recorded rule of the source, one a
  rule by its name, whatever severities its alerts carry, written as
  `grafana-history`. The rule and its history are one rule, tied by name, as a
  cloud's alarm and its history are ([alarms.md](alarms.md)): the names of
  the rules judged so are kept in the ledger's meta (`grafana.judged`)
  before each run, and each run, version and copy ties them. Its page says
  so, and its record is what the copies cover. A source `grafana` a job
  wrote to before 0.68 isn't written over: serve says so once, and copies
  nothing (docs/upgrading.md).
- **A rule left out alone** (0.69.2, schema 030). A rule the history
  can't say all it fired of in a day's read is left out of that day alone,
  and the other rules' day is written: one whose name another of Grafana's
  rules has too (both, when both are judged), one a change of which the
  history passed over or whose alerts it doesn't tell apart, one with a
  firing a write can't hold where it would be written (starting in the
  write, or on at its start; one over before it is of nothing the ledger
  has, and is dropped alone, so that a read back doesn't leave the rule
  out again). Nothing of it is written or taken away, an
  alert of it on stays on, the log names it, and the period is kept for
  the rule in `left_out`: by source and rule, periods that meet as one, 64
  a rule (the two earliest run together past that, so nothing left out
  comes to read as copied), 5,000 rules, dropped with `--keep`. What the
  copies cover of a rule is their spans but for its periods, wherever
  that is read: its tie's `Since` and `To`, so whether it has twelve
  weeks to be retired by; its page, which says since when it is left out
  while it is; whether its numbers are set beside the span before; and,
  for a rule only copied in, what it is held to. A period stays no record
  whatever the history says of it later: nothing that began in it is
  written, in the round the rule is judged again, read as on a first day
  (each alert on then written from there), in a round shorter than
  `--settle` after it, which reads the period again, or after a restart.
  Judged again, the rule is read back from where it was left out, a day
  before the day copied at least and a week at most (0.69.2; a day alone
  before, so an alert of it that ended early in a longer period stayed
  on, and one that began then and was still on was never written): past
  a week, its alerts on since before it was left out, that the read says
  nothing of, are ended where it was left out, and one that began more
  than a week before and is on still, changing state in none of the
  week, isn't seen. Where reading back that far finds a rule unsure that
  the shorter read judged (a change passed over within the period), that
  rule alone is judged by a read from a day before, so one such change
  leaves it out for a day, not a week; an alert of it on since before,
  that this read says nothing of, stays on, as one a read holds no change
  of does, and is ended where the rule was left out only past a week. A
  read back of no use where the day's read was, this one or the one from
  a day before (past any of the reader's bounds, Grafana's list of its
  rules refused or past its own, answers that can't be used, such as
  annotations from before Grafana 12.4), leaves the rules it judges again
  out of the round once more, logged, and the other rules' day is
  written; the next round reads back again, so a Grafana that busy keeps
  them out while it is. The rules a name may be of across
  the read are those of the versions in effect across what is written,
  from the settle before it: a title shared in a version a round writes
  across leaves the rule out of that round too, where the history doesn't
  say each change's rule. A name the read holds no change of is its
  rule's, judged with no firing, whoever else has it. Before 0.69.2, such
  a rule made the day a gap for every rule judged so: the record was the
  source's, not each rule's. A rule whose name can't be kept (one that
  doesn't show) has no period kept: no firing of it is ever written, so
  it has no record at all. Past the 5,000 rules, the periods of rules no
  longer among Grafana's are let go of, those that ended earliest first,
  before a day is refused: such a rule's record, kept under its old name
  once it is renamed or deleted, then reads as copied over the periods
  let go of.
- **By its title.** A rule judged under a name made to tell it apart,
  from another of its title or from a rule read elsewhere (a Loki ruler's,
  a cloud's alarm), is read from the history by the title it has in
  Grafana. In 0.68.0 and 0.68.1 serve read it by the name it is judged
  under, which the history never has: it read as a rule that never fired.
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
  on. So does one deleted and made again with its uid, in annotations:
  Grafana gives the new rule another id, and the old one's changes, of an
  id it lists no more, are passed over.

## Attacker's look

This reads what another system sends, the Grafana, and through it what
its data sources answered.

- **How much.** The reader's bounds, unchanged: 16 MiB an answer, 5,000
  requests, 256 MiB of answers, Grafana's list of its rules among them,
  200,000 changes of 50,000 alerts, their labels 64 MiB. A day past any
  is passed over, logged, and the copy goes on from the day after; so is
  a day of more than 100,000 firings, which no write holds. Only a
  failure worth asking again fails the round.
- **What is kept.** A firing a write can't hold (more than 32 labels,
  labels past 4 KB, a name that doesn't show) leaves its rule out of the
  day before anything is made of its labels: the flipping alert of
  `TestCheckReplayGrafanaHistoryMisses`, 14,401 firings of 128 labels,
  the one rule judged there, leaves its days a gap, and none of its
  labels is made into a key (`TestServeGrafanaJudgedFlipping` counts);
  beside another rule, that one's days are written
  (`TestServeGrafanaJudgedUnheldAlone`). An alert is compared by a hash
  of its labels. What is kept goes through the checks a copy through the
  port does, and where a copied-in firing goes.
- **Who writes.** Only serve writes the source `grafana`: the firings port
  refuses it, so a job's token can't write over Grafana's history, nor
  make a recorded rule that ties itself to a Grafana rule.
- **Who writes the history.** It is taken as Grafana wrote it. Kept in
  Loki, it is written by whoever can push to that tenant, Grafana or not:
  they write these rules' record, their firings within the bounds above,
  under a rule's uid; with a line of a rule's name that has no uid, they
  have that rule judged by its name, as above; with a change to a state
  Grafana has no name for, under a rule's uid, they leave that rule with
  no record while a read holds the change, and with no uid every rule of
  its name (a read back from where it was left out holds it a day at
  most); and with changes passed over of more than 1,000 rules, which a
  read can't say all of, they make every day a gap. SECURITY.md says so:
  guard the tenant as the Grafana.
- **Names.** The history names a rule only by the title of one of the
  rules read from that Grafana; any other name's changes are passed over,
  never kept, so the history can't add a rule. A change of a judged rule's
  title is that rule's only where the history says so, by the rule's uid
  (Loki) or its id as Grafana lists the rule's uid (annotations), as
  above: so whoever can make a rule of a judged rule's title, in a folder
  Lapilli reads or one its token doesn't, paused or not, writes nothing
  into that rule's record, ends none of its alerts, and takes nothing
  away, whether the rule is there at a round or made, firing and deleted
  between two, and whether its changes are read or passed over (two
  labels Grafana's tags keep under one name, `team:a` and `team_a`)
  (`TestServeGrafanaJudgedSharedBetweenRounds`,
  `TestServeGrafanaJudgedHiddenFolder`,
  `TestServeGrafanaJudgedHiddenFolderPassedOver`,
  `TestGrafanaJudgedByUID`, `TestGrafanaJudgedByID`,
  `TestGrafanaJudgedPassedOverByRule`). Past 1,000 rules, though, a read
  can't say which rules it passed over changes of, and judges none: a day
  holding changes passed over of more than 1,000 rules, firing in any
  folder, is a gap for every rule. Before 0.69.2 a rule there between two
  rounds wrote its firings into the judged rule's record, and its alert
  could end the judged rule's, as a title was counted at the round alone;
  and a title shared at a round left the judged rule out. Where the history
  says no rule's uid or id (a Loki line without `ruleUID`, as a writer to
  the tenant may push), a name two rules give,
  counted over every rule read before `--rule` picks, paused ones among
  them, and over every version in effect across what a round writes, is
  neither's: `lapilli verdict` lists such a rule as before, and serve
  leaves the rule out while a round writes across such a version, the
  period kept (`TestServeGrafanaJudgedShared`); a rule no read of the
  rules saw, in a folder the token doesn't read or there between two
  rounds, is taken for the judged rule then, and such a line passed over
  leaves every rule of its name out. That is the one way left for
  another rule's changes to be taken for a judged rule's, or to take its
  record away, but for changes passed over of more than 1,000 rules.
  What fired under the title while it was left out is no one's; an alert
  the history has on under the title where the rule is judged again is
  written from there, the other rule's among them, until the history
  says it ended.
- **What it can make serve miss.** One rule whose changes the history
  can't say all of, or whose alerts no write holds, is left out alone,
  with no record for those days: the other rules' copies go on. What
  still makes a day a gap for every rule judged so is of the whole read:
  the history refused or of no use, Grafana's list of its rules refused
  or past its bounds (64 MB, 50,000 rules, or what the day's read leaves
  of the reader's) where the day's read needs a rule's id, a day of more
  changes than the reader's bounds, of changes passed over of more than
  1,000 rules, or of more than 100,000 firings (one rule flapping enough
  is enough), and more than 5,000 rules to keep as left out. A read back
  of no use, from where a rule was left out, where the day's read was of
  use, leaves out the rules judged again alone.

## Questions

- One recorded rule a name keeps no severity: the report gives such a rule
  none, where a replayed rule has its labels'.
- In annotations, a rule is tied to its changes by its id as
  `/api/v1/provisioning/alert-rules` lists it (0.69.2), which Grafana's
  main branch marks deprecated, beside the rules API of its app platform:
  once it is gone, no rule is judged from annotations, and each day is a
  gap. Read the ids from wherever Grafana lists them then?
- A day past the reader's bounds, or of more firings than a write holds,
  is a gap for every rule judged so: leave out the rule with the most?
- The rules list gives a rule that fired before the weeks shown, and has
  no firing in them, a dash for how it did, one left out through them
  too, as it does a rule of any source across a gap: say no record there?
- Judge the rules `--check-grafana`'s Grafana runs, read from files, the
  same way?
- A value in the chart for `--grafana-history=false` (today `extraArgs`).
