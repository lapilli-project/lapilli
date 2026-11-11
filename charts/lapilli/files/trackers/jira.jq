# The issue jira.sh makes for a task, as POST /rest/api/3/issue takes it:
# the task (as Lapilli's GET /api/v1/tasks gives it) in, $text the issue's
# text, $project and $type from JIRA_PROJECT and JIRA_ISSUE_TYPE. The labels
# lapilli and lapilli-task-ID are how its issue is found again, and must
# stay; labels can't hold spaces, and a summary is one line. Change the
# summary, labels or other fields here.
{fields: {
  project: {key: $project},
  issuetype: {name: $type},
  summary: (.title | gsub("\\s*[\\r\\n]+\\s*"; " ")),
  labels: (["lapilli", "lapilli-task-\(.id)", .kind]
    + (if .team then [.team | gsub("[^A-Za-z0-9_.-]"; "-")] else [] end)),
  description: {type: "doc", version: 1, content: ($text | split("\n") | map(
    if . == "" then {type: "paragraph"} else {type: "paragraph", content: [{type: "text", text: .}]} end))}
}}
