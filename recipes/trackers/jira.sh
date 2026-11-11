#!/bin/sh
# Keeps Lapilli's tasks in step with issues in a Jira Cloud project, as
# github.sh does with a GitHub repository. Run it every few minutes. An
# issue is found again by its label lapilli-task-ID, among those the
# account made; it is closed by the first transition into the done
# category.
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
    printf 'Authorization: Basic %s\n' "$auth" | curl -fsS --max-time 30 --retry 3 -X "$1" -H @- \
      -H 'Accept: application/json' -H 'Content-Type: application/json' --data-binary "$3" "$base/rest/api/3$2"
  else
    printf 'Authorization: Basic %s\n' "$auth" | curl -fsS --max-time 30 --retry 3 -X "$1" -H @- \
      -H 'Accept: application/json' "$base/rest/api/3$2"
  fi
}

# ours: whether the issue on stdin is one this job made for the marker $m:
# made by $me (its creator, which can't be changed as its reporter can),
# with the label $m.
# shellcheck disable=SC2016 # $me and $m are jq's
ours='def ours: .fields.creator.accountId == $me and (.fields.labels // [] | index($m));'

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
  out=$(jira GET "/issue/$1?fields=status,updated,resolution,creator,labels") || return 1
  printf '%s' "$out" | jq -r --arg m "$2" --arg me "$me" "$epoch$ours"'
    select(ours) | (if .fields.status.statusCategory.key != "done" then "open"
     elif (.fields.resolution.name // "" | test("won.t|duplicate|declined|cannot|rejected"; "i")) then "dropped"
     else "done" end) + " " + (.fields.updated | epoch | tostring)'
}

t_close() {
  out=$(jira GET "/issue/$1/transitions") || return 1
  id=$(printf '%s' "$out" | jq -r '[.transitions[] | select(.to.statusCategory.key == "done")][0].id // empty')
  if [ -z "$id" ]; then
    say "$1 has no transition into done"
    return 1
  fi
  jira POST "/issue/$1/transitions" "$(jq -cn --arg id "$id" '{transition: {id: $id}}')" >/dev/null
}

sync_tasks
