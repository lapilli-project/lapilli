# The issue jira-dc.sh makes for a task, as POST /rest/api/2/issue takes it
# in Jira Data Center: the task (as Lapilli's GET /api/v1/tasks gives it)
# in, $text the issue's text in wiki markup, $project and $type from
# JIRA_PROJECT and JIRA_ISSUE_TYPE. The labels lapilli and lapilli-task-ID
# are how its issue is found again, and must stay, and no other label may
# start lapilli-task- (a team's is lapilli-team-NAME); labels can't hold
# spaces, and a summary is one line. Change the summary, labels or other
# fields here.
{fields: {
  project: {key: $project},
  issuetype: {name: $type},
  summary: (.title | gsub("\\s*[\\r\\n]+\\s*"; " ")),
  labels: (["lapilli", "lapilli-task-\(.id)", .kind]
    + (if .team then ["lapilli-team-" + (.team | gsub("[^A-Za-z0-9_.-]"; "-"))] else [] end)),
  description: $text
}}
