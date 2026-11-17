# The numbers, and how to count them again

Every number Lapilli gives is counted from the firings and the incidents of
a report. This document says what each number is, as the code counts it,
and how to count it again yourself from the report, without Lapilli. Then
it says which names stay the same from release to release, and how one is
changed when it must be.

## The report

`lapilli verdict --json` writes the report; `lapilli serve` gives the same
shape for any period at `/api/v1/report?from=&to=` (31 days at most with
every firing):

```sh
lapilli verdict --rules alerts/ --prometheus http://prometheus:9090 --incidents incidents.yaml \
  --match service --from 2026-10-01T00:00:00Z --to 2026-11-01T00:00:00Z --json > report.json
curl -s 'http://lapilli:8080/api/v1/report?from=2026-10-01T00:00:00Z&to=2026-11-01T00:00:00Z' > report.json
```

Each block below is a jq program for `jq -f FILE report.json`, or `jq
'PROGRAM' report.json`. A number comes twice: as the report has it, from
`rules[]` and `incidents[]`, and counted again from `firings[]`, the
firings one by one; a number of `/api/v1/summary` a third time, as the
summary has it, beside the report of the same week. They give the same
answer: `TestNumbersRecount` and `TestNumbersRecountServe` run every block
in this file, on a report of `lapilli verdict` and of `lapilli serve`, and
hold them to it.

A field marked † below is left out of the JSON when it is zero, false or
empty: read it missing as that (`.unjudged // 0`). Times are RFC 3339, in
UTC, or in the offset the file they were read from gave. jq reads only UTC
(`fromdateiso8601`), so the blocks that do sums with times define `epoch`,
which reads any offset:

```
def epoch: capture("^(?<t>.{19})(?<f>\\.[0-9]+)?(?<z>Z|[+-][0-9]{2}:[0-9]{2})$")
  | (.t + "Z" | fromdateiso8601) + ((.f // "0") | tonumber)
    - (if .z == "Z" then 0 else (if .z[0:1] == "-" then -1 else 1 end) * ((.z[1:3] | tonumber) * 3600 + (.z[4:6] | tonumber) * 60) end);
```

`settings` says what the firings were judged by (`lead_ns`, `match`,
`refire_ns`, `unknown_length_ns`, `record_lookback_ns`, `impact_from`†,
`impact_no_data`†, `record_by_week`†, `impact_signals`†, `impact_rules`†):
the flags, or in
`lapilli serve` the settings page. Durations are in nanoseconds, as
everywhere in the report.

## Which firings and incidents a report holds

**Firings.** `lapilli verdict`: every firing on at some time from `from` to
`to`, so one that started before the period and was still firing is in it.
`lapilli serve`: every firing that started in `[from, to)`, apart from
those a quiet window excused, which are in no number but `rules[].excused`†.
A firing is one alert: one rule, one label set, from when it fired to when
it resolved (`end`†, missing while it still fires).

**Incidents.** Those that started in `[from, to)`: written down, or impact
windows (`candidate: true`, below). In `lapilli serve`, an impact window
dismissed in the UI isn't one.

## A white or a black stone

A firing **catches** an incident when it started from `lead` (15m) before
the incident's start to the incident's end, both included, and agrees with
the incident on each `match` label both have. An incident with no end is
taken to last `unknown_length` (1h). An incident's label may have several
values, any of which agrees; a label only one of them has is no
constraint. A firing that caught an incident is a **white stone**
(`white: true`); one that caught none is a **black stone**. A firing
already on before `lead` caught nothing: it was on anyway.

With named impact signals (`--impact NAME=PROMQL`, `--impact-file`;
[design/signals.md](design/signals.md)), a window has its `signal`†, and
is matched on that signal's `match` in `settings.impact_signals`† in place
of `match` (`null` for `match` itself, `[]` for no label). A rule named by
a rule entry (`rules[].impact_rule`†, its place in `settings.impact_rules`†
from 1) catches only the windows of the entry's `signals`, on the entry's
`match` when it gives one: another signal's windows are, to it, as if they
weren't there. An incident written down has no signal, and every rule is
matched against it on `match`.

```jq
# incidents caught: as the report has it
[.firings[] | .incidents // []]
```

