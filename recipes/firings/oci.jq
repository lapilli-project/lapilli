# OCI Monitoring's alarm history ($entries, state transitions, as the CLI's
# get-alarm-history gives them) to Lapilli's firings: from each transition to
# Firing until the next one from it. OCI says which in words only ("State
# transitioned from OK to Firing"), so that is what is read. An alarm still
# firing at the end has no end; one already firing when the period starts is
# left out, since when it began isn't in the period's history. A firing is
# of the period it was triggered in: one triggered before $since or from
# $until on is another period's, though written down in this one.
#
# The rule is the alarm's display name ($alarms, alarm list). Its severity
# (critical, error, warning, info) and metric namespace become labels, and
# its OCID, alarm_id, by which lapilli serve's /v1/oci ties the alarm's
# notifications to its firings, whatever their title, and an alarm it reads
# with --alarms to its history; rename the others to what --match needs. The history is for the alarm as a whole, not
# for each metric stream: firing while any stream is.
# Seconds since 1970 of an RFC 3339 time, its fraction and zone included, so
# times written differently still sort and compare as times.
def epoch: . as $t
  | (capture("^(?<d>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(?<f>\\.[0-9]+)?(?<z>Z|[+-][0-9]{2}:?[0-9]{2})$")
     // error("not a time: \($t)"))
  | ((.d + "Z") | fromdateiso8601) + (("0" + (.f // "")) | tonumber)
    - (if .z == "Z" then 0 else (.z[0:1] + "1" | tonumber) * ((.z[1:3] | tonumber) * 3600 + (.z[-2:] | tonumber) * 60) end);
def lower: ascii_downcase;
(($alarms[0].data // []) | map({key: .id, value: .}) | from_entries) as $byId
| [$entries[]
    | (.summary // "" | lower) as $s
    | (.["timestamp-triggered"] // .timestamp) as $at
    | {alarm, at: $at, t: ($at | epoch), to: ($s | test("to firing")), from: ($s | test("from firing"))}]
| sort_by(.alarm, .t) | group_by(.alarm)[]
| .[0].alarm as $id | ($byId[$id] // {}) as $a
| reduce .[] as $t ({open: null, out: []};
    if $t.to and ($t.from | not) then .open = $t.at
    elif $t.from and ($t.to | not) then
      (if .open then .out += [{start: .open, end: $t.at}] else . end) | .open = null
    else . end)
| (.out + (if .open then [{start: .open}] else [] end))[]
| select((.start | epoch) >= ($since | epoch) and ($until == "" or (.start | epoch) < ($until | epoch)))
| {rule: ($a["display-name"] // $id),
   labels: ({severity: (($a.severity // "") | lower), namespace: ($a.namespace // ""), alarm_id: $id} | with_entries(select(.value != "")))} + .
