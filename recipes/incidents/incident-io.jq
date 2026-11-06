# incident.io's incidents, an array of them as GET /v2/incidents gives them,
# to Lapilli's. Those that weren't real (tests, tutorials) or turned out not
# to be incidents are left out. Lifecycle timestamps are named per
# organisation: change the names here if yours differ. The custom field that
# says which services were hit is too.
def at($name): [.incident_timestamp_values[]? | select(.incident_timestamp.name == $name) | .value.value] | first;
def services: [.custom_field_entries[]? | select(.custom_field.name == "Affected services") | .values[]?
  | (.value_catalog_entry.name // .value_option.value // .value_text // empty)];
{incidents: [.[]
  | select(.mode == "standard" or .mode == "retrospective")
  | select(.incident_status.category | IN("triage", "declined", "merged", "canceled") | not)
  | (at("Resolved at") // at("Fixed at")) as $end
  | services as $services
  | {
      id: .reference,
      title: (.name // "")[:120],
      start: (at("Impact started") // .created_at)
    }
    + (if $end then {end: $end} else {} end)
    + (if ($services | length) > 0 then {labels: {service: $services}} else {} end)
]}