```jq
# incidents caught: counted again
def epoch: capture("^(?<t>.{19})(?<f>\\.[0-9]+)?(?<z>Z|[+-][0-9]{2}:[0-9]{2})$")
  | (.t + "Z" | fromdateiso8601) + ((.f // "0") | tonumber)
    - (if .z == "Z" then 0 else (if .z[0:1] == "-" then -1 else 1 end) * ((.z[1:3] | tonumber) * 3600 + (.z[4:6] | tonumber) * 60) end);
.settings as $s
| (($s.impact_signals // []) | map({key: .name, value: .}) | from_entries) as $sig
| ([.rules[] | select(.impact_rule) | {key: .rule, value: $s.impact_rules[.impact_rule - 1]}] | from_entries) as $entry
| .incidents as $incs
| [.firings[] | . as $f | ($f.start | epoch) as $at | $entry[$f.rule] as $e
   | [$incs[] | . as $in
       | (if $in.signal == null then $s.match
          elif $e != null and $e.match != null then $e.match
          else $sig[$in.signal].match // $s.match end) as $match
       | select(($in.signal == null or $e == null or any($e.signals[]; . == $in.signal))
         and $at >= ($in.start | epoch) - $s.lead_ns / 1e9
         and $at <= (if $in.end then $in.end | epoch else ($in.start | epoch) + $s.unknown_length_ns / 1e9 end)
         and all($match[]; ($f.labels // {})[.] as $v | ($in.labels // {})[.] as $vs
               | $v == null or $vs == null or any($vs[]; . == $v)))
       | .id]]
```

In `lapilli serve`, a firing near the start of the period may have caught
an incident that started before it, which the report's `incidents` leave
out: count again over a period that starts `lead` earlier.

A rule's stones, as `rules[]` counts them:

```jq
# stones by rule: as the report has it
[.rules[] | select(.firings > 0) | {rule, firings, white, black, unjudged: (.unjudged // 0)}] | sort_by(.rule)
```

```jq
# stones by rule: counted again
[.firings | group_by(.rule)[] | {rule: .[0].rule, firings: length, white: map(select(.white)) | length,
   black: map(select(.white | not)) | length, unjudged: map(select(.no_record)) | length}]
```

## No record

A black stone says nothing was wrong, which is only so if someone was
writing down what went wrong. A firing is **judged** when the record was
kept at its time: an incident was on at some time in the two weeks
(`record_lookback`) before it, or an impact signal (`--impact`) was read
over its time (`impact_from`† at or before it), or it caught an incident.
`impact_from` is where the signal was first read, or later, where the
data of every selector it reads begins, as a named signal's `start`
(below); it is missing, with `impact_no_data: true`†, when a selector it
reads has no data, which keeps no record.
The incidents that keep the record are all those given, from before the
period too. Any other firing is black, with `no_record: true`†: counted,
in `firings` and `black`, and in its rule's `unjudged`†, but in no
precision, and it makes no rule noisy. A rule none of whose firings is
judged has `no_record: true`†; a report none of whose firings could be,
too.

With named impact signals, a firing's record is kept by what could have
seen the incident it would catch: its `signals`†. They are its rule
entry's; else each signal that, for every label of the signal's `match`
the firing has, has returned the firing's value of it over the read (a
series that returned something without that label, or more than 1,000
values of it, or 256 KB of values in all, has returned every value); and,
when none of those keeps
the record (`record`), every signal that does too. Such a firing is judged
when an incident written down was on in the two weeks before it, or a
window of one of its `signals` was, or it caught an incident, or the
signals among its `signals` that keep the record, one at least, had each
started by its time: `settings.impact_signals[].start`†, where the signal
was first read, or later, where the data of every selector it reads
begins (none for a selector with no data). `impact_from` is then missing:
it is the start of an impact signal with no name.

`lapilli verdict` judges each firing by its own time:

```jq
# no record (lapilli verdict): as the report has it
[.firings[] | .no_record // false]
```

```jq
# no record (lapilli verdict): counted again
def epoch: capture("^(?<t>.{19})(?<f>\\.[0-9]+)?(?<z>Z|[+-][0-9]{2}:[0-9]{2})$")
  | (.t + "Z" | fromdateiso8601) + ((.f // "0") | tonumber)
    - (if .z == "Z" then 0 else (if .z[0:1] == "-" then -1 else 1 end) * ((.z[1:3] | tonumber) * 3600 + (.z[4:6] | tonumber) * 60) end);
.settings as $s
| (($s.impact_signals // []) | map({key: .name, value: .}) | from_entries) as $sig
| (.incidents + ($ARGS.named.earlier // [])) as $incs
| [.firings[] | (.start | epoch) as $at | (.signals // []) as $ties
   | [$ties[] | $sig[.] | select(.record)] as $waits
   | (.white | not)
     and ($s.impact_from == null or ($s.impact_from | epoch) > $at)
     and ($waits == [] or any($waits[]; .start == null or (.start | epoch) > $at))
     and (any($incs[];
            (.signal == null or (.signal as $x | any($ties[]; . == $x)))
            and (.start | epoch) <= $at
            and (if .end then .end | epoch else (.start | epoch) + $s.unknown_length_ns / 1e9 end)
                >= $at - $s.record_lookback_ns / 1e9)
          | not)]
```

