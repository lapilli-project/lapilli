# Datadog's monitor events, as GET /api/v2/events gives them (each of its
# data), to Lapilli's firings. A monitor's events are each of a group: a
# firing of a group is from an event of status error (Alert) or warn
# (Warn) until its next of status ok (recovered). Another error or warn
# between them, a notification again or Warn to Alert, changes nothing,
# and events of other statuses are passed over. One still on has no end.
# One on already when the period starts is left out, since when it began
# isn't in the period: datadog.sh reads the day before the period, so a
# monitor notifying again in the period is known to be on already. A
# firing is of the period it starts in, $since to $until (milliseconds).
#
# The rule is the monitor's name before its template variables are filled
# in (templated_name), so one monitor is one rule whatever each group's
# notification says. Its labels are the group's tags (the group
# env:prod,service:checkout gives env and service), the monitor's ID as
# alarm_id, its priority as severity (P1 to P5), and the monitor's own tags
# where the group's don't say; rename them to what --match needs, such as
# service. A tag of no value is left out, and so are labels serve wouldn't
# take: a name or value with a character that doesn't show, a value over
# 256 bytes, past 32 labels or 4 KB of them. So is the rule's name, as
# serve takes it: the characters that don't show taken out, and 512 at
# most; with nothing left, the name filled in, and then "monitor ID".
# Seconds since 1970 of an RFC 3339 time, its fraction and zone included.
def epoch: . as $t
  | (capture("^(?<d>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(?<f>\\.[0-9]+)?(?<z>Z|[+-][0-9]{2}:?[0-9]{2})$")
     // error("not a time: \($t | tojson)"))
  | ((.d + "Z") | fromdateiso8601) + (("0" + (.f // "")) | tonumber)
    - (if .z == "Z" then 0 else (.z[0:1] + "1" | tonumber) * ((.z[1:3] | tonumber) * 3600 + (.z[-2:] | tonumber) * 60) end);
# RFC 3339, in UTC, of milliseconds since 1970.
def time: floor | (. / 1000 | floor) as $s | (. - $s * 1000) as $f
  | ($s | todate) | if $f == 0 then . else .[:-1] + "." + ("00" + ($f | tostring))[-3:] + "Z" end;
# An event's time: its milliseconds, or else its RFC 3339 timestamp. One
# with neither, or with a number too small to be milliseconds, stops the
# conversion, rather than a firing going missing or moving.
def evtime: . as $v
  | (if (.attributes.attributes.timestamp | type) == "number" then .attributes.attributes.timestamp
     elif (.attributes.timestamp | type) == "string" then (.attributes.timestamp | epoch) * 1000
     else error("event \($v.id | tojson) has no time") end)
  | if . < 1e12 then error("event \($v.id | tojson): \(tojson) is no time in milliseconds") else floor end;
# key:value tags, a group's written key:value,key:value, as {key, value}s.
def pairs: [.[]? | strings | split(",")[] | index(":") as $i | select($i != null and $i > 0)
  | {key: .[:$i], value: .[$i + 1:]}];
# A group's tags, of the names it gives one value: an event of two groups
# names neither's.
def group: pairs | group_by(.key) | map(select(map(.value) | unique | length == 1) | .[0]);
# A rule's name as serve takes it: the characters that don't show (a line
# break, a zero-width space) taken out, and cut to 512 characters. One
# serve wouldn't take would stop the whole copy.
def rulename: tostring | gsub("[\\p{Cc}\\p{Cf}\\p{Zl}\\p{Zp}]"; "") | .[:512];
# {key, value}s as labels, the first of each name, as serve takes them.
def bounded:
  reduce .[] as $l ({};
    ($l.key | tostring) as $k | ($l.value | tostring) as $v
    | if $k == "" or $v == "" or has($k) or length >= 32 then .
      elif ($k + $v) | test("[\\p{Cc}\\p{Cf}\\p{Zl}\\p{Zp}]") then .
      elif ($k | utf8bytelength) > 128 or ($v | utf8bytelength) > 256 then .
      elif ([to_entries[] | (.key | utf8bytelength) + (.value | utf8bytelength) + 6] | add // 0)
             + ($k | utf8bytelength) + ($v | utf8bytelength) + 6 > 4096 then .
      else .[$k] = $v end);
[.[] | (.attributes.attributes // {}) as $e
  | ($e.monitor_id // $e.monitor.id) as $id | select($id != null and $id != "")
  | {id: ($id | tostring), event: (.id // "" | tostring), t: evtime, status: ($e.status // ""),
     group: (($e.monitor_groups // []) | map(tostring) | sort),
     name: ($e.monitor.templated_name // "" | rulename | if . == "" then $e.monitor.name // "" | rulename else . end),
     priority: ($e.priority // "" | tostring), tags: ($e.monitor.tags // [])}]
| [sort_by(.id, .group, .t, .event) | group_by([.id, .group])[]
  | reduce .[] as $v ({open: null, out: []};
      if $v.status == "error" or $v.status == "warn" then (if .open == null then .open = $v else . end)
      elif $v.status == "ok" then (if .open then .out += [{on: .open, end: $v.t}] else . end) | .open = null
      else . end)
  | (.out + (if .open then [{on: .open}] else [] end))[]
  | select(.on.t >= $since and .on.t < $until)]
| sort_by(.on.t, .on.id, .on.group)[]
| .on as $o
| {rule: (if $o.name != "" then $o.name else "monitor \($o.id)" | rulename end),
   labels: ([{key: "alarm_id", value: $o.id}]
            + (if $o.priority | test("^[1-5]$") then [{key: "severity", value: "P\($o.priority)"}] else [] end)
            + ($o.group | group) + ($o.tags | pairs)
            | bounded),
   start: ($o.t | time)}
  + (if .end then {end: (.end | time)} else {} end)
