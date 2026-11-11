# The issue gitlab.sh makes for a task, as POST /projects/{id}/issues takes
# it: the task (as Lapilli's GET /api/v1/tasks gives it) in, $text the
# issue's text (with the line it is found again by, which must stay, as
# must the label lapilli). Change the title or the labels here; labels are
# one string, comma-separated.
#
# A line of the text starting with / is escaped as \/, which Markdown shows
# as /: GitLab would otherwise take it as a quick action (/move, /assign,
# /confidential) and carry it out as the job's account.
{
  title: .title,
  description: ($text | split("\n") | map(sub("^(?<s>[ \\t]*)/"; "\(.s)\\/")) | join("\n")),
  labels: (["lapilli", .kind] + (if .team then [.team | gsub(","; " ")] else [] end) | join(","))
}
