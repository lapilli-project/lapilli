# Azure Monitor's alerts, as azure-monitor.sh's query projects them, to
# Lapilli's firings. Each alert is one firing, from when it fired until its
# condition was resolved, or it was closed by hand. An activity log, service
# health or resource health alert is stateless, fired once for an event, and
# ends as it fires. Any other still open has no end; a log search alert
# without auto-resolve stays so until someone closes it.
#
# The rule is the alert rule's name, the last part of its id; the id is
# alarm_id, by which lapilli serve ties an alert rule it reads with --alarms
# to its history. Its severity (Sev0 to Sev4, as Lapilli names Azure's
# Prometheus rules), the resource, its resource group and, for metric and log
# search alerts, the dimensions become labels; rename them to what --match
# needs, such as service. Azure writes a
# resource's name and its group's in either case, so both are lower-cased,
# as lapilli serve's /v1/azure writes them for the alert's notifications.
# The name is as serve takes a rule's: the characters that don't show
# taken out, and 512 at most; with nothing left, the id.
# A rule's name as serve takes it: the characters that don't show (a line
# break, a zero-width space) taken out, and cut to 512 characters. One
# serve wouldn't take would stop the whole copy.
def rulename: tostring | gsub("[\\p{Cc}\\p{Cf}\\p{Zl}\\p{Zp}]"; "") | .[:512];
def name: split("/") | .[-1];
def dims: (.dimensions | fromjson? // []) | if type == "array" then map({key: .name, value: (.value | tostring)}) | from_entries else {} end;
.[] | select(.start != "") | . as $r
| {rule: (.rule | name | rulename | if . != "" then . else $r.rule | rulename end),
   labels: (({severity: .severity, resource: (.resource // "" | ascii_downcase), resource_group: (.resourceGroup // "" | ascii_downcase)}
             | with_entries(select(.value != ""))) + dims + (if (.rule // "") != "" then {alarm_id: .rule} else {} end)),
   start: .start}
  + (if .condition == "Resolved" and .resolved != "" then {end: .resolved}
     elif (.service // "" | startswith("ActivityLog")) or (.service // "" | IN("ServiceHealth", "Resource Health")) then {end: .start}
     elif .state == "Closed" and .modified != "" then {end: .modified}
     else {} end)
