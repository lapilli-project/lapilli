# What Grafana's rules really fired, from Grafana's own history

Status: built, on the branch after v0.46.0. It changes where what really
fired is read from for Grafana's rules, not how a firing is judged, and
keeps nothing new in the ledger, so it is written down beside the build,
as one pull request. The companion to [fidelity.md](fidelity.md), whose
check it reads for.

## Why

`lapilli verdict --check-replay` and `lapilli serve`'s daily check set the
replay beside what really fired. For Grafana's rules that was
`GRAFANA_ALERTS{alertstate="firing",grafana_alertstate!="error"}`, which
Grafana writes only once its alert state history is sent to Prometheus
(`[unified_alerting.state_history] backend = prometheus`, or `multiple`
with it among the secondaries). That isn't its default: Grafana keeps its
history in annotations, in its own database, unless told otherwise; Loki is
the other place it keeps one. Most teams couldn't check a Grafana rule's
replay without changing how their production Grafana is configured, and
many won't. Grafana already serves the history it keeps, to anyone who may
read its rules: Lapilli reads it there.

## How it was measured

On 2026-11-16, four Grafanas ran the same rules, each with its own
database, from 07:17:30 UTC to 08:55, with Prometheus 3.5.0 and Loki
3.7.9: Grafana 12.4.14, the newest 12.x release two weeks old, and 13.3.1,
which the end-to-end tests run. They ran as the release tarballs for macOS
on arm64, on 127.0.0.1, as Docker wouldn't start on the machine measured
on. Loki took nothing until 07:21, as its write-ahead log stops taking
writes on a disk 90% full; it was started again without one.

| Grafana | Where it keeps its state history | `GRAFANA_ALERTS` |
|---|---|---|
| 12.4.14 | annotations, its default, nothing configured | none |
| 12.4.14 | `multiple`: annotations first, then Prometheus | written to one Prometheus |
| 13.3.1 | `multiple`: annotations first, then Prometheus | written to another |
| 13.3.1 | Loki | none |

The rules are the 26 of [states.md](states.md)
(`internal/replay/testdata/grafana-13.3.1/rules.json`), every query a
function of `time()` alone, so that what Grafana read and what the replay
reads are the same; and three more, every 10s: a title and labels with
`{`, `}`, `,`, `=` and `"` in them (`Tricky {title}, a=b - c`, a pod
`a, b=c} d`), a rule with three series, and a `for` of 1m. Each Grafana had
two service accounts: a Viewer, and one with no basic role.

### What Grafana serves

| Ask | What it answers | Who may |
|---|---|---|
| `GET /api/frontend/settings` | `unifiedAlerting.alertStateHistoryBackend`: `annotations`, `loki`, `prometheus` or `multiple`, and with `multiple`, `alertStateHistoryPrimary`, the one it serves. 13.3.1 and 12.4.14 alike | anyone signed in, the account with no role too |
| `GET /api/annotations?type=alert&from=MS&to=MS&limit=N` | Every alert annotation of the organisation, from and to in milliseconds, both within, newest first; with only one of them given, not by time at all. Each has `id`; `alertId`, the rule's id in Grafana's database, not its uid; `alertName`, the rule's title now, as joined from its rules (empty for one deleted); `prevState` and `newState`; `time`, the evaluation's, in milliseconds; `text`; `data`, the values; and `tags`, the alert's labels, `alertname` and `grafana_folder` among them, as Grafana 12.4 and later write them (below). `limit` is 100 unless given; 100000 answered all of them. `alertUID=UID` keeps one rule's. No cursor, nor an offset | `annotations:read`: a Viewer; the account with no role was refused (403) |
| `GET /api/v1/rules/history?ruleUID=UID&from=S&to=S`, annotations kept | A data frame of `time`, `text`, `prev`, `next` and `data`. The labels only in the text, `Title {a=b, c=d} - B=1.000000`, which can't be read back once a title or a value has `{`, `}`, `, ` or `=` in it: `Tricky {title}, a=b - c {alertname=Tricky {title}, a=b - c, ..., pod=a, b=c} d} - A=1.000000`. Its times a thousand times too large, the annotation's milliseconds read as seconds: `1794813480000000` for 07:18:00. `ruleUID` needed ("ruleUID is required to query annotations"); `limit` not taken (2 answered 17) | `alert.rules:read`: a Viewer; the account with no role was refused (403) |
| the same, Loki kept | A data frame of `time`, in milliseconds, `line` and `labels`, each line JSON: `previous`, `current`, `ruleUID`, `ruleTitle`, `fingerprint` and `labels`, as they are. Every rule's without `ruleUID`; newest first across Loki's streams, each stream's lines together (a stream a group); `limit` the newest so many; from and to in seconds | as above |

