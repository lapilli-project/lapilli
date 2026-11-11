# The issue gitlab.sh makes for a task, as POST /projects/{id}/issues takes
# it: the task (as Lapilli's GET /api/v1/tasks gives it) in, $text the
# issue's text (with the line it is found again by, which must stay). Change
# the title or the labels here; labels are one string, comma-separated.
{
  title: .title,
  description: $text,
  labels: (["lapilli", .kind] + (if .team then [.team | gsub(","; " ")] else [] end) | join(","))
}
