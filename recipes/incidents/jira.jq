# Jira's issues, an array of them as GET /rest/api/3/search/jql gives them, to
# Lapilli's incidents. jira.sh passes its settings; with none, an incident
# starts when its issue was created, ends at its resolution date, and its
# components are the label service, as these would say:
#
#   --arg start created           fields it starts at, the first holding a time
#   --arg end resolutiondate      the same for its end
#   --arg labels service=components
#                                 labels from fields, such as
#                                 service=components,team=customfield_10004
#
# A time is RFC 3339, as Jira writes a date-time field's, and one Go reads:
# a real day, an hour to 23, seconds to 59, a zone to 23:59. A date, as a
# date picker's 2026-11-05, is never taken for one: as midnight, or as the
# whole day, it would have every firing that day catch the incident. Nor is
# a date with something after it that isn't a time, such as 2026-11-05 10:30.
# Either says the day, though, so a time in a later field of the list counts
# only on that day, as Jira writes the time: in the time zone of the
# account's profile. A field holding anything else ends the list there.
#
# An issue whose start can't be read is skipped. One whose end can't be (a
# date, anything else, or before its start) is kept with no end, and taken
# to last an hour. Each is in skipped or noend, by its key, with why.
#
# A label takes what Jira gives: a text, a number, a list of them, an
# option's value (a cascading select's parent), a component's name and a
# user's display name; empty ones are left out. So are values serve would
# refuse the incident for: over 256 bytes, past 64 a label, past 4 KB of
# labels in all, as Go writes them. Those are in cut. Jira writes zones as
# +0900; RFC 3339 wants +09:00.
#
# Given null (jq -n), it checks the settings and gives the fields to ask for.
def stop($msg): "jira.sh: \($msg)\n" | halt_error(1);
def strip: sub("\\A\\s+"; "") | sub("\\s+\\z"; "");
def uniq: reduce .[] as $v ({seen: {}, out: []}; if .seen[$v] then . else .seen[$v] = true | .out += [$v] end) | .out;
def field($env):
  strip
  | if test("\\A[A-Za-z][A-Za-z0-9_]{0,63}\\z") then .
    else stop("\($env) names fields by their ids, such as created or customfield_10001, not \(tojson)") end;
def fields($env): split(",") | map(field($env));
def mapping:
  if strip == "" then []
  else split(",") | map(. as $m | strip
    | capture("\\A(?<label>[A-Za-z_][A-Za-z0-9_]{0,127})\\s*=(?<field>.*)\\z")
      // stop("JIRA_LABELS maps labels to fields, such as service=components,team=customfield_10004, not \($m | tojson)")
    | .field |= field("JIRA_LABELS"))
    | if (map(.label) | uniq | length) > 32 then stop("JIRA_LABELS names 32 labels at most, as many as an incident has") else . end
  end;
def epoch:
  capture("\\A(?<d>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(?:\\.[0-9]+)?(?<z>Z|[+-][0-9]{2}:[0-9]{2})\\z")
  | (.d + "Z" | fromdateiso8601) - (if .z == "Z" then 0 else ((.z[1:3] | tonumber) * 3600 + (.z[4:6] | tonumber) * 60) * (if .z[0:1] == "-" then -1 else 1 end) end);
# A day, or a time to the second, that comes back the same from seconds since
# 1970: so not 2026-02-30, nor 24:00 or :60.
def real: (if length == 10 then . + "T00:00:00Z" else . + "Z" end) as $u
  | [try ($u | fromdateiso8601 | todateiso8601) catch empty] == [$u];
def zone: sub("(?<h>[+-][0-9]{2}):?(?<m>[0-9]{2})\\z"; "\(.h):\(.m)");
# What a field holds: {time}, RFC 3339; {date, exact}, a day and no time
# that can be read, exact when the day is all there is; {other}, something
# else; or {} for nothing.
def when:
  if . == null or . == "" then {}
  elif type != "string" then {other: true}
  elif test("\\A[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\\.[0-9]{1,9})?(Z|[+-][0-9]{2}:?[0-9]{2})\\z")
    and (zone | (.[:19] | real) and (endswith("Z") or ((.[-5:-3] | tonumber) <= 23 and (.[-2:] | tonumber) <= 59))) then
    {time: zone}
  elif test("\\A[0-9]{4}-[0-9]{2}-[0-9]{2}") and (.[:10] | real) then {date: .[:10], exact: (length == 10)}
  else {other: true} end;