The report's `incidents` are those of the period; give the incidents of
the two weeks before it as `--argjson earlier '[{"start": "...", "end":
"..."}]'`, from your incidents file or from `lapilli serve`'s
`/api/v1/incidents?from=&to=`.

`lapilli serve` (`record_by_week: true`) judges a firing by its week, from
Monday in its `--tz`, as it keeps its stones: a week is judged when an
incident was on at some time from two weeks before the week's Monday to
the week's end, or the impact signal was read over all of the week. The
firings of a week are judged by the signals tied to them: every firing of
a week tied to the same signals is judged, or none; with no named signal,
which `lapilli serve` reads one of for now, every firing of the week.
`/api/v1/summary` gives each week's span and `no_record`.

## Precision

A rule's **precision** is the share of its judged firings that caught an
incident: `white / (firings - unjudged)`. With none judged (it never fired,
or `no_record`), it has none: `–` in the text, `null` here. The precision
of a set of firings, a week's or everyone's, is the same sum over all of
them; with no firing judged, none.

```jq
# precision by rule: as the report has it
[.rules[] | select(.firings > 0)
 | {rule, precision: (if .no_record or .firings == (.unjudged // 0) then null else .white / (.firings - (.unjudged // 0)) end)}]
| sort_by(.rule)
```

```jq
# precision by rule: counted again
[.firings | group_by(.rule)[] | map(select(.no_record | not)) as $judged
 | {rule: .[0].rule, precision: (if $judged == [] then null else ($judged | map(select(.white)) | length) / ($judged | length) end)}]
```

```jq
# precision: as the report has it
[.rules[] | {white, judged: (.firings - (.unjudged // 0))}]
| {white: (map(.white) | add // 0), judged: (map(.judged) | add // 0)}
| if .judged > 0 then .white / .judged else null end
```

```jq
# precision: as the summary has it
first(.rows[] | select(.team == "")) | .precision
```

```jq
# precision: counted again
[.firings[] | select(.no_record | not)] | if . == [] then null else (map(select(.white)) | length) / length end
```

`/api/v1/summary`'s `precision` is a week's, over the firings of the week:
count it again from `/api/v1/report` over the same week.

## Noisy

A rule is **noisy** when at least `noisy_at.firings` (10) of its firings
were judged and under `noisy_at.precision` (20%) of them caught anything:
it fires whatever happens, so its catches are likely chance. Over a report,
noisy by the report's period; in `/api/v1/summary`, `/metrics` and the
pages of `lapilli serve`, by the incident's week.

```jq
# noisy: as the report has it
[.rules[] | select(.firings > 0) | {rule, noisy}] | sort_by(.rule)
```

```jq
# noisy: counted again
.noisy_at as $at
| [.firings | group_by(.rule)[] | map(select(.no_record | not)) as $j | ($j | map(select(.white)) | length) as $w
   | {rule: .[0].rule, noisy: (($j | length) > 0 and ($j | length) >= $at.firings and $w / ($j | length) < $at.precision)}]
```

## Re-fired

A firing **re-fired** when it started within `refire` (1h) of the same
alert, the same rule and labels, last resolving: its start minus the last
one's end is `refire` or less. The rule's `refired` counts them;
`firings[].refired`† marks each.

```jq
# re-fired: as the report has it
[.rules[] | select(.firings > 0) | {rule, refired}] | sort_by(.rule)
```

```jq
# re-fired: counted again
def epoch: capture("^(?<t>.{19})(?<f>\\.[0-9]+)?(?<z>Z|[+-][0-9]{2}:[0-9]{2})$")
  | (.t + "Z" | fromdateiso8601) + ((.f // "0") | tonumber)
    - (if .z == "Z" then 0 else (if .z[0:1] == "-" then -1 else 1 end) * ((.z[1:3] | tonumber) * 3600 + (.z[4:6] | tonumber) * 60) end);
(.settings.refire_ns / 1e9) as $refire
| [.firings | group_by([.rule, (.labels // {})])[] | sort_by(.start | epoch) as $g
   | range($g | length) as $i
   | {rule: $g[$i].rule,
      refired: ($i > 0 and $g[$i - 1].end != null and ($g[$i].start | epoch) - ($g[$i - 1].end | epoch) <= $refire)}]
| group_by(.rule) | map({rule: .[0].rule, refired: map(select(.refired)) | length})
```

