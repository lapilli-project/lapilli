# Grafana IRM's OnCall alert groups, an array of them as GET
# /api/v1/alert_groups/ gives them (its pages' results), to Lapilli's
# responses: each group created in the period is a page, IRM- and its id,
# sent when it was created.
#
# --argjson from, to: the period, in seconds; created from on, before to.
#
# Its alerts: the group's last_alert.payload is what the integration sent;
# for Alertmanager's it is Alertmanager's webhook, whose alerts each carry
# their labels (alertname among them): alertname is the rule, the others
# the labels (../incidents/alerts.jq). A group whose last alert's payload
# has none is left out: a response needs an alert at least.
#
# Who acted, from the fields the reference gives:
#   - acknowledged: acknowledged_at, when acknowledged_by is set. The
#     engine's serializer (engine/apps/public_api/serializers/
#     alert_groups.py) writes acknowledged_by as the id of the user who
#     acknowledged, null otherwise, so an acknowledgement by the alert
#     source (acknowledged_at set, acknowledged_by null) is not a person's
#     and is left out.
#   - resolved: only when resolved_by is set, which is a person's
#     (resolved_by is the id of a user, as acknowledged_by is). The
#     serializer writes it as null for a group the alert source resolved
#     and also for one resolved by the last step of its escalation chain,
#     by stopping maintenance, or wiped (the model's resolved_by
#     SOURCE, LAST_STEP, DISABLE_MAINTENANCE and WIPED, which the API does
#     not expose), so null doesn't say the source did it. Such a group
#     gets no resolve here, and no group is written as resolved by the
#     alert: a count says how many were left so.
# No user id is written.
include "alerts";
include "responses";
def alerts: [(.last_alert.payload // null) | objects | .alerts | arrays | .[] | objects | .labels | objects | {rule: .alertname, labels: del(.alertname)} | as_alert];
def user: type == "string" and . != "";
[.[] | select(.created_at | in_period($from; $to))
  | (alerts | linked) as $k
  | (if (.acknowledged_by | user) then .acknowledged_at | time else null end) as $ack
  | (if (.resolved_by | user) then .resolved_at | time else null end) as $res
  | {r: response("IRM-\(.id)"; $k.alerts; .created_at; $ack; $res; (if $res == null then null else "person" end)),
     noalerts: (($k.alerts | length) == 0),
     unsettled: (((.resolved_at | time) != null) and (.resolved_by | user | not))}] as $all
| {responses: ($all | map(.r) | written),
   said: {groups: ($all | length), noalerts: ($all | map(select(.noalerts)) | length),
          unsettled: ($all | map(select(.unsettled and (.noalerts | not))) | length)}}
