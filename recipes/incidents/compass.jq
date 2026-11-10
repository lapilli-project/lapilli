# Compass's alerts, an array of them as GET /v1/alerts gives them, to
# Lapilli's incidents. Compass keeps alerts, not incidents, so which alerts
# are incidents is yours to say: here those of priority P1 or P2 (Opsgenie's
# critical and high). An alert raised by the rules Lapilli judges would only
# catch itself: leave those out here, by source or tag, or with
# COMPASS_QUERY. A closed alert ends at its last update, which is when it
# was closed unless something changed it after. Alertmanager sends an
# alert's labels as extra properties: its service becomes the label service.
def epoch:
  capture("\\A(?<d>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(?:\\.[0-9]+)?(?<z>Z|[+-][0-9]{2}:[0-9]{2})\\z")
  | (.d + "Z" | fromdateiso8601) - (if .z == "Z" then 0 else ((.z[1:3] | tonumber) * 3600 + (.z[4:6] | tonumber) * 60) * (if .z[0:1] == "-" then -1 else 1 end) end);
($from | tonumber) as $from
| {incidents: [.[]
  | select(.priority | IN("P1", "P2"))
  | select((.createdAt | epoch) >= $from)
  | {
      id: "CP-\(.tinyId // .id)",
      title: (.message // "")[:120],
      start: .createdAt
    }
    + (if .status == "closed" and .updatedAt then {end: .updatedAt} else {} end)
    + (if .extraProperties.service then {labels: {service: [.extraProperties.service | tostring]}} else {} end)
]}