# The first time of $names, the fields of $f, and the field it is in; or why
# there is none: {} when none holds anything.
def pick($f; $names; $what):
  reduce ($names[] | {name: ., w: ($f[.] | when)}) as $x ({};
    if .time or .other then .
    elif $x.w.time then
      if .day and $x.w.time[:10] != .day then .off = (.off // $x.name)
      else .time = $x.w.time | .field = $x.name end
    elif $x.w.date then
      if .day then . else .day = $x.w.date | .dayfield = $x.name | .exact = $x.w.exact end
    elif $x.w.other then .other = $x.name
    else . end)
  | if .time then {time, field}
    elif .dayfield then {why: ("the \($what) field \(.dayfield) holds a date and "
      + (if .exact then "no time" else "nothing that reads as a time" end)
      + (if .off then ", and \(.off) is on another day" else "" end))}
    elif .other then {why: "the \($what) field \(.other) holds something other than a time"}
    else {} end;
def text:
  if type == "object" then .value // .name // .displayName else . end
  | select(type == "string" or type == "number") | tostring | select(. != "");
def texts: if type == "array" then .[] | text else text end;
# Bytes as serve keeps labels: Go writes U+2028 and U+2029 as \u2028, \u2029.
def size: tojson | utf8bytelength + 3 * ([match("[\u2028\u2029]"; "g")] | length);
def labelled($f; $labels):
  [$labels[] as $m | $f[$m.field] | texts | {label: $m.label, value: .}]
  | reduce .[] as $p ({labels: {}, seen: {}, long: 0, many: 0, big: 0};
      if .seen[$p.label][$p.value] then .
      else .seen[$p.label][$p.value] = true
        | if ($p.value | utf8bytelength) > 256 then .long += 1
          elif (.labels[$p.label] | length) >= 64 then .many += 1
          elif (.labels | .[$p.label] += [$p.value] | size) > 4096 then .big += 1
          else .labels[$p.label] += [$p.value] end
      end)
  | {labels, cut: ([if .long > 0 then "\(.long) over 256 bytes" else empty end,
      if .many > 0 then "\(.many) past 64 a label" else empty end,
      if .big > 0 then "\(.big) past 4 KB in all" else empty end] | join(", "))};

($ARGS.named.start // "created" | fields("JIRA_START_FIELD")) as $start
| ($ARGS.named.end // "resolutiondate" | fields("JIRA_END_FIELD")) as $end
| ($ARGS.named.labels // "service=components" | mapping) as $labels
| if . == null then ["summary"] + $start + $end + [$labels[].field] | uniq | join(",")
  else [.[]
    | .key as $key
    | (.fields // {}) as $f
    | pick($f; $start; "start") as $s
    | pick($f; $end; "end") as $e
    | if $s.time == null then {skip: {key: $key, why: ($s.why // "no start in \($start | join(" or "))")}}
      else
        (if $e.time and ($e.time | epoch) < ($s.time | epoch) then
          {why: "the end field \($e.field) is before the start field \($s.field)"} else $e end) as $e
        | labelled($f; $labels) as $l
        | {incident: ({id: $key, title: ($f.summary // "")[:120], start: $s.time}
            + (if $e.time then {end: $e.time} else {} end)
            + (if $l.labels == {} then {} else {labels: $l.labels} end))}
        + (if $e.why then {noend: {key: $key, why: $e.why}} else {} end)
        + (if $l.cut != "" then {cut: {key: $key, why: $l.cut}} else {} end)
      end] as $all
  | {incidents: [$all[].incident | values], skipped: [$all[].skip | values],
     noend: [$all[].noend | values], cut: [$all[].cut | values], found: length}
  end
