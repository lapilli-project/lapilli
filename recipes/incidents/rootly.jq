# Rootly's incidents, an array of them as GET /v1/incidents gives them, to
# Lapilli's. Tests, examples and scheduled maintenance are left out, and so
# are incidents still in triage or cancelled. An incident starts when it
# started, or else when it was detected or created, and ends when it was
# resolved, mitigated or closed. Its services become the label service.
# Related resources come as JSON:API documents or flat; both are read.
def name: .data.attributes.name // .attributes.name // .name;
{incidents: [.[]
  | .id as $id
  | .attributes
  | select((.kind // "normal") | IN("test", "test_sub", "example", "example_sub", "scheduled", "scheduled_sub") | not)
  | select((.status // "") | IN("in_triage", "cancelled", "scheduled", "in_progress", "completed") | not)
  | [.services[]? | name | select(. != null and . != "")] as $services
  | (.resolved_at // .mitigated_at // .closed_at) as $end
  | {
      id: "RL-\(.sequential_id // $id)",
      title: (.title // "")[:120],
      start: (.started_at // .detected_at // .created_at)
    }
    + (if $end then {end: $end} else {} end)
    + (if ($services | length) > 0 then {labels: {service: $services}} else {} end)
]}
