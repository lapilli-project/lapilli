# New Relic's NrAiIncident events, open and close, as NerdGraph's NRQL
# gives them, to Lapilli's firings. Each incident is one firing, from when
# it opened (its openTime, or else its open event's time) until it closed
# (closeTime, or else its close event's time); one still open has no end.
# A firing is of the period it opened in, $since to $until (milliseconds):
# one opened before is left out, though it closed in the period.
#
# The rule is the condition's name. Its labels are the condition's ID as
# alarm_id, the incident's priority as severity (critical, high, medium,
# low), the policy's name as policy, the entity's name as entity (or,
# without one, the incident's target), and the incident's tags (tags.team
# gives team); rename them to what --match needs, such as service. Two
# conditions of one name, in two policies, are one rule: tell them apart
# here if yours are. Labels serve wouldn't take are left out: a name or
# value with a character that doesn't show, a value over 256 bytes, past
# 32 labels or 4 KB of them.
# RFC 3339, in UTC, of milliseconds since 1970.
def time: floor | (. / 1000 | floor) as $s | (. - $s * 1000) as $f
  | ($s | todate) | if $f == 0 then . else .[:-1] + "." + ("00" + ($f | tostring))[-3:] + "Z" end;
# An event's time named $name, in milliseconds. One that isn't stops the
# conversion, rather than a firing going missing or moving.
def msof($name): . as $r | .[$name]
  | if type == "number" and . >= 1e12 then floor
    else error("incident \($r.incidentId | tojson): its \($name) is no time in milliseconds") end;
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
[.[] | if (.incidentId // "" | tostring) == "" or (.event | type) != "string"
       then error("an event that isn't NrAiIncident's: no incidentId or event") else . end]
# An incident's first open and first close, in time: the millisecond two
# pages meet at is read twice.
| [group_by(.incidentId | tostring)[]
   | (map(select(.event == "open")) | min_by(.timestamp)) as $open | select($open != null)
   | ($open | if .openTime != null then msof("openTime") else msof("timestamp") end) as $start
   | select($start >= $since and $start < $until)
   | (map(select(.event == "close")) | min_by(.timestamp)) as $close
   | ($close | if . == null then null elif .closeTime != null then msof("closeTime") else msof("timestamp") end) as $end
   | if $end != null and $end < $start then error("incident \($open.incidentId | tojson) closes before it opens") else . end
   | {o: $open, start: $start, end: $end}]
| sort_by(.start, (.o.conditionName // "" | tostring), (.o.incidentId | tostring))[]
| .o as $o
| {rule: ($o.conditionName // "" | tostring
          | if . != "" then . elif $o.conditionId != null then "condition \($o.conditionId)"
            else error("incident \($o.incidentId | tojson) names no condition") end),
   labels: ([{key: "alarm_id", value: ($o.conditionId // "")},
             {key: "severity", value: ($o.priority // "" | tostring | ascii_downcase)},
             {key: "policy", value: ($o.policyName // "")},
             {key: "entity", value: ($o["entity.name"] // $o.targetName // "")}]
            + ([$o | to_entries[] | select(.key | startswith("tags.")) | {key: .key[5:], value}] | sort_by(.key))
            | bounded),
   start: (.start | time)}
  + (if .end != null then {end: (.end | time)} else {} end)
