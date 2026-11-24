# GitHub's issues, as `gh issue list --json number,title,createdAt,closedAt,labels`
# gives them, to Lapilli's. A label like service:checkout becomes the label
# service. When an issue was opened or closed is when the incident started or
# ended, as near as the issue can tell.
{incidents: [.[]
  | [.labels[]?.name | select(startswith("service:")) | ltrimstr("service:")] as $services
  | {
      id: "GH-\(.number)",
      title: (.title // "")[:120],
      start: .createdAt
    }
    + (if .closedAt then {end: .closedAt} else {} end)
    + (if ($services | length) > 0 then {labels: {service: $services}} else {} end)
]}
