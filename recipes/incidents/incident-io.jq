# incident.io's incidents, an array of them as GET /v2/incidents gives them,
# to Lapilli's. Those that weren't real (tests, tutorials) or turned out not
# to be incidents are left out. Lifecycle timestamps are named per
# organisation: change the names here if yours differ. The custom field that
# says which services were hit is too.
#
# With --slurpfile linked, each incident's alerts too ({id, alerts} a line),
# each as GET /v2/alerts/{id} gives it, of those GET /v2/incident_alerts
# lists for the incident: incident.io keeps an alert's labels only as the
# attributes its alert source takes from the payload. An alert with an
# attribute named alertname is of that rule, and its other attributes of a
# literal value, named as labels are, are its labels; one with none is left
# out. With --argjson ids true, the ids of the incidents kept, whose alerts
# are to be read.
include "alerts";
def keep:
  select(.mode == "standard" or .mode == "retrospective")
  | select(.incident_status.category | IN("triage", "declined", "merged", "canceled") | not);
def at($name): [.incident_timestamp_values[]? | select(.incident_timestamp.name == $name) | .value.value] | first;
def services: [.custom_field_entries[]? | select(.custom_field.name == "Affected services") | .values[]?
  | (.value_catalog_entry.name // .value_option.value // .value_text // empty)];
def alert:
  ([.attributes[]? | select(.attribute.array != true and (.value.literal | type) == "string")
    | {key: .attribute.name, value: .value.literal}] | from_entries) as $a
  | {rule: $a.alertname, labels: ($a | del(.alertname))}
  | as_alert;
(($ARGS.named.linked // []) | group_by(.id) | map({key: .[0].id, value: [.[].alerts[]? | alert]}) | from_entries) as $by
| if $ARGS.named.ids then .[] | keep | .id
  else {incidents: [.[]
    | keep
    | .id as $id
    | (at("Resolved at") // at("Fixed at")) as $end
    | services as $services
    | {
        id: .reference,
        title: (.name // "")[:120],
        start: (at("Impact started") // .created_at)
      }
      + (if $end then {end: $end} else {} end)
      + (if ($services | length) > 0 then {labels: {service: $services}} else {} end)
    | with_alerts($by; $id)
  ], alerts_said: alerts_said($by)}
  end