`lapilli verdict` sees the firings of the period alone. `lapilli serve`
marks a firing when it records it, against every firing it holds: one
early in a period may have re-fired after one before it, which a count
from the period's firings alone can't see; and the alert's last firing
may be one a quiet window excused, which the report's firings leave out
(the CSV of `lapilli serve` has it). An excused firing re-fires for
nothing: it is in no rule's `refired`.

`/api/v1/summary`'s `refired` is a week's: the firings that re-fired, and
their share of the week's firings. Count it again from `/api/v1/report`
over the same week:

```jq
# re-fired in a week: as the summary has it
first(.rows[] | select(.team == "")) | {firings: .refired.firings, share: .refired.share}
```

```jq
# re-fired in a week: counted again
(.firings | length) as $all | [.firings[] | select(.refired)] | length
| {firings: ., share: (if $all > 0 then . / $all else null end)}
```

## Incidents missed, and caught only by noisy rules

An incident is **missed** when no firing caught it (`caught_by` empty). It
was **caught only by noisy rules** when every rule that caught it is noisy:
little better than missed. `caught_by[]` lists each rule that caught it,
with `delay_ns`, from the incident's start to the rule's first firing that
caught it (negative when the alert came first), the earliest first.

```jq
# missed: as the report has it
{missed: [.incidents[] | select(.caught_by == []) | .id],
 only_noisy: [.incidents[] | select(.caught_by != [] and all(.caught_by[]; .noisy)) | .id]}
```

```jq
# missed: counted again
.noisy_at as $at
| ([.firings | group_by(.rule)[] | map(select(.no_record | not)) as $j | ($j | map(select(.white)) | length) as $w
    | {key: .[0].rule, value: (($j | length) > 0 and ($j | length) >= $at.firings and $w / ($j | length) < $at.precision)}]
   | from_entries) as $noisy
| .firings as $fs
| [.incidents[] | .id as $id | [$fs[] | select(any((.incidents // [])[]; . == $id)) | .rule] | unique | {id: $id, by: .}] as $c
| {missed: [$c[] | select(.by == []) | .id],
   only_noisy: [$c[] | select(.by != [] and all(.by[]; $noisy[.])) | .id]}
```

`/api/v1/summary` counts them by week, written incidents (`incidents`) and
impact windows (`impacts`) apart, `noisy_only` by the rules noisy in the
incident's week.

## Impact windows

`--impact PROMQL` reads an expression that returns something while users
are hurt. Each series is read on its own, from two weeks before the period
(which keeps the record, above); its samples from the period's start,
`--step` apart, gaps of two steps or less not splitting them, make a
window from the first to a step after the last, kept when it lasts
`--impact-for` (5m) or longer, with the series' labels. Windows of one
signal that overlap are one, with every value of the labels they all
have; windows of two named signals never are, however they overlap. One
that overlaps an incident written down, agreeing on its `match`, is that
incident, and isn't added. The rest are incidents with `candidate: true`,
ids `impact-1`, `impact-2` and so on, or a named signal's
`impact-NAME-1`, with its `signal`†, judged against as any incident is.
`lapilli serve` keeps them, and a person confirms or dismisses each.
`[.incidents[] | select(.candidate)]` are a report's impact windows, and
`[.incidents[] | select(.candidate | not)]` its incidents written down.

## Lead

An incident's **lead** is how long before its start the first alert of a
rule that isn't noisy came: `-delay_ns` of its first catch by a rule that
isn't noisy, positive before, negative after. Caught only by noisy rules,
or missed, it has none. `lapilli_incident_detection_delay_seconds` is the
same time the other way round (delay), as it always was.

```jq
# lead: as the report has it
[.incidents[] | {id, lead_seconds: (first(.caught_by[] | select(.noisy | not) | -.delay_ns / 1e9) // null)}]
```

```jq
# lead: counted again
def epoch: capture("^(?<t>.{19})(?<f>\\.[0-9]+)?(?<z>Z|[+-][0-9]{2}:[0-9]{2})$")
  | (.t + "Z" | fromdateiso8601) + ((.f // "0") | tonumber)
    - (if .z == "Z" then 0 else (if .z[0:1] == "-" then -1 else 1 end) * ((.z[1:3] | tonumber) * 3600 + (.z[4:6] | tonumber) * 60) end);
.noisy_at as $at
| ([.firings | group_by(.rule)[] | map(select(.no_record | not)) as $j | ($j | map(select(.white)) | length) as $w
    | {key: .[0].rule, value: (($j | length) > 0 and ($j | length) >= $at.firings and $w / ($j | length) < $at.precision)}]
   | from_entries) as $noisy
| .firings as $fs
| [.incidents[] | .id as $id | (.start | epoch) as $at
   | [$fs[] | select(any((.incidents // [])[]; . == $id) and ($noisy[.rule] | not)) | .start | epoch] as $starts
   | {id: $id, lead_seconds: (if $starts == [] then null else $at - ($starts | min) end)}]
```

