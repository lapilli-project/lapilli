# The issue youtrack.sh makes for a task, as POST /api/issues takes it: the
# task (as Lapilli's GET /api/v1/tasks gives it) in, $text the issue's text
# (with the line it is found again by, which must stay), $project the
# project's database id, which youtrack.sh asks YouTrack for. A summary is
# one line. Tags are given by id, made and shared in YouTrack beforehand
# (tags: [{id: "6-12"}]); custom fields by name, as YouTrack names them
# (customFields: [{name: "Priority", "$type": "SingleEnumIssueCustomField",
# value: {name: "Major"}}]): add them here.
{
  project: {id: $project},
  summary: (.title | gsub("\\s*[\\r\\n]+\\s*"; " ")),
  description: $text
}
