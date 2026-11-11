# The issue github.sh makes for a task, as POST /repos/{owner}/{repo}/issues
# takes it: the task (as Lapilli's GET /api/v1/tasks gives it) in, $text the
# issue's text (with the line it is found again by, which must stay, as
# must the label lapilli). Change the title or the labels here; GitHub
# makes a label it hasn't seen.
{
  title: .title,
  body: $text,
  labels: (["lapilli", .kind] + (if .team then [.team] else [] end))
}
