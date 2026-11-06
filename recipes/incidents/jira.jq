# Jira's issues, an array of them as GET /rest/api/3/search/jql gives them with
# fields=summary,created,resolutiondate,components, to Lapilli's. Jira writes
# zones as +0900; RFC 3339 wants +09:00. Components become the label service.
def iso: sub("(?<h>[+-][0-9]{2})(?<m>[0-9]{2})$"; "\(.h):\(.m)");
{incidents: [.[]
  | [.fields.components[]?.name] as $services
  | {
      id: .key,
      title: (.fields.summary // "")[:120],
      start: (.fields.created | iso)
    }
    + (if .fields.resolutiondate then {end: (.fields.resolutiondate | iso)} else {} end)
    + (if ($services | length) > 0 then {labels: {service: $services}} else {} end)
]}
