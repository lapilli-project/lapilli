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
`linked_only`†, `refire_ns`, `unknown_length_ns`, `record_lookback_ns`, `impact_from`†,
`impact_no_data`†, `record_by_week`†, `impact_signals`†, `impact_rules`†),
and the zone days are counted in (`zone`†, which parts episodes, below):
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
matched against it on `match`. A window of a signal not in effect, its
`signal` none of the names in `settings.impact_signals` (a signal taken
away on the Settings page, dropped by a start without it, or set aside at
a start past eight; [design/judging.md](design/judging.md), item 5), and
the incident a confirmation of one wrote, which is caught as its window
was, are caught only by the rules no entry names, on the labels the
window was found with: its signal's `match` then, which the report
doesn't give, so the block below counts such a window on `match`.

A signal may give a lead of its own, `lead_ns`† in
`settings.impact_signals`†, and a rule entry one for its rules against its
signals' windows, `lead_ns`† in `settings.impact_rules`†
([design/judging.md](design/judging.md), item 1): a firing catches a window
of a signal (or the incident a confirmation of one wrote, which carries
its `signal`) from its entry's lead before it, else its signal's, else
`lead`. Every other incident, and every incident written down, is caught
from `lead`. An entry's `require`† are labels a firing of its rules and a
window of its signals must both carry, or it doesn't catch that window.

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
       | ($in.signal != null and $sig[$in.signal] != null) as $named
       | (if $named and $e != null and $e.lead_ns != null then $e.lead_ns
          elif $named then $sig[$in.signal].lead_ns // $s.lead_ns
          else $s.lead_ns end) as $lead
       | (if $named and $e != null then $e.require // [] else [] end) as $require
       | select(($in.signal == null or $e == null or ($named and any($e.signals[]; . == $in.signal)))
         and $at >= ($in.start | epoch) - $lead / 1e9
         and $at <= (if $in.end then $in.end | epoch else ($in.start | epoch) + $s.unknown_length_ns / 1e9 end)
         and all($require[]; ($f.labels // {})[.] != null and ($in.labels // {})[.] != null)
         and all($match[]; ($f.labels // {})[.] as $v | ($in.labels // {})[.] as $vs
               | $v == null or $vs == null or any($vs[]; . == $v)))
       | .id]]
```

In `lapilli serve`, a firing near the start of the period may have caught
an incident that started before it, which the report's `incidents` leave
out: count again over a period that starts the longest lead earlier.

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

A firing a person said is **unrelated** to what it caught (`firings[].unrelated`†,
the incidents it caught as Lapilli judged it that they named; lapilli
serve, [design/judging.md](design/judging.md), item 3) doesn't catch
those: it is a black stone when it caught nothing else, and what caught
each of them is worked out from the other firings. A rule entry may say
`judged: false`† (`settings.impact_rules[].judged`): its rules' firings
catch nothing, written incidents included, and are counted, not judged
(`firings[].not_judged`†, `no_record`†). "A person's verdict", below,
counts both again.

## No record

A black stone says nothing was wrong, which is only so if someone was
writing down what went wrong. A firing is **judged** when the record was
kept at its time: an incident was on at some time in the two weeks
(`record_lookback`) before it, or an impact signal (`--impact`) was read
over its time (`impact_from`† at or before it), or it caught an incident.
`impact_from` is where the signal was first read, or later, where the
data of every selector it reads begins, as a named signal's `start`
(below); it is missing, with `impact_no_data: true`†, when a selector it
reads has no data, which keeps no record. But a selector that only
narrows another the expression reads (the same offset and @, and each of
the other's matchers, its metric's name among them, one of its own) is
read whenever the widest of those is, so long as the labels it narrows
by are there: it begins where that one's series with its metric's name
and each label it narrows them by, any value, begin, when that is before
its own data or it has none, as `http_requests_total{code!=""}` for
`http_requests_total{code=~"5.."}` beside `http_requests_total`. A
client library makes a 5xx series at the first 5xx, so a service that
served none had nothing to match, not nothing to read; and a code label
that came later (a relabelling, a rename) holds the record back to where
it came. One read with nothing wider beside it keeps its own start. Not
a selector an `absent()` or `absent_over_time()` reads, at any depth.
The incidents that keep the record are all those given, from before the
period too. Any other firing is black, with `no_record: true`†: counted,
in `firings` and `black`, and in its rule's `unjudged`†, but in no
precision, and it makes no rule noisy. A rule none of whose firings is
judged has `no_record: true`†; a report none of whose firings could be,
too.

With named impact signals, a firing's record is kept by what could have
seen the incident it would catch: its `signals`†. They are its rule
entry's; else each signal that, for every label of the signal's `match`
the firing has, has seen the firing's value of it: one the series its
expression reads have had over the read (each selector counted by those
labels, one that only narrows another through the selector it narrows; a
label none of them carries, and every label of an expression that reads
no selector, has every value seen), or one it returned (a series returned
without that label has every value seen); more than 1,000 values of a
label, or 256 KB in all, has every value seen; and, when none of those
keeps the record (`record`), every signal that does too. Such a firing is judged
when an incident written down was on in the two weeks before it, or a
window of one of its `signals` was, or it caught an incident, or the
signals among its `signals` that keep the record, one at least, had each
started by its time: `settings.impact_signals[].start`†, where the signal
was first read, or later, where the data of every selector it reads
begins (none for a selector with no data, as above, but by its stand-in).
`impact_from` is then missing:
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
a week tied to the same signals, of a rule no entry names, or of the same
rule entry, is judged, or none; with no named signal, every firing of the
week. A window keeps the record of the firings tied to its signal alone;
an incident written down, or written by confirming a window, keeps
everyone's. A window of a signal not in effect (its `signal` none of the
names in `settings.impact_signals`: taken away, dropped by a start, or set
aside past eight; [design/judging.md](design/judging.md), item 5) keeps no
rule's record, with named signals in effect or none, as no firing is
tied to its signal: a rule's record in its weeks is what the signals in
effect and the incidents written down keep, as `lapilli verdict`, which
never read that signal, judges it, and the block below leaves it out.
Confirmed, it is written down, and keeps everyone's. With named signals,
a signal's start is the first whole week
after where its data begins (`settings.impact_signals[].start`), and a
firing is read as judged when each of its `signals` that keeps the record
had started by its week's Monday. Give the time zone's offset from UTC, in
seconds, as `--argjson offset`, and the incidents of the two weeks before
the period as `earlier` (above):

```jq
# no record (lapilli serve): as the report has it
[.firings[] | .no_record // false]
```

```jq
# no record (lapilli serve): counted again
def epoch: capture("^(?<t>.{19})(?<f>\\.[0-9]+)?(?<z>Z|[+-][0-9]{2}:[0-9]{2})$")
  | (.t + "Z" | fromdateiso8601) + ((.f // "0") | tonumber)
    - (if .z == "Z" then 0 else (if .z[0:1] == "-" then -1 else 1 end) * ((.z[1:3] | tonumber) * 3600 + (.z[4:6] | tonumber) * 60) end);
.settings as $s
| (($s.impact_signals // []) | map({key: .name, value: .}) | from_entries) as $sig
| (.incidents + ($ARGS.named.earlier // [])) as $incs
| ($ARGS.named.offset // 0) as $off
| [.firings[] | (.start | epoch) as $at
   | ((($at + $off) / 86400 | floor) as $d | ($d - ($d + 3) % 7) * 86400 - $off) as $w
   | (.signals // []) as $ties
   | [$ties[] | $sig[.] | select(.record)] as $waits
   | (.white | not)
     and ($s.impact_from == null or ($s.impact_from | epoch) > $w)
     and ($waits == [] or any($waits[]; .start == null or (.start | epoch) > $w))
     and (any($incs[];
            (.candidate != true or .signal == null or (.signal as $x | any($ties[]; . == $x)))
            and (.start | epoch) < $w + 7 * 86400
            and (if .end then .end | epoch else (.start | epoch) + $s.unknown_length_ns / 1e9 end)
                >= $w - $s.record_lookback_ns / 1e9)
          | not)]
```

`/api/v1/summary` gives each week's span, `no_record` (no firing of the
row was judged; with no named signal, the week had no record) and
`unjudged`, its firings counted and not judged, as the report counts them:

```jq
# unjudged in a week: as the summary has it
first(.rows[] | select(.team == "")) | .unjudged
```

```jq
# unjudged in a week: counted again
[.firings[] | select(.no_record)] | length
```

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

## A person's verdict (lapilli serve)

On Alert history, and through `/api/v1/verdicts`, a person says of one
firing that it is **unrelated** to the incidents it caught, which turns
that firing alone black (its `unrelated`† names them, and its
`incidents`† are those it still catches), and whether it **needed
action**, `yes` or `no` (`action`†), which changes no stone
([design/judging.md](design/judging.md), item 3). An incident "This was
real" wrote from a firing is `real`†. The report's stones, and every number
here, keep people's word in them; the summary's `people` says it apart:

- `unrelated`, the row's firings white as Lapilli judged them and black by
  a person's word, and `unrelated_excused`, those of them a quiet window
  then excused, which are in no other count, as every excused firing. As
  Lapilli judged it, a firing is white when it caught an incident "This
  was real" didn't write, whatever it was said unrelated to: one said
  unrelated to such an incident alone isn't among them;
- `real`, the incidents "This was real" wrote from a firing of the row's
  rules that started in the week; `real_white`, the row's firings white
  only by such incidents, and `real_excused`, those of them a quiet window
  that matches them would have excused as Lapilli judged them;
- `caught_by_lapilli` and `precision_by_lapilli`, as Lapilli's own
  judging would have them: every "unrelated" white, those excused for it
  counted again, the incidents "This was real" wrote left out (their
  firings black, excused, or not judged where only such an incident kept
  the record);
- `action`: the row's `firings`, those `answered`, `yes` and `no`,
  `yes_black` (needed action, caught nothing) and `no_white` (needed none,
  caught something), and `share`, yes over those answered, given only once
  80% of the row's firings are answered, `null` below it.

A rule's `unrelated`†, `action_yes`† and `action_no`† count its firings so;
`not_judged`† those its entry judges by nothing, of its `unjudged`†.

```jq
# people (lapilli serve): as the summary has it
first(.rows[] | select(.team == "")) | .people
| {unrelated: (.unrelated - .unrelated_excused), caught_by_lapilli: (.caught_by_lapilli - .unrelated_excused), yes: .action.yes, no: .action.no}
```

```jq
# people (lapilli serve): counted again
([.incidents[] | select(.real) | .id]) as $real
| {unrelated: ([.firings[] | select((.white | not) and (((.unrelated // []) - $real) != []))] | length),
   caught_by_lapilli: ([.firings[] | select((((.incidents // []) + (.unrelated // [])) - $real) != [])] | length),
   yes: ([.firings[] | select(.action == "yes")] | length),
   no: ([.firings[] | select(.action == "no")] | length)}
```

Counted again from `/api/v1/report` over the same week, of the firings it
gives, which a quiet window didn't excuse.

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

## Episodes

An **episode** is one problem however many alerts it is
(docs/design/judging.md, item 2): the firings of one rule with the same
values of the match labels (`settings.match`, a label a firing doesn't
carry counted as none), each starting while another of them is on, or
within `refire` of the latest end among them so far. The first such
firing of a day starts a new one: a firing's `day`† is the day its start
falls on in `settings.zone`† (`lapilli verdict`'s `--tz`, the machine's,
`Local`, when it isn't given; `lapilli serve`'s `--tz`). The labels that
tell alerts apart and aren't matched on, a `pod`, an `instance`, don't
part them; with no match, every overlapping firing of a rule is one.
`lapilli serve` leaves out the firings a quiet window excused, which are
in no number.

An episode is **white** when any of its firings is white; else judged,
and black, when its first firing was judged, and not judged
(`unjudged`†) when it wasn't. Its day, its week, its marks and its record
are its first firing's. A rule's `episodes`† are `total`, `white`,
`unjudged`† and `noisy`: noisy by its episodes, by the same thresholds as
by its firings (`noisy_at`). Episodes are counted beside the firings and
change no other number: `noisy` stays by firings, and decides what it
decides; `episodes.noisy` is shown, and decides nothing. Each firing's
`episode`† numbers the episode it is of, from 1 in the report, in order
of their first firing.

```jq
# episodes: as the report has it
[.rules[] | select(.episodes) | {rule, total: .episodes.total, white: .episodes.white, unjudged: (.episodes.unjudged // 0), noisy: .episodes.noisy}]
| sort_by(.rule)
```

```jq
# episodes: counted again
.noisy_at as $at
| [.firings | map(select(.episode)) | group_by(.episode)[] | sort_by(.start)
   | {rule: .[0].rule, white: any(.white), judged: (any(.white) or (.[0].no_record | not))}]
| group_by(.rule)
| map({rule: .[0].rule, total: length, white: map(select(.white)) | length, unjudged: map(select(.judged | not)) | length}
      | (.total - .unjudged) as $j | . + {noisy: ($j > 0 and $j >= $at.firings and .white / $j < $at.precision)})
| sort_by(.rule)
```

Which firings make one, from each firing's rule, labels, start, end and
`day`, with `settings.match` and `settings.refire_ns`:

```jq
# which firings make an episode: as the report has it
[.firings[] | .episode]
```

```jq
# which firings make an episode: counted again
def epoch: capture("^(?<t>.{19})(?<f>\\.[0-9]+)?(?<z>Z|[+-][0-9]{2}:[0-9]{2})$")
  | (.t + "Z" | fromdateiso8601) + ((.f // "0") | tonumber)
    - (if .z == "Z" then 0 else (if .z[0:1] == "-" then -1 else 1 end) * ((.z[1:3] | tonumber) * 3600 + (.z[4:6] | tonumber) * 60) end);
.settings.match as $m | (.settings.refire_ns / 1e9) as $refire
| reduce .firings[] as $f ({walks: {}, n: 0, out: []};
    ([$f.rule, ($m[] as $k | if ($f.labels // {}) | has($k) then "=" + $f.labels[$k] else null end)] | tojson) as $key
    | ($f.start | epoch) as $s | (if $f.end then $f.end | epoch else null end) as $e
    | .walks[$key] as $w
    | if $w == null or $w.day != $f.day or (($w.on | not) and $s - $w.until > $refire)
      then .n += 1 | .walks[$key] = {n: .n, day: $f.day, until: $e, on: ($e == null)}
      else .walks[$key] |= (if $e == null then .on = true elif $e > .until then .until = $e else . end)
      end
    | .out += [.walks[$key].n])
| .out
```

`lapilli verdict` sees the firings of the period alone, so an episode it
counts begins in it. `lapilli serve` keeps each firing's episode, walked
over whole days: a period that begins inside a day may begin inside an
episode, which the report numbers from its first firing in the period and
leaves out of `rules[].episodes`, counted where its first firing is; count
from a period of whole days. A ledger upgraded to this release, or opened
in another `--tz`, counts its episodes again a week at a time, the newest
first: until it has, a report of those weeks has no `episodes` and no
`episode`, the summary's rows `"episodes": null`.

`/api/v1/summary`'s `episodes` is a week's: `total`, `caught` (white),
`unjudged`, `precision` (null as the row's is) and `noisy_rules`, how many
of the row's rules are noisy by their episodes. Count it again from
`/api/v1/report` over the same week:

```jq
# episodes in a week: as the summary has it
first(.rows[] | select(.team == "")) | .episodes | {total, caught, unjudged, precision}
```

```jq
# episodes in a week: counted again
[.firings | map(select(.episode)) | group_by(.episode)[] | sort_by(.start)
 | {white: any(.white), judged: (any(.white) or (.[0].no_record | not))}]
| {total: length, caught: map(select(.white)) | length, unjudged: map(select(.judged | not)) | length}
| (.total - .unjudged) as $j | . + {precision: (if $j > 0 then .caught / $j else null end)}
```

`/metrics` gives each rule's episodes over the last seven days, and those
caught (`lapilli_rule_episodes`, `lapilli_rule_episodes_caught`), as
`lapilli verdict --metrics` over its period; the CSV each firing's
`episode`.

## Incidents missed, and caught only by noisy rules

An incident is **missed** when no firing caught it (`caught_by` empty). It
was **caught only by noisy rules** when every rule that caught it is noisy:
little better than missed. `caught_by[]` lists each rule that caught it,
with `delay_ns`, from the incident's start to the rule's first firing that
caught it (negative when the alert came first), the earliest first, and
`linked`† when a firing of the rule that caught it is one of the alerts the
incident's tool linked to it (the incident's `alerts`†,
[How it judges](how-it-judges.md)). A missed incident has `not_known`†
when a rule couldn't be replayed (its `unsupported`, not judged from its
history): that rule may have caught it, so the report says the incident
"can't be judged", not that it had no alert. It is still missed here, and
in `lapilli_incidents_not_known`.

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

A row's `goals` (docs/design/goals.md) hold, besides each goal of each
week, a goal over a period once for each occurrence of its kind that
ended in the week, or is on now in this one: with `each`, `occurrence`,
`title`, `from` and `to`, and, of `no_worse_than`, `by` and `before`. Its
`value` is the measure inside the occurrence, as the Marked windows page
counts it, which `marked` doesn't give apart when two occurrences of a
kind meet one week.

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
| `episode` | the episode it is of, as `firings[].episode`; empty for an excused firing, or with episodes not counted |

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
`unreplayed`, `hours`, `impact_short`, a rule's `unseen`, `no_data`, `for_outlasts`
and `late`, an incident's `fired_after`, a firing's `after` and
`held_from`, the report's `late_cut`, and the
metrics of `lapilli serve` about itself that aren't below) may change in a
minor release, said in the changelog.

`/api/v1/summary` keeps its own `schema` (docs/design/noc.md); the summary
of `--compare` its own too ([Pull request backtests in CI](pr-backtests.md), "What a tool reads is
stable").

### The report's fields

`lapilli verdict --json` and `/api/v1/report`. `TestReportFieldsStable`
holds them, and this list.

| Where | Fields |
|---|---|
| the report | `from`, `to`, `noisy_at`, `no_record`†, `unjudged`†, `some_rules`†, `rules`, `incidents`, `firings`, `settings` |
| `noisy_at` | `firings`, `precision` |
| `settings` | `lead_ns`, `match`, `linked_only`†, `refire_ns`, `unknown_length_ns`, `record_lookback_ns`, `impact_from`†, `impact_no_data`†, `record_by_week`†, `impact_signals`†, `impact_rules`†, `zone`† |
| `settings.impact_signals[]` | `name`, `for_ns`, `match`, `record`, `start`†, `lead_ns`† |
| `settings.impact_rules[]` | `rules`†, `labels`†, `signals`, `match`, `lead_ns`†, `require`†, `judged`† |
| `rules[]` | `rule`, `severity`†, `firings`, `white`, `black`, `refired`, `noisy`, `unjudged`†, `no_record`†, `replayed`, `unsupported`†, `incidents`, `first`, `alone`, `spare`, `off_hours`, `off_hours_black`, `asleep`, `asleep_black`, `excused`†, `during_deploys`†, `during_deploys_caught`†, `during_deploys_unjudged`†, `impact_rule`†, `marked`†, `episodes`†, `unrelated`†, `action_yes`†, `action_no`†, `not_judged`† |
| `rules[].marked.*` | `firings`, `caught`, `refired`, `unjudged`† |
| `rules[].episodes` | `total`, `white`, `unjudged`†, `noisy` |
| `incidents[]` | `id`, `title`†, `start`, `end`†, `labels`†, `alerts`†, `candidate`, `caught_by`, `signal`†, `real`†, `not_known`† |
| `incidents[].alerts[]` | `rule`, `labels`† |
| `incidents[].caught_by[]` | `rule`, `delay_ns`, `noisy`, `linked`† |
| `firings[]` | `rule`, `labels`†, `start`, `end`†, `white`, `incidents`†, `no_record`†, `refired`†, `during_deploy`†, `signals`†, `marked`†, `episode`†, `day`†, `unrelated`†, `action`†, `not_judged`† |

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
| `lapilli_incidents_not_known` | `source` (`written`, `impact`) |
| `lapilli_incident_detection_delay_seconds` | `incident` |
| `lapilli_last_run_timestamp_seconds` | |
| `lapilli_rule_episodes` | `rule`, [`severity`], [`team`] |
| `lapilli_rule_episodes_caught` | `rule`, [`severity`], [`team`] |
| `lapilli_rule_action` | `rule`, [`severity`], [`team`], `answer` (`yes`, `no`): `lapilli serve` alone |
| `lapilli_impact_windows` | `signal`, `state` (`caught`, `noisy_only`, `missed`) |
| `lapilli_impact_signal_start_timestamp_seconds` | `signal` |
| `lapilli_rule_firings_during_deploys` | `rule`, [`severity`], [`team`] |
| `lapilli_rule_caught_during_deploys` | `rule`, [`severity`], [`team`] |
| `lapilli_rule_refired_weeks` | `rule`, [`severity`], [`team`] |
| `lapilli_refired_weeks` | [`team`] |
| `lapilli_weeks_counted` | |
| `lapilli_incident_state` | `incident`, `source`, [`signal`], `state` |
| `lapilli_incident_lead_seconds` | `incident`, `source`, [`signal`] |
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
| `lapilli_expectations_held` | |
| `lapilli_expectations_broken` | |
| `lapilli_rule_expectations_broken` | `rule` |
| `lapilli_rule_firings_marked` | `rule`, [`severity`], [`team`], `kind` |
| `lapilli_rule_caught_marked` | `rule`, [`severity`], [`team`], `kind` |
| `lapilli_marked_on` | `kind` |
| `lapilli_serve_marked_read_timestamp_seconds` | |

The first thirteen are `--metrics`' too. The next two are there with named
impact signals, in `--metrics` and in `lapilli serve`'s `/metrics` (over
its last seven days): each signal's windows by state, and from when it
keeps the record (none for a signal that doesn't, or whose data a
selector has none of); `TestVerdictMetricsStable` and
`TestSignalMetricsStable` hold them. The rest are `lapilli serve`'s
alone; `lapilli_incident_state` and `lapilli_incident_lead_seconds` have
`signal` on a named signal's windows, one incident having one signal. The goals' four (docs/design/goals.md) are of the last whole week,
from Monday in `--tz`, for each goal `--config` declares: everyone's
without `team`, a team's with it, as the leader worked them out after its
last round; `lapilli_goal_met` and `lapilli_goal_value` have no series for
a week not known (`lapilli_goal_known` 0), and `lapilli_goal_weeks_met`
counts of the twelve whole weeks before this one, as
`lapilli_weeks_counted` does. A goal over a period (`each: KIND`) is of
its latest occurrence that ended, and `lapilli_goal_weeks_met` counts its
occurrences met that ended in those twelve weeks, so, of a kind more often
than weekly, it can be over `lapilli_weeks_counted`. 500 series of each at
most, the goals missed first, goals over a period among them. `lapilli_rule_incidents_first` and `lapilli_rule_incidents_alone`
are left out of a report of some of the rules (`--rule`), as what a rule
adds to the others can't be said there.

### The CSV's columns

`rule`, `severity`, `team`, `labels`, `start`, `end`, `stone`, `no_record`,
`incidents`, `refired`, `during_deploy`, `excused`, `marked`, `episode`,
`unrelated`, `action`, in that order. `TestCSVColumns` holds them.
`unrelated` is the JSON list of the incidents a person said the firing is
unrelated to (`firings[].unrelated`), `action` their answer to whether it
needed someone to act (`yes`, `no`, or empty).

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
| `estimates` | `hourly`†, `backfill`†, `nights`†, `beside`†, `backtests`†, `memory`† |
| `estimates.hourly` | `runs`, `requests`, `most`, `day`, `by`, `guard`, `impact`, `seconds`†, `share`†, `while_backtest`† |
| `estimates.hourly.while_backtest` | `rate`, `seconds`, `share`†: with `--backtest-listen` and `--max-rate`, the runs while a backtest runs the whole hour |
| `estimates.backfill` | `pieces`, `requests`, `most`, `by`, `guard`, `impact`, `rate`†, `seconds`†, `piece_hours`† |
| `estimates.nights` | `hours`, `nights` (`-1`: never, the catch-up falls further behind), `per_night` |
| `estimates.beside` | `day`, `by` |
| `estimates.backtests` | `share`, `rate`†, `parallel`, `timeout_seconds`, `fit`†, `rules`, `days`, `changed`, `shared`, `changed_days`, `shared_days` (`0`: not a day fits), `week_rules`†, `day_rules`† (how many of the rules changed fit 7 days, and a day, when all of them don't) |
| `estimates.memory` | `from` (`alerts`, `caps`), `peak_bytes`, `request_bytes`, `limit_bytes`, `cgroup_bytes`†, `backtest_bytes`† |

The ids: `rules.read`, `rules.replayable`, `rules.sources`, `rules.history`, `expressions`,
`history.reach`, `history.loki`, `history.grafana`, `history.alerts`,
`history.intervals`, `grafana.notifications`, `content.alertmanager`,
`grafana.write`, `changes.cloud`, `changes.aws`, `changes.azure`, `changes.google`,
`changes.oci`, `changes.ruler`, `changes.loki`, `inputs.files`, `secrets.files`,
`ledger.sqlite`, `ledger.postgres`, `signin`, `backtest.trust`,
`backtest.issuers`, `access`, `estimate.hourly`, `estimate.backfill`,
`estimate.nights`, `estimate.beside`, `estimate.backtests`,
`estimate.memory`. The keys of `asked.by` and of the estimates' `by` are the
APIs asked (`prometheus`, `loki`, `grafana`, `ruler`, `alertmanager` (a Mimir's
or a Cortex's `--alertmanager-config`), `alarms:SOURCE`, `signin`, `on-call`, `credentials`, `backtest` (the backtest port's
issuers' discovery documents and keys), and what a cloud's identity may write
is asked of, `iam:aws`, `iam:azure`, `iam:google`, `iam:oci`); of `estimates.beside.by`, what asks
(`rules`, `loki_rules`, `alarms`, `check_replay`, `scrape_intervals`,
`grafana_notifications`, `alertmanager_config`, `grafana_data_sources`,
`on_call`). The texts of `what`, `why` and `fix` may change in any release.
