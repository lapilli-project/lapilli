# Azure Monitor's alerts, as azure-monitor.sh's query projects them, to
# Lapilli's firings. Each alert is one firing, from when it fired until its
# condition was resolved, or it was closed by hand. An activity log, service
# health or resource health alert is stateless, fired once for an event, and
# ends as it fires. Any other still open has no end; a log search alert
# without auto-resolve stays so until someone closes it.
#
# The rule is the alert rule's name, the last part of its id. Its severity
# (Sev0 to Sev4, as Lapilli names Azure's Prometheus rules), the resource, its
# resource group and, for metric and log search alerts, the dimensions become
# labels; rename them to what --match needs, such as service.
def name: split("/") | .[-1];
def dims: (.dimensions | fromjson? // []) | if type == "array" then map({key: .name, value: (.value | tostring)}) | from_entries else {} end;
.[] | select(.start != "")
| {rule: (.rule | name),
   labels: (({severity: .severity, resource: .resource, resource_group: .resourceGroup} | with_entries(select(.value != ""))) + dims),
   start: .start}
  + (if .condition == "Resolved" and .resolved != "" then {end: .resolved}
     elif .state == "Closed" and .modified != "" then {end: .modified}
     elif (.service // "" | startswith("ActivityLog")) or (.service // "" | IN("ServiceHealth", "Resource Health")) then {end: .start}
     else {} end)
