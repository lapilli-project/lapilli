# PagerDuty's incidents, an array of them as GET /incidents gives them, to
# Lapilli's responses (docs/design/responses.md): each high-urgency
# incident created in the period (low-urgency ones page no one) is a page,
# its id PD- and its number, sent when it was created.
#
# --argjson from, to: the period, in seconds; created from on, before to.
# --arg urgency: high keeps the high-urgency incidents, all every one.
# --slurpfile linked: each incident's alerts, as GET /incidents/{id}/alerts
# gives them ({id, alerts} a line), read as ../incidents/pagerduty.jq reads
# them. An incident with no alert Lapilli takes is left out: a response
# needs one at least.
# --slurpfile logs: each incident's log entries, as GET
# /incidents/{id}/log_entries gives them ({id, log_entries} a line).
#
# Who acted is read from a log entry's agent.type (the agent "that created or
# modified" the entry). PagerDuty's OpenAPI document gives AgentReference's
# type as user_reference, service_reference or integration_reference, and an
# integration's own reference carries a concrete type, one of the
# IntegrationReference enum's (events_api_v2_inbound_integration_reference
# for Alertmanager's events), which the log entries of real incidents show:
#   - person: user_reference alone (bot_user_reference is no person).
#   - alert: service_reference, technical_service_reference,
#     integration_reference, outbound_integration_reference and the
#     IntegrationReference enum's eleven types (aws_cloudwatch_,
#     cloudkick_, event_transformer_api_, generic_email_,
#     generic_events_api_, keynote_, nagios_, pingdom_, sql_monitor_ and
#     events_api_v2_inbound_integration_reference, inbound_integration_reference).
#   - any other type (bot_user_reference, business_service_reference, one
#     not in the document) is neither, and is left out, said by a count.
#   - acknowledged: the earliest acknowledge log entry whose agent is a user.
#     One by a service or an integration (an acknowledge event sent through
#     the API) isn't a person's, and is not counted.
#   - resolved: only when the incident's status is resolved, at its last
#     resolve_log_entry. resolved_by is person when its agent is a person,
#     alert when it is an alert's (Alertmanager's resolve event, an
#     auto-resolve). An entry whose agent is neither leaves the resolve out.
# acknowledgement_log_entry is accepted beside acknowledge_log_entry: the
# document gives each, the second in the LogEntry's list of types, the
# first as AcknowledgeLogEntry's own.
#
# With --argjson ids true, the ids of the incidents kept, whose alerts and
# log entries are to be read.
include "alerts";
include "responses";
def keep: select($urgency == "all" or .urgency == "high") | select(.created_at | in_period($from; $to));
def of_labels: {rule: .alertname, labels: del(.alertname)} | as_alert;
# alerts mirrors ../incidents/pagerduty.jq's, which is a script, not a
# library to include.
def alerts:
  (.body.details // .body.cef_details.details) as $d
  | if ($d | type) != "object" then [null]
    else [($d.firing, $d.resolved) | arrays | .[] | .labels | objects | of_labels] as $each
      | if ($each | length) > 0 then $each
        elif ($d.alertname | type) == "string" then
          [{rule: $d.alertname, labels: (if (kept_names | length) > 0 then $d | del(.alertname, .firing, .resolved, .num_firing, .num_resolved) else {} end)} | as_alert]
        else [null] end
    end;
def acks: IN("acknowledge_log_entry", "acknowledgement_log_entry");
# The agent types that are not a person's: PagerDuty's OpenAPI document's
# service and integration references.
def alert_agents: [
  "service_reference", "technical_service_reference",
  "integration_reference", "outbound_integration_reference", "inbound_integration_reference",
  "aws_cloudwatch_inbound_integration_reference", "cloudkick_inbound_integration_reference",
  "event_transformer_api_inbound_integration_reference", "generic_email_inbound_integration_reference",
  "generic_events_api_inbound_integration_reference", "keynote_inbound_integration_reference",
  "nagios_inbound_integration_reference", "pingdom_inbound_integration_reference",
  "sql_monitor_inbound_integration_reference", "events_api_v2_inbound_integration_reference"];
def by_agent: (.agent.type // "") as $t | if $t == "user_reference" then "person" elif ($t | IN(alert_agents[])) then "alert" else null end;
(($ARGS.named.linked // []) | group_by(.id) | map({key: .[0].id, value: [.[].alerts[]? | alerts[]]}) | from_entries) as $by
| (($ARGS.named.logs // []) | group_by(.id) | map({key: .[0].id, value: [.[].log_entries[]?]}) | from_entries) as $logs
| if $ARGS.named.ids then .[] | keep | .id
  else
    [.[] | keep
      | .id as $id
      | ($logs[$id] // []) as $l
      | (($by[$id] // []) | linked) as $k
      | [$l[] | select(.type | acks) | select(.agent.type == "user_reference") | .created_at] as $acks
      | (if .status == "resolved" then [$l[] | select(.type == "resolve_log_entry") | select(.created_at | epoch != null)] | sort_by(.created_at | epoch) | last else null end) as $r
      | {r: response("PD-\(.incident_number)"; $k.alerts; .created_at; ($acks | earliest); ($r.created_at // null); ($r | if . == null then null else by_agent end)),
         noalerts: (($k.alerts | length) == 0),
         unknown: (.status == "resolved" and ($r == null or ($r | by_agent) == null))}] as $all
    | {responses: ($all | map(.r) | written),
       said: {incidents: ($all | length), noalerts: ($all | map(select(.noalerts)) | length), unknown: ($all | map(select(.unknown and (.noalerts | not))) | length)},
       alerts_said: alerts_said($by)}
  end
