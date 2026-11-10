# FireHydrant's incidents, an array of them as GET /v1/incidents gives them,
# to Lapilli's. Archived incidents are left out. An incident starts when it
# started, or else when it was opened, and ends at its resolved milestone,
# or else its mitigated one: read from its lifecycle phases, or from the
# older milestones list. Its services become the label service.
def at($slug): first(
  (.lifecycle_phases[]?.milestones[]? | select(.slug == $slug) | .occurred_at),
  (.milestones[]? | select(.type == $slug) | .occurred_at)
  | select(. != null and . != ""));
{incidents: [.[]
  | select(.discarded_at == null)
  | [.services[]?.name | select(. != null and . != "")] as $services
  | (at("resolved") // at("mitigated") // null) as $end
  | {
      id: "FH-\(.number // .id)",
      title: (.name // "")[:120],
      start: (.started_at // .created_at)
    }
    + (if $end then {end: $end} else {} end)
    + (if ($services | length) > 0 then {labels: {service: $services}} else {} end)
]}
