# CloudWatch's alarm history ($history, describe-alarm-history) to Lapilli's
# firings: each move into ALARM until the next move out of it. An alarm still
# in ALARM at the end has no end; one already in ALARM when the period starts
# is left out, since when it began isn't in the period's history.
#
# An alarm's dimensions ($alarms, describe-alarms) become its labels; rename
# them to what --match needs, such as service. CloudWatch alarms have no
# severity. For an alarm on several series (PromQL, Metrics Insights GROUP
# BY), this is the alarm as a whole: in ALARM while any series is.
# Seconds since 1970 of an RFC 3339 time, its fraction and zone included, so
# times written differently still sort and compare as times.
def epoch: . as $t
  | (capture("^(?<d>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(?<f>\\.[0-9]+)?(?<z>Z|[+-][0-9]{2}:?[0-9]{2})$")
     // error("not a time: \($t)"))
  | ((.d + "Z") | fromdateiso8601) + (("0" + (.f // "")) | tonumber)
    - (if .z == "Z" then 0 else (.z[0:1] + "1" | tonumber) * ((.z[1:3] | tonumber) * 3600 + (.z[-2:] | tonumber) * 60) end);
def dims: reduce (.Dimensions // [])[] as $d ({}; .[$d.Name] = ($d.Value | tostring));
# The move an item records: from HistoryData, or else its summary ("Alarm
# updated from OK to ALARM"). One that says neither stops the conversion,
# rather than a firing going missing.
def move: (try (.HistoryData | fromjson | {from: .oldState.stateValue, to: .newState.stateValue}) catch null) as $d
  | if $d.from and $d.to then $d
    else (.HistorySummary // "" | capture("from (?<from>[A-Z_]+) to (?<to>[A-Z_]+)"))
      // error("\(.AlarmName): a state change that says neither from nor to: \(.HistorySummary)")
    end;
($alarms[0] | [(.MetricAlarms // [])[] | {key: .AlarmName, value: dims}] | from_entries) as $labels
| [($history[0].AlarmHistoryItems // [])[]
    | select(.HistoryItemType == "StateUpdate")
    | move as $m
    | {alarm: .AlarmName, at: .Timestamp, t: (.Timestamp | epoch), from: $m.from, to: $m.to}]
| sort_by(.alarm, .t) | group_by(.alarm)[]
| .[0].alarm as $alarm
| reduce .[] as $t ({open: null, out: []};
    if $t.to == "ALARM" and $t.from != "ALARM" then .open = $t.at
    elif $t.from == "ALARM" and $t.to != "ALARM" then
      (if .open then .out += [{start: .open, end: $t.at}] else . end) | .open = null
    else . end)
| (.out + (if .open then [{start: .open}] else [] end))[]
| {rule: $alarm, labels: ($labels[$alarm] // {})} + .