`/api/v1/summary`'s `median_lead_seconds` is, for a week's incidents
caught, the median of their leads: the middle one, or with an even number
the mean of the two in the middle, cut to whole seconds toward zero; none
caught, `null`.

```jq
# median lead: as the summary has it
first(.rows[] | select(.team == "")) | {incidents: .incidents.median_lead_seconds, impacts: .impacts.median_lead_seconds}
```

```jq
# median lead: counted again
def median: sort | if length == 0 then null
  elif length % 2 == 1 then .[(length - 1) / 2]
  else (.[length / 2 - 1] + .[length / 2]) / 2 | if . < 0 then ceil else floor end end;
[.incidents[] | {candidate, lead: first(.caught_by[] | select(.noisy | not) | -.delay_ns / 1e9)}]
| {incidents: map(select(.candidate | not) | .lead) | median, impacts: map(select(.candidate) | .lead) | median}
```

## Critical share caught

Of the firings of the rules whose severity is critical, the share that
caught an incident: what a team that wants "80% of critical alerts needed
action" reads. A rule's severity is its `severity` label; a rule whose
versions, or rules of one name, have several is `critical/warning`, and
counts when any of them is one counted (`/api/v1/summary`'s `critical`,
`critical` by default). The share is of the firings judged: with none,
`null`. `firings` counts every one, judged or not.

```jq
# critical share caught: as the report has it
[.rules[] | select((.severity // "") | split("/") | any(. == "critical"))]
| {firings: (map(.firings) | add // 0), caught: (map(.white) | add // 0),
   judged: (map(.firings - (.unjudged // 0)) | add // 0)}
| {firings, caught, share_caught: (if .judged > 0 then .caught / .judged else null end)}
```

```jq
# critical share caught: as the summary has it
first(.rows[] | select(.team == "")) | .critical
```

```jq
# critical share caught: counted again
(.rules | map({key: .rule, value: (.severity // "")}) | from_entries) as $sev
| [.firings[] | select($sev[.rule] | split("/") | any(. == "critical"))]
| {firings: length, caught: map(select(.white)) | length, judged: map(select(.no_record | not)) | length}
| {firings, caught, share_caught: (if .judged > 0 then .caught / .judged else null end)}
```

## Firings during deploys (lapilli serve)

With `--deploys`, a firing came **during a deploy** when it started from
`--deploy-before` before the deploy's start to `--deploy-after` after its
end and agrees with the deploy on every label both have, one at least;
a deploy that names no label is everything's, every firing in its time
(docs/design/noc.md). It is marked, not judged apart: it is in `firings`
and `white` as any other, and also in the rule's `during_deploys`† and
`during_deploys_caught`†. Precision without them leaves them out of both
sides.

```jq
# during deploys (lapilli serve): as the report has it
[.rules[] | select(.firings > 0) | {rule, firings: (.during_deploys // 0), caught: (.during_deploys_caught // 0)}] | sort_by(.rule)
```

```jq
# during deploys (lapilli serve): counted again
[.firings | group_by(.rule)[] | {rule: .[0].rule, firings: map(select(.during_deploy)) | length,
   caught: map(select(.during_deploy and .white)) | length}]
```

## Firings during marked windows (lapilli serve)

