# PagerDuty's incidents, an array of them as GET /incidents gives them, to
# Lapilli's. Low-urgency incidents page no one, so they are left out.
def title: (.title // "")[:120];
{incidents: [.[] | select(.urgency == "high") | {
    id: "PD-\(.incident_number)",
    title: title,
    start: .created_at
  }
  + (if .resolved_at then {end: .resolved_at} else {} end)
  + (if .service.summary then {labels: {service: [.service.summary]}} else {} end)
]}
