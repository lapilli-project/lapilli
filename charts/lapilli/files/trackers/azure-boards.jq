# The work item azure-boards.sh makes for a task, as a JSON Patch for POST
# _apis/wit/workitems/$TYPE: the task (as Lapilli's GET /api/v1/tasks gives
# it) in, $text its text, $marker the tag it is found again by (which must
# stay, as must the tag lapilli). A description is HTML: the text is
# escaped, a line each. Tags can't hold ; or , and a team's tag is
# lapilli-team-NAME, so a team's name is never a marker.
def tag: gsub("[;,]"; " ") | .[0:400];
[
  {op: "add", path: "/fields/System.Title", value: (.title | gsub("[\r\n]+"; " ") | .[0:255])},
  {op: "add", path: "/fields/System.Description",
   value: ($text | split("\n") | map(@html) | join("<br>"))},
  {op: "add", path: "/fields/System.Tags",
   value: (["lapilli", (.kind | tag)] + (if .team then ["lapilli-team-" + (.team | tag)] else [] end) + [$marker] | join("; "))}
]