A marked window is a span of a named kind, a sale, a launch, a campaign
(docs/design/marked.md), from `--marked`, `--config`, the UI or an
expression (`--mark`). A firing came **during a kind** when it started
during an occurrence of a window of the kind (each repeat of one that
repeats is one) and agrees with it on every label both have, one at
least; a window that names no label marks every firing, and a team's
window only the team's rules. A firing a quiet window excused isn't
marked. Like a deploy, it is marked, not judged apart: it is in `firings`
and `white` as any other, and also in its rule's `marked`†, by kind:
`firings`, `caught`, `refired` and `unjudged`†. A firing may be marked
with several kinds; `firings[].marked`† names them. Deploys are no kind
here: they are `during_deploys`†. **Outside a kind** is a count less the
kind's: precision outside a sale is `(white - marked.sale.caught) /
(firings - unjudged - marked.sale.firings + marked.sale.unjudged)`.

```jq
# marked (lapilli serve): as the report has it
[.rules[] | select(.marked) | {rule, marked: (.marked | map_values({firings, caught, refired}))}] | sort_by(.rule)
```

```jq
# marked (lapilli serve): counted again
[.firings | map(select(.marked)) | group_by(.rule)[]
 | {rule: .[0].rule,
    marked: ([.[] | .marked[] as $k | {k: $k, white, refired: (.refired // false)}] | group_by(.k)
             | map({key: .[0].k, value: {firings: length, caught: map(select(.white)) | length, refired: map(select(.refired)) | length}})
             | from_entries)}]
| sort_by(.rule)
```

`/api/v1/summary`'s `marked` holds, for each week, each kind with an
occurrence in it: `windows`, the occurrences; the firings marked, those
that caught something and re-fired, with precision inside the kind and
outside it; and the incidents and impact windows that started during an
occurrence of the kind and agree with it, as a firing does, caught,
caught only by noisy rules or missed, with their median lead. Count a
week's again from `/api/v1/report` over the same week:

```jq
# marked in a week: as the summary has it
first(.rows[] | select(.team == "")) | .marked | map_values({firings, caught, refired}) | with_entries(select(.value.firings > 0))
```

```jq
# marked in a week: counted again
[.firings[] | select(.marked) | .marked[] as $k | {k: $k, white, refired: (.refired // false)}] | group_by(.k)
| map({key: .[0].k, value: {firings: length, caught: map(select(.white)) | length, refired: map(select(.refired)) | length}})
| from_entries
```

## The firings as CSV

`lapilli verdict --csv`, and `/api/v1/report?from=&to=&format=csv` (31 days
at most, one at a time, as with every firing as JSON), give the firings a
row each, for a spreadsheet or a script:

| Column | |
|---|---|
| `rule` | the rule's name |
| `severity` | the rule's severity, as `rules[].severity` has it |
| `team` | in `lapilli serve`, the team that owns the rule; empty when none does, and in `lapilli verdict` |
| `labels` | the firing's labels, a JSON object, its keys in order |
| `start`, `end` | RFC 3339, in UTC; `end` empty while it still fires |
| `stone` | `white`, `black`, or empty for a firing a quiet window excused |
| `no_record` | `true` for a black stone not judged (above) |
| `incidents` | the ids of the incidents it caught, a JSON array |
| `refired` | `true` when it re-fired; `false` for an excused firing, which is in no rule's `refired` |
| `during_deploy` | `true` when it came during a deploy (`lapilli serve`) |
| `excused` | `true` for a firing a quiet window excused (`lapilli serve`): in none of the numbers |
| `marked` | the kinds of marked window it came during (`lapilli serve`), a JSON array, `[]` for none |

The firings are in order of their start, then of their rule. The file is
UTF-8 with no byte order mark: open it in a spreadsheet as UTF-8 (Excel's
Data, From Text/CSV), or a name in another script reads as other
characters.

A rule's name, a severity, a team, labels and incident ids come from
whoever wrote them, and are written so a spreadsheet shows them rather
than runs them, and a reader sees what is there:

- a control or format character (a line break, a tab, a bidirectional
  override) is written as `\u{...}`, as the text report writes it, and a
  backslash as `\\`, so no two names are written alike, in every cell and
  in the JSON of labels and incidents;
- a cell whose first character, past any spaces, is `=`, `+`, `-` or `@`,
  or one of their full-width forms (`＝`, `＋`, `－`, `＠`), is written with
  `'` before it. Take the `'` off to read the name as it is.

Labels and incidents are JSON, and their cells start with `{` or `[`.

## What stays the same

What is listed here keeps its name and its meaning in every release. While
the version is 0.x a minor release may still change it (GOVERNANCE.md),
and when one does:

- its changelog entry says so, under Changed, with what to do;
- a JSON field or a metric renamed, or changed in meaning, is written under
  its new name beside the old, as it was, for at least one minor release
  before the old one goes; a CSV column the same, the new one at the end;
- a label of a metric is renamed or taken away only with the metric's
  name, as above;
- a number defined here changes only with this document, in the same pull
  request.

A field, a column (at the end), a metric or a label may be added in a
minor release, said in the changelog. A patch release changes none of
them. What isn't listed (`near_misses`, `suggestions`, `unrecorded`,
`unreplayed`, `hours`, a rule's `unseen` and `for_outlasts`, and the
metrics of `lapilli serve` about itself that aren't below) may change in a
minor release, said in the changelog.

`/api/v1/summary` keeps its own `schema` (docs/design/noc.md); the summary
of `--compare` its own too (README, "What a tool reads is stable").

### The report's fields

`lapilli verdict --json` and `/api/v1/report`. `TestReportFieldsStable`
holds them, and this list.

| Where | Fields |
|---|---|
| the report | `from`, `to`, `noisy_at`, `no_record`†, `unjudged`†, `some_rules`†, `rules`, `incidents`, `firings`, `settings` |
| `noisy_at` | `firings`, `precision` |
| `settings` | `lead_ns`, `match`, `refire_ns`, `unknown_length_ns`, `record_lookback_ns`, `impact_from`†, `impact_no_data`†, `record_by_week`†, `impact_signals`†, `impact_rules`† |
| `settings.impact_signals[]` | `name`, `for_ns`, `match`, `record`, `start`† |
| `settings.impact_rules[]` | `rules`†, `labels`†, `signals`, `match` |
| `rules[]` | `rule`, `severity`†, `firings`, `white`, `black`, `refired`, `noisy`, `unjudged`†, `no_record`†, `replayed`, `unsupported`†, `incidents`, `first`, `alone`, `spare`, `off_hours`, `off_hours_black`, `asleep`, `asleep_black`, `excused`†, `during_deploys`†, `during_deploys_caught`†, `during_deploys_unjudged`†, `impact_rule`†, `marked`† |
| `rules[].marked.*` | `firings`, `caught`, `refired`, `unjudged`† |
| `incidents[]` | `id`, `title`†, `start`, `end`†, `labels`†, `candidate`, `caught_by`, `signal`† |
| `incidents[].caught_by[]` | `rule`, `delay_ns`, `noisy` |
| `firings[]` | `rule`, `labels`†, `start`, `end`†, `white`, `incidents`†, `no_record`†, `refired`†, `during_deploy`†, `signals`†, `marked`† |

### The metrics

`lapilli verdict --metrics`, and `lapilli serve`'s `/metrics` over the last
seven days. `TestMetricsStable` holds each name, and that its series have
these labels and no others. A label in brackets is there only when it has
a value: a rule's `severity` when it has one, its `team` when a team owns
it, which only `lapilli serve` says.

| Metric | Labels |
|---|---|
| `lapilli_rule_firings` | `rule`, [`severity`], [`team`] |
| `lapilli_rule_caught` | `rule`, [`severity`], [`team`] |
| `lapilli_rule_nothing` | `rule`, [`severity`], [`team`] |
| `lapilli_rule_refired` | `rule`, [`severity`], [`team`] |
| `lapilli_rule_incidents` | `rule`, [`severity`], [`team`] |
| `lapilli_rule_incidents_first` | `rule`, [`severity`], [`team`] |
| `lapilli_rule_incidents_alone` | `rule`, [`severity`], [`team`] |
| `lapilli_rule_nothing_by_hours` | `rule`, [`severity`], [`team`], `hours` (`asleep`, `off_hours`, `at_work`) |
| `lapilli_incidents` | `state` (`caught`, `noisy_only`, `missed`), `source` (`written`, `impact`) |
| `lapilli_incident_detection_delay_seconds` | `incident` |
| `lapilli_last_run_timestamp_seconds` | |
| `lapilli_impact_windows` | `signal`, `state` (`caught`, `noisy_only`, `missed`) |
| `lapilli_impact_signal_start_timestamp_seconds` | `signal` |
| `lapilli_rule_firings_during_deploys` | `rule`, [`severity`], [`team`] |
| `lapilli_rule_caught_during_deploys` | `rule`, [`severity`], [`team`] |
| `lapilli_rule_refired_weeks` | `rule`, [`severity`], [`team`] |
| `lapilli_refired_weeks` | [`team`] |
| `lapilli_weeks_counted` | |
| `lapilli_incident_state` | `incident`, `source`, `state` |
| `lapilli_incident_lead_seconds` | `incident`, `source` |
| `lapilli_incident_team` | `incident`, [`team`] |
| `lapilli_rule_notifications` | `rule`, `receiver`, `reason` |
| `lapilli_serve_cursor_timestamp_seconds` | |
| `lapilli_serve_run_duration_seconds` | |
| `lapilli_serve_runs_total` | `kind`, `result` |
| `lapilli_serve_firings_avoided` | |
| `lapilli_serve_nothing_avoided` | |
| `lapilli_serve_rule_changes` | `outcome` |
| `lapilli_serve_incident_last_timestamp_seconds` | |
| `lapilli_serve_edits` | |
| `lapilli_serve_source_last_timestamp_seconds` | `source` |
| `lapilli_serve_source_firings` | `source` |
| `lapilli_serve_source_quiet_seconds` | |
| `lapilli_serve_notifications_since_timestamp_seconds` | |
| `lapilli_serve_notifications_last_timestamp_seconds` | `source` |
| `lapilli_serve_deploys_last_timestamp_seconds` | |
| `lapilli_serve_deploys_read_timestamp_seconds` | |
| `lapilli_serve_incidents_passed_over` | |
| `lapilli_goal_met` | `goal`, [`team`] |
| `lapilli_goal_known` | `goal`, [`team`] |
| `lapilli_goal_value` | `goal`, [`team`] |
| `lapilli_goal_weeks_met` | `goal`, [`team`] |
| `lapilli_rule_firings_marked` | `rule`, [`severity`], [`team`], `kind` |
| `lapilli_rule_caught_marked` | `rule`, [`severity`], [`team`], `kind` |
| `lapilli_marked_on` | `kind` |
| `lapilli_serve_marked_read_timestamp_seconds` | |

The first eleven are `--metrics`' too. The next two are `--metrics`'
alone, with named impact signals, which only `lapilli verdict` reads for
now: each signal's windows by state, and from when it keeps the record
(none for a signal that doesn't, or whose data a selector has none of);
`TestVerdictMetricsStable` holds them. The rest are `lapilli serve`'s
alone. The goals' four (docs/design/goals.md) are of the last whole week,
from Monday in `--tz`, for each goal `--config` declares: everyone's
without `team`, a team's with it, as the leader worked them out after its
last round; `lapilli_goal_met` and `lapilli_goal_value` have no series for
a week not known (`lapilli_goal_known` 0), and `lapilli_goal_weeks_met`
counts of the twelve whole weeks before this one, as
`lapilli_weeks_counted` does. 500 series of each at most, the goals missed
first. `lapilli_rule_incidents_first` and `lapilli_rule_incidents_alone`
are left out of a report of some of the rules (`--rule`), as what a rule
adds to the others can't be said there.

### The CSV's columns

`rule`, `severity`, `team`, `labels`, `start`, `end`, `stone`, `no_record`,
`incidents`, `refired`, `during_deploy`, `excused`, `marked`, in that order.
`TestCSVColumns` holds them.

### lapilli preflight's report

`lapilli preflight --json` (docs/design/preflight.md). Its items' ids, their
states and the fields below keep their names; a new check is a new id.
`TestPreflightStable` holds them, and this list. A field marked † is left
out when it is zero or empty, as in the report.

| Where | Fields |
|---|---|
| the report | `version`, `lapilli`, `items`, `estimates`, `left_out`, `asked` |
| `items[]` | `id`, `state` (`ok`, `problem`, `unknown`), `what`, `why`†, `fix`† |
| `asked` | `requests`, `by`, `refused` |
| `estimates` | `hourly`†, `backfill`†, `nights`†, `beside`†, `memory`† |
| `estimates.hourly` | `runs`, `requests`, `most`, `day`, `by`, `guard`, `impact`, `seconds`†, `share`† |
| `estimates.backfill` | `pieces`, `requests`, `most`, `by`, `guard`, `impact`, `rate`†, `seconds`†, `piece_hours`† |
| `estimates.nights` | `hours`, `nights` (`-1`: never, the catch-up falls further behind), `per_night` |
| `estimates.beside` | `day`, `by` |
| `estimates.memory` | `from` (`alerts`, `caps`), `peak_bytes`, `request_bytes`, `limit_bytes`, `cgroup_bytes`† |

The ids: `rules.read`, `rules.replayable`, `rules.history`, `expressions`,
`history.reach`, `history.loki`, `history.grafana`, `history.alerts`,
`history.intervals`, `grafana.notifications`, `grafana.write`,
`changes.cloud`, `changes.ruler`, `inputs.files`, `secrets.files`,
`ledger.sqlite`, `ledger.postgres`, `signin`, `access`, `estimate.hourly`,
`estimate.backfill`, `estimate.nights`, `estimate.beside`,
`estimate.memory`. The keys of `asked.by` and of the estimates' `by` are the
APIs asked (`prometheus`, `loki`, `grafana`, `ruler`, `alarms:SOURCE`,
`signin`, `on-call`, `credentials`); of `estimates.beside.by`, what asks
(`rules`, `loki_rules`, `alarms`, `check_replay`, `scrape_intervals`,
`grafana_notifications`, `grafana_data_sources`, `on_call`). The texts of
`what`, `why` and `fix` may change in any release.
