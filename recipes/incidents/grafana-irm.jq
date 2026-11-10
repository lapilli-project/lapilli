# Grafana IRM's incidents, an array of them as
# IncidentsService.QueryIncidents gives them, to Lapilli's. Drills are left
# out. An incident starts at its incident start, when one was set, and ends
# at its incident end, or when it was closed. A label whose key is service
# becomes the label service: change the key here if yours differs.
# Times without a zone are read as UTC, and Go's zero time as no time.
def time: if . == null or . == "" or startswith("0001-") then empty
  elif test("\\A[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2}(\\.[0-9]+)?\\z") then sub(" "; "T") + "Z"
  else . end;
{incidents: [.[]
  | select(.isDrill != true)
  | [.labels[]? | select(.key == "service") | .label | select(. != null and . != "")] as $services
  | (first(.incidentEnd | time) // (if .status == "resolved" then first(.closedTime | time) else null end) // null) as $end
  | {
      id: "IRM-\(.incidentID)",
      title: (.title // "")[:120],
      start: (first(.incidentStart | time) // .createdTime)
    }
    + (if $end then {end: $end} else {} end)
    + (if ($services | length) > 0 then {labels: {service: $services}} else {} end)
]}
