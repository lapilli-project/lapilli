# Compass's alerts, an array of them as GET /v1/alerts gives them, to
# Lapilli's incidents. Compass keeps alerts, not incidents, so which alerts
# are incidents is yours to say: here those of priority P1 or P2 (Opsgenie's
# critical and high). An alert raised by the rules Lapilli judges would only
# catch itself: leave those out here, by source or tag, or with
# COMPASS_QUERY. A closed alert ends when its log says it was closed
# (compass-closed.jq), or, where its log wasn't read, at its last update,
# which is when it was closed unless something changed it after.
# Alertmanager sends an alert's labels as extra properties: its service
# becomes the label service.
#
# $closes[0] is null on a first pass, which gives the ids of the closed
# alerts that count, one a line, for their logs to be read; then an object
# of each one's close, as seconds since 1970.
def epoch:
  capture("\\A(?<d>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(?:\\.[0-9]+)?(?<z>Z|[+-][0-9]{2}:[0-9]{2})\\z")
  | (.d + "Z" | fromdateiso8601) - (if .z == "Z" then 0 else ((.z[1:3] | tonumber) * 3600 + (.z[4:6] | tonumber) * 60) * (if .z[0:1] == "-" then -1 else 1 end) end);
($from | tonumber) as $from
| $closes[0] as $closes
| [.[]
  | select(.priority | IN("P1", "P2"))
  | select((.createdAt | epoch) >= $from)] as $alerts
| if $closes == null then $alerts[] | select(.status == "closed") | .id
  else {incidents: [$alerts[]
  | (.createdAt | epoch) as $created
  | {
      id: "CP-\(.tinyId // .id)",
      title: (.message // "")[:120],
      start: .createdAt
    }
    + (if .status != "closed" then {}
       elif ($closes[.id] // -1) >= $created then {end: ($closes[.id] | floor | todate)}
       elif .updatedAt then {end: .updatedAt}
       else {} end)
    + (if .extraProperties.service then {labels: {service: [.extraProperties.service | tostring]}} else {} end)
  ]} end
