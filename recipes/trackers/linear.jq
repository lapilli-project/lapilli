# The issue linear.sh makes for a task, as issueCreate's IssueCreateInput
# takes it: the task (as Lapilli's GET /api/v1/tasks gives it) in, $text the
# issue's text (with the line it is found again by, which must stay), $team
# from LINEAR_TEAM. Linear's labels are given by id (labelIds), made in
# Linear beforehand: add them here, or a priority, as the team wants.
{in: {
  teamId: $team,
  title: .title,
  description: $text
}}