**The tags are not the labels as they are.** As Grafana's source has it
(`pkg/services/ngalert/state/historian/annotation.go`,
`convertLabelsToTags`, from 12.4.0; the tag store's `ParseTagPairs`), and
as a second recording on 12.4.14 showed: each colon in a name or a value
is an underscore (`instance:10.0.0.1_9100`, `url:http_//a.example_8080/x`);
a label whose name or value is then past 100 characters is left out (two
alerts told apart only by a path of 120 characters were one); so is one
past `[annotations] tags_length`, 500 by default, counted as JSON in the
labels' order by name (of nine labels of 60 characters and a short one,
the eighth long one went, the short one after it stayed); spaces either
end are trimmed, and a value of spaces leaves the name alone, with no
colon (`pad`). Grafana before 12.4 (12.3.0's source, and 11.x) writes no
tags at all: its annotations say nothing of which alert of a rule each
is of. The first recording had no colon in a value, nor a long one.

The states are Grafana's: `Normal`, `Pending`, `Alerting`, `Recovering`,
`NoData` and `Error`, with a reason or not: `Normal (MissingSeries)`,
`Normal (NoData)`, `Alerting (NoData)`, `Alerting (NoData, KeepLast)`,
`Pending (NoData)` were all seen. Each change is at the evaluation it
happened at, as `GRAFANA_ALERTS`'s samples are.

### How many matched

What the history says fired, set beside `GRAFANA_ALERTS` from 07:30 to
08:30, both read as `--check-replay` reads them, from before the hour (a
firing on since before 07:30 starting then); labels alike but
`grafana_rule_uid`, which only `GRAFANA_ALERTS` has. "Exactly" is the
firings on in the hour, each the same rule, labels, start and end, to
the second.

| History | Beside | Alike, as `--check-replay` counts | Exactly |
|---|---|---|---|
| 12.4.14, `multiple`, annotations | its own `GRAFANA_ALERTS` | 445 of 445 | 426 of 426 |
| 12.4.14, annotations alone | the 12.4.14 `GRAFANA_ALERTS` above | 445 of 445 | 426 of 426 |
| 13.3.1, `multiple`, annotations | its own `GRAFANA_ALERTS` | 445 of 445 | 426 of 426 |
| 13.3.1, Loki, read from 07:25 | the 13.3.1 `GRAFANA_ALERTS` above | 428 of 428 | 420 of 420, and 6 not seen |

A Grafana keeping its history alone has no `GRAFANA_ALERTS` of its own:
it is set beside another Grafana of the same version, running the same
rules from the same moment, whose evaluations are at the same times. The
six the Loki one didn't see are alerts that fired from 07:17:40, before
Loki took anything, to past 08:30, and changed state in none of it: the
history has nothing to say of them (below).

Read a page at a time, pages of 7 and of 3 read the same changes from the
real Grafanas as one page of 100,000 did: 1,277 annotations, every page
of 3 among changes at one time grown, and 1,305 lines from Loki.

And the replay, `lapilli verdict --check-replay` over the hour, against
each Grafana's own history and, for those writing both, against its
`GRAFANA_ALERTS` (`--check-series`): alike either way, 437 firings alike,
2 only in the replay (one each of `Keep through no data` and `NoData
NoData for 30s`, the replay's own differences, as the series has them
too), none only in what really fired, and the error alerts of the rule
whose query fails left out and said. Against the Loki one: 420 alike, the
same 2 only in the replay and the 6 it couldn't see.

### A second recording, of what the tags keep

The review of the first build found the tags weren't the labels as they
are (above), which the first recording, with no colon in a value nor a
long one, hadn't shown. Two more Grafanas 12.4.14 ran from 10:00 UTC, one
writing both and one on the default, with four rules every 10s: two
instances with their ports (`10.0.0.1:9100`) and a rule label with a URL;
two alerts told apart only by a path of 120 characters; a title with a
colon (`Disk: padded`) and a value of spaces beside one with spaces
round it; and nine labels of 60 characters beside a short one.

| From 10:05 (10:09 for the replay) to 10:30 | Alike |
|---|---|
| The history beside `GRAFANA_ALERTS`, as Grafana writes tags (`TestRealGrafanaHistory`) | 75 of 75; 68 of 68 exactly; the path's rule unsure |
| The same, the replay's side as `AnnotationLabels` gives it | 75 of 75 |
| `verdict --check-replay`, the default Grafana's history | 62 of 62; the path's rule not checked |
| The same, the first build, reading the tags as the labels | 31 of 78 (33%): the instances 0 of 17, the padded rule 0 of 22 and taken for an alert no rule is named, the path's rule 11 of 19; 29 changes passed over |
| `verdict --check-replay`, the other Grafana, `multiple` | 82 of 82, from `GRAFANA_ALERTS`, read first |

The five recordings are kept as test data (`internal/source/testdata`,
`grafana-*`), and `TestRealGrafanaHistory` reads them on every change
through a fake Grafana that pages as the real one does: each firing the
same as `GRAFANA_ALERTS`'s, set beside the tags as Grafana writes them,
starting and ending at the same evaluation, as in the tables.

## What was built

**Which Grafana.** `--grafana URL` reads the rules from a Grafana: its
history is read too. Grafana's rules read from files, provisioning files
with `--rules` or `--rules-history`, name theirs with `--check-grafana URL`,
as `--check-series` and `--check-firings` name where what really fired is
read from. Both take the token `--grafana-token-file` holds, read again
for each request: a Viewer's reads the rules, the data sources and the
history alike. `--check-series` still names series instead, and doesn't go
with `--check-grafana`.

**Where it is kept.** The frontend settings say, read once a run by
`verdict`, and within the hour by `serve`: annotations, from Grafana 12.4
on (`buildInfo.version`), are read from `/api/annotations`; Loki from
`/api/v1/rules/history`. Kept in several places (`multiple`), Grafana may
keep it in Prometheus too, which the settings don't say: then
`GRAFANA_ALERTS`, as before, while it holds anything for the period or the
day, and the history where it holds nothing. Anything else (Prometheus
alone, none, annotations before 12.4, settings that can't be read) is read
as before, from `GRAFANA_ALERTS`, and `verdict` says so.

**Annotations, not the history API, for annotations.** Their tags hold the
labels as Grafana keeps them, their times are right, and one read is of
every rule's, as `GRAFANA_ALERTS` is: alerts no rule given is named are
found, as there.

**Set beside the tags as they are.** Read from annotations, the replay's
firings of Grafana's rules are set beside them with their labels as the
tags hold them (`source.AnnotationLabels`: colons underscores, spaces
trimmed, those past 100 characters left out), and a rule is known by its
name there (`AnnotationName`), its tag `alertname`, which is what it was
called when the change was written, as `GRAFANA_ALERTS`'s is; for a title
past 100 characters, `alertName`. Labels the tags leave out, past
`tags_length`, a check passes over, as it passes over labels either side
hasn't. One annotation with no tags makes the read of no use (Grafana
before 12.4).

**A page at a time.** Neither has a cursor. Each page asks for the newest
1,000 changes to a time, the next to the time of the oldest of the page
before, and the changes at that time are read again and told apart: by id
for annotations, by a hash of when, the rule, the alert's fingerprint and
the states for Loki, to the second, as that API takes times. A page of
changes at one time alone is asked again for twice as many, 16,000 at
most, and 5,000 from Loki, its `max_entries_limit_per_query` by default:
none is lost where a page ends. More than that at one time can't be read,
and the read is of no use. A span asked over is a week at most, within
Loki's 721h.

**Into firings** (`firing.FromTransitions`), as `GRAFANA_ALERTS` makes
them: a firing from a change into `Alerting`, `Recovering` or `NoData`
until a change out of them; `Alerting` and `Recovering` are one firing, as
Grafana's notifications take them; `NoData` is one of its own, labelled
`grafana_alertstate="nodata"`, as the series and the replay label it; a
reason doesn't change the state. `Error` isn't a firing, as
`grafana_alertstate!="error"` leaves it out, and `--check-replay` says how
many error alerts each rule had, as it did from the series. The changes of
one alert keep its labels once, and its firings share them.

**What doesn't follow on.** Grafana writes every change of an alert from
the state the change before left (those it leaves out, Normal to Normal
(NoData) and into KeepLast, don't change whether it fires). A change from
a state the one before didn't leave, firing or not, says changes are
missing, or that two alerts are one in the history, as the tags left out
what told them apart: the rule is unsure, and isn't checked. So is a rule
some change of which was passed over (below), and two rules whose names
are one in the tags.

**From before.** Grafana keeps changes of state, not states, so the
history is read from before the period: a day before for `verdict`, and a
day and an hour before the day for `serve`, as the series and the replayed
firings are read. An alert on since then is seen as on at the start, and
one over by the period's start isn't in it, as a series read from then
doesn't have it.

**What reaches back.** Before a read, one more request asks whether the
history holds a change at or before the period's start, or the day's: one
that holds none begins after it, as one kept so far back no more, or
written only since, does, and says nothing of what fired then. Grafana's
rules aren't checked then. Annotations are kept for good unless
`[unified_alerting.state_history.annotations] max_age` or
`max_annotations_to_keep` says otherwise, and Loki as long as its
retention: a first start's 28 days of backfill may reach past them. A
quiet history, which holds an older change, is read as it is.

**What fails, fails Grafana's rules alone.** In `verdict`, a history that
can't be read, of no use, or not reaching back leaves Grafana's rules not
checked, and says why beside each; the others are checked as before. In
`serve`, a day whose history is of no use (`source.ErrUnusable`: refused,
HTTP 4xx but 401 and 429; more than is read at once; an answer that isn't the
history; changes at one time past what a page grows to) or doesn't reach
back isn't checked for Grafana's rules, and the check goes on to the next
day, so neither a busy Grafana nor a token that may not read annotations
holds it up. A rule unsure isn't checked that day, the others are. Only a
failure worth asking again, no answer or HTTP 429 or 5xx once the client
has asked again as it does, fails the round, which asks again the next, as
a Prometheus that doesn't answer does; so does HTTP 401, a token expired
or not yet rotated in, which would otherwise leave the days it met
unchecked for good once it is put right. Settings that can't be used are
taken as no history, and `GRAFANA_ALERTS` read. The Run history page says
which: that Grafana's history stands in, and where it is kept; or that over
the day last checked it held nothing from before it, too much, or what
couldn't be used, the log saying why. What it reads it reads with
Lapilli's own token, as the rules; nothing an outsider sends reaches it.

Nothing new is kept in the ledger: the counts, misses and alerts no rule is
named are kept as before (schema 010).

## What it reads, and its bounds

This reads what another system sends: the Grafana, and through it what
its data sources answered, labels a scraped target chose among them.
`serve` reads a day again after a restart, so nothing may grow past these,
or a hostile answer would bring it down again and again.

- **Answers:** 16 MiB each at most; a page past it is asked again half the
  size, down to ten changes, and past it then, of no use. A read asks 5,000
  requests and reads 256 MiB of answers at most, and keeps 200,000 changes
  of 50,000 alerts, their labels 64 MiB as counted (each label its bytes
  and 64 more, each alert 128 more), and tells 400,000 changes read apart:
  past any, it fails as too many, and `serve` doesn't check Grafana's rules
  that day.
- **What is kept:** of each alert, its rule and labels once, by a hash of
  them; of each change, its alert, its time and its states, each state's
  text once; of a change read, to tell it from one read again, its id or a
  16-byte hash. 40,000 changes of one alert with 128 labels, and its 20,000
  firings, keep some 5 MiB (`TestGrafanaHistoryMemory`).
- **A change** is passed over, and counted, when its alert has more than
  128 labels, a label's name past 256 bytes or its value past 4 KB, or a
  name given twice; when it names no rule, or one past 1 KB; when a state
  isn't one of Grafana's, or is past 200 bytes; or when it is outside the
  time asked for, by more than a minute. So is every other change of its
  alert: what was left of it would say it fired when it didn't. Its rule,
  where it has a name, is unsure and isn't checked; past a thousand rules
  unsure, none are. `verdict` says how many on stderr, `serve` logs it.
- **Retries** are the history client's, each try in its turn by the
  Grafana's `--max-rate`, the pace and the `--parallel` its data sources
  are asked by. Nothing it sends is followed: no redirect, and no URL in an
  answer (a line's `dashboardUID`, a link) is read. `http://`, in any case,
  is said to send the token unencrypted.
- **Misses:** a day can have hundreds of thousands of firings found only
  one way (an alert flipping between Alerting and NoData every 500ms, all
  day, is within every bound above). Only those the ledger keeps, the
  newest twenty of a rule, have their labels kept and are written
  (`ledger.NewestMisses`): copying 128 labels for each of 172,793 took
  serve's heap past 3 GiB (`TestCheckReplayGrafanaHistoryMisses` counts the
  copies).
- **Where names go:** an alert no rule given is named goes where one from
  the series went, kept only as a name that shows, 512 bytes at most, a
  hundred a day, and shown cleaned in `verdict`'s text. The labels of a
  firing found only in what really fired are kept as a replayed alert's
  are, a value 256 bytes at most, with nothing that turns text around (this
  now holds for the series' too), and go where those of the series went:
  the rule's page, and MCP's `rule`, as `replay_check.misses`. No rule or
  label read from the history reaches `/metrics` or Slack but as the
  counts of the rules Lapilli was given.

## What it can't see

- An alert that fired through all of what was read, a day and more,
  changing state in none of it. `GRAFANA_ALERTS` would have it; the
  history has nothing to say. Its firing started before, and was counted
  on the day it started, when it changed.
- A history pruned: a day it no longer reaches isn't checked.
- What the tags leave out. Labels past 100 characters, or past
  `tags_length`, aren't set beside the replay's; where they alone told
  alerts apart, the rule is found unsure and isn't checked, when its
  alerts' changes cross. Alerts told apart only by them that never fire at
  once are one alert to the history, and their firings are set beside the
  replay's as they are, which agree on what is left.
- Annotations of a rule whose title changed are read under the title they
  were written with, as `GRAFANA_ALERTS`'s `alertname` is.

## Alternatives

- **The history API for annotations too:** one endpoint for both. Its
  labels are in a text that can't be read back, and its times are wrong
  by a thousand; it asks a rule at a time, so alerts no rule given is
  named couldn't be found.
- **A rule at a time** (`alertUID=`): as many requests a day as rules, where
  one read takes them all.
- **Asking teams to write `GRAFANA_ALERTS`:** what this moves on from.

## Running the measurement again

Prometheus 3.5.0 with `--web.enable-remote-write-receiver`, Loki 3.7.9,
and the Grafanas with their Prometheus data source provisioned as
`uid: prom`, `jsonData.prometheusType: Prometheus`, and the rules of
`internal/replay/testdata/grafana-13.3.1/rules.json`. Those writing both:

```
GF_UNIFIED_ALERTING_STATE_HISTORY_ENABLED=true
GF_UNIFIED_ALERTING_STATE_HISTORY_BACKEND=multiple
GF_UNIFIED_ALERTING_STATE_HISTORY_PRIMARY=annotations
GF_UNIFIED_ALERTING_STATE_HISTORY_SECONDARIES=prometheus
GF_UNIFIED_ALERTING_STATE_HISTORY_PROMETHEUS_TARGET_DATASOURCE_UID=prom
```

the one in Loki `GF_UNIFIED_ALERTING_STATE_HISTORY_BACKEND=loki` and
`..._LOKI_REMOTE_URL`, the default one nothing. The recordings are the
answers, as given, with a Viewer's token:

```
curl -H "Authorization: Bearer $TOKEN" "$GRAFANA/api/annotations?type=alert&from=$FROM_MS&to=$TO_MS&limit=100000" | gzip > annotations.json.gz
curl -H "Authorization: Bearer $TOKEN" "$GRAFANA/api/v1/rules/history?from=$FROM_S&to=$TO_S&limit=100000" | gzip > history.json.gz
curl "$PROMETHEUS/api/v1/query_range" --data-urlencode 'query=GRAFANA_ALERTS{alertstate="firing",grafana_alertstate!="error"}' \
  -d start=$FROM_S -d end=$TO_S -d step=10 | gzip > alerts.json.gz
```

from 07:17:00 to 08:30:00 (the Loki one from 07:25:00), with the periods
in `grafanahistory_real_test.go` set to what was recorded. The second
recording's rules are `internal/source/testdata/grafana-12.4.14-tags/rules.json`,
recorded from 10:00:00 to 10:30:00.
