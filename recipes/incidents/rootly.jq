# Rootly's incidents, an array of them as GET /v1/incidents gives them, to
# Lapilli's. Tests, examples and scheduled maintenance are left out, and so
# are incidents still in triage or cancelled. An incident starts when it
# started, or else when it was detected or created, and ends when it was
# resolved, mitigated or closed. Its services become the label service.
# Related resources come as JSON:API documents or flat; both are read. An
# incident on two pages, as numbered pages with no order can give, is one.
#
# With --slurpfile linked, each incident's alerts too ({id, alerts} a line),
# as GET /v1/incidents/{id}/alerts gives them: an alert from Alertmanager
# carries alertname, instance, dc and job among its labels, which are its
# rule and labels; status and version are the webhook's, source_name
# Rootly's, so not labels. One with no alertname is left out. With
# --argjson ids true, the ids of the incidents kept, whose alerts are to be
# read.
include "alerts";
def name: .data.attributes.name // .attributes.name // .name;
def keep:
  select((.attributes.kind // "normal") | IN("test", "test_sub", "example", "example_sub", "scheduled", "scheduled_sub") | not)
  | select((.attributes.status // "") | IN("in_triage", "cancelled", "scheduled", "in_progress", "completed") | not);
def alert:
  ([.attributes.labels[]? | select((.key | type) == "string") | {key, value}] | from_entries) as $l
  | {rule: $l.alertname, labels: ($l | del(.alertname, .status, .version, .source_name))}
  | as_alert;
(($ARGS.named.linked // []) | group_by(.id) | map({key: .[0].id, value: [.[].alerts[]? | alert]}) | from_entries) as $by
| if $ARGS.named.ids then unique_by(.id)[] | keep | .id
  else {incidents: [unique_by(.id)[]
    | keep
    | .id as $id
    | .attributes
    | [.services[]? | name | select(. != null and . != "")] as $services
    | (.resolved_at // .mitigated_at // .closed_at) as $end
    | {
        id: "RL-\(.sequential_id // $id)",
        title: (.title // "")[:120],
        start: (.started_at // .detected_at // .created_at)
      }
      + (if $end then {end: $end} else {} end)
      + (if ($services | length) > 0 then {labels: {service: $services}} else {} end)
    | with_alerts($by; $id)
  ], alerts_said: alerts_said($by)}
  end
