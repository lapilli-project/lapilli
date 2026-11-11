#!/bin/sh
# Keeps Lapilli's tasks in step with issues in a Jira Cloud project, as
# github.sh does with a GitHub repository. Run it every few minutes. An
# issue is found again by its label lapilli-task-ID, among those the
# account made, with no other label lapilli-task-*; it is closed by the
# first transition into the done category.
#
#   JIRA_URL         such as https://example.atlassian.net
#   JIRA_EMAIL       the account's email
#   JIRA_TOKEN_FILE  a file holding its API token
#   JIRA_PROJECT     the project's key, such as OPS
#   JIRA_ISSUE_TYPE  default Task
#   JIRA_WEB         where its issues' pages are (default JIRA_URL)
#
# The issue made for a task is in jira.jq. And Lapilli's side, as
# recipes/trackers/lib.sh says: LAPILLI_URL, LAPILLI_TOKEN_FILE,
# CLOSE_ISSUES, MAX_ISSUES, MAX_PAGES, SYNC_DAYS.
set -eu
name=jira.sh
here=$(dirname "$0")
# shellcheck source=recipes/trackers/lib.sh
. "$here/lib.sh"
: "${JIRA_URL:?such as https://example.atlassian.net}"
: "${JIRA_EMAIL:?the email of the account}"
: "${JIRA_TOKEN_FILE:?a file holding its API token}"
: "${JIRA_PROJECT:?a project key, such as OPS}"
base=${JIRA_URL%/}
web=${JIRA_WEB:-$base}
web=${web%/}
project=$JIRA_PROJECT
if ! printf '%s' "$project" | grep -Eq '^[A-Z][A-Z0-9_]{0,49}$'; then
  say "JIRA_PROJECT is a project key, such as OPS"
  exit 1
fi
type=${JIRA_ISSUE_TYPE:-Task}
auth=$(printf '%s:%s' "$JIRA_EMAIL" "$(cat "$JIRA_TOKEN_FILE")" | base64 | tr -d '\n')

# jira METHOD PATH [BODY]: Jira's answer, or a failure for anything but a
# 2xx. Taken whole before it is read, so a failure isn't read as nothing.
jira() {
  if [ $# -gt 2 ]; then
    printf 'Authorization: Basic %s\n' "$auth" | fetch -X "$1" -H @- \
      -H 'Accept: application/json' -H 'Content-Type: application/json' --data-binary "$3" "$base/rest/api/3$2"
  else
    printf 'Authorization: Basic %s\n' "$auth" | fetch -X "$1" -H @- \
      -H 'Accept: application/json' "$base/rest/api/3$2"
  fi
}

# ours: whether the issue on stdin is one this job made for the marker $m:
# made by $me (its creator, which can't be changed as its reporter can),
# with $m its one label lapilli-task-ID (a team's label is lapilli-team-NAME,
# so a team's name can't be one).
# shellcheck disable=SC2016 # $me and $m are jq's
ours='def ours: .fields.creator.accountId == $me
  and ([.fields.labels // [] | .[] | strings | select(startswith("lapilli-task-"))] == [$m]);'

t_me() {
  out=$(jira GET /myself) || return 1
  printf '%s' "$out" | jq -r '.accountId // empty'
}

t_find() {
  jql=$(printf 'project = %s AND labels = "%s" AND labels = "lapilli" AND creator = currentUser()' "$project" "$1" | jq -sRr @uri)
  out=$(jira GET "/search/jql?jql=$jql&maxResults=20&fields=creator,labels") || return 1
  key=$(printf '%s' "$out" | jq -r --arg m "$1" --arg me "$me" "$ours"'
    [.issues[] | select(ours and (.fields.labels | index("lapilli")))][0].key // empty')
  [ -z "$key" ] || printf '%s/browse/%s\n' "$web" "$key"
}

t_create() {
  text=$(issue_text <"$1")
  body=$(jq -c --arg text "$text" --arg project "$project" --arg type "$type" -f "$here/jira.jq" "$1")
  out=$(jira POST "/issue" "$body") || return 1
  key=$(printf '%s' "$out" | jq -r '.key // empty')
  [ -z "$key" ] || printf '%s/browse/%s\n' "$web" "$key"
}

t_ref() {
  printf '%s' "$1" | jq -Rr --arg w "$web/browse/" --arg p "$project" \
    'if startswith($w) then ltrimstr($w) | select(test("\\A" + $p + "-[0-9]+\\z")) else empty end'
}

t_state() {
  if ! out=$(jira GET "/issue/$1?fields=status,updated,resolution,creator,labels"); then
    # Not there (deleted), in a project the account still browses.
    gone || return 1
    jira GET "/project/$project" >/dev/null || return 1
    echo gone
    return 0
  fi
  printf '%s' "$out" | jq -r --arg m "$2" --arg me "$me" "$epoch$ours"'
    select(ours) | (if .fields.status.statusCategory.key != "done" then "open"
     elif (.fields.resolution.name // "" | test("won.t|duplicate|declined|cannot|rejected"; "i")) then "dropped"
     else "done" end) + " " + (.fields.updated | epoch | tostring)'
}

t_close() {
  out=$(jira GET "/issue/$1/transitions") || return 1
  to=$(printf '%s' "$out" | jq -r '[.transitions[] | select(.to.statusCategory.key == "done")][0].id // empty')
  if [ -z "$to" ]; then
    say "$1 has no transition into done"
    return 1
  fi
  jira POST "/issue/$1/transitions" "$(jq -cn --arg id "$to" '{transition: {id: $id}}')" >/dev/null
}

# adf: Atlassian Document Format to text, a paragraph a line: what a
# comment says, read as text, never as markup.
adf='def adf: [.. | objects | select(.type == "paragraph") | [.content[]? | if .type == "hardBreak" then "\n" else (.text // "") end] | join("")] | join("\n");'

# t_comments: the issue's comments, ten pages of 100 at most.
t_comments() {
  start=0
  while [ "$start" -lt 1000 ]; do
    out=$(jira GET "/issue/$1/comment?startAt=$start&maxResults=100&orderBy=created") || return 1
    printf '%s' "$out" | jq -c --arg me "$me" "$adf"'.comments[] | {id: (.id | tostring), author: (.author.displayName // "someone"),
      mine: (.author.accountId == $me), at: (.created | sub("(?<z>[+-][0-9]{2})(?<m>[0-9]{2})$"; "\(.z):\(.m)")), body: (.body | adf)}'
    n=$(printf '%s' "$out" | jq '.comments | length')
    total=$(printf '%s' "$out" | jq '.total // 0')
    number "Jira's total of comments" "$total"
    start=$((start + n))
    { [ "$n" -gt 0 ] && [ "$start" -lt "$total" ]; } || return 0
  done
}

# t_comment: the text as Atlassian Document Format, a paragraph a line, as
# text nodes only: nothing in it becomes a link, a mention or a macro.
t_comment() {
  jira POST "/issue/$1/comment" "$(jq -Rsc '{body: {type: "doc", version: 1, content: [split("\n")[] |
    if . == "" then {type: "paragraph", content: []} else {type: "paragraph", content: [{type: "text", text: .}]} end]}}' "$2")" >/dev/null
}

sync_tasks
