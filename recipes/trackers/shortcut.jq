# The story shortcut.sh makes for a task, as POST /api/v3/stories takes it:
# the task (as Lapilli's GET /api/v1/tasks gives it) in, $text its text
# (with the line it is found again by, which must stay, as must the label
# lapilli), $state the workflow state it is made in. Change the type or
# the labels here.
{
  name: (.title | gsub("[\r\n]+"; " ") | .[0:512]),
  description: $text,
  story_type: "chore",
  workflow_state_id: ($state | tonumber),
  labels: ([{name: "lapilli"}, {name: .kind}] + (if .team then [{name: .team}] else [] end))
}
