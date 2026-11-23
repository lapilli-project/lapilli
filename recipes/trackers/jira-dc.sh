#!/bin/sh
# Keeps Lapilli's tasks in step with issues in a project of Jira Data
# Center (or Server, 8.14 and later), as jira.sh does with Jira Cloud: REST
# API v2, a personal access token, an issue's text and comments in wiki
# markup. Run it every few minutes. An issue is found again by its label
# lapilli-task-ID, among those the token's account made, with no other
# label lapilli-task-*; it is closed by the first transition into the done
# category.
#
#   JIRA_URL         such as https://jira.example.com (or https://HOST/jira)
#   JIRA_TOKEN_FILE  a file holding a personal access token
#   JIRA_PROJECT     the project's key, such as OPS
#   JIRA_ISSUE_TYPE  default Task
#   JIRA_WEB         where its issues' pages are (default JIRA_URL)
#
# The issue made for a task is in jira-dc.jq. And Lapilli's side, as
# recipes/trackers/lib.sh says: LAPILLI_URL, LAPILLI_TOKEN_FILE,
# CLOSE_ISSUES, MAX_ISSUES, MAX_PAGES, SYNC_DAYS, COMMENTS.
set -eu
name=jira-dc.sh
here=$(dirname "$0")
# shellcheck source=recipes/trackers/lib.sh
. "$here/lib.sh"
: "${JIRA_URL:?such as https://jira.example.com}"
: "${JIRA_TOKEN_FILE:?a file holding a personal access token}"
: "${JIRA_PROJECT:?a project key, such as OPS}"
base=${JIRA_URL%/}
web=${JIRA_WEB:-$base}
web=${web%/}
tracker_api JIRA_URL "$base"
project=$JIRA_PROJECT
if ! printf '%s' "$project" | grep -Eq '^[A-Z][A-Z0-9_]{0,49}$'; then
  say "JIRA_PROJECT is a project key, such as OPS"
  exit 1
fi
type=${JIRA_ISSUE_TYPE:-Task}
# One assignment the token, so set -e sees it refused. A personal access
# token is sent as a Bearer token (Jira 8.14 and later).
jira_token=$(secret JIRA_TOKEN_FILE "$JIRA_TOKEN_FILE")

# jira HOW METHOD PATH [BODY]: Jira's answer, or a failure for anything
# but a 2xx, the request asked again or once (HOW, as lib.sh's ask_again
# says). Taken whole before it is read, so a failure isn't read as nothing.
jira() {
  if [ $# -gt 3 ]; then
    printf 'Authorization: Bearer %s\n' "$jira_token" | fetch "$1" -X "$2" -H @- \
      -H 'Accept: application/json' -H 'Content-Type: application/json' --data-binary "$4" "$base/rest/api/2$3"
  else
    printf 'Authorization: Bearer %s\n' "$jira_token" | fetch "$1" -X "$2" -H @- \
      -H 'Accept: application/json' "$base/rest/api/2$3"
  fi
}

# ours: whether the issue on stdin is one this job made for the marker $m:
# made by $me (its creator's key, which a rename doesn't change and which,
# unlike its reporter, can't be changed), with $m its one label
# lapilli-task-ID (a team's label is lapilli-team-NAME, so a team's name
# can't be one).
# shellcheck disable=SC2016 # $me and $m are jq's
ours='def ours: (.fields.creator.key // "") == $me
  and ([.fields.labels // [] | .[] | strings | select(startswith("lapilli-task-"))] == [$m]);'

# t_me: the account's key (JIRAUSER10100, or a name for an account older
# than Jira 8.4), as an issue's creator gives it.
t_me() {
  out=$(jira again GET /myself) || return 1
  printf '%s' "$out" | jq -r '.key // empty'
}

t_find() {
  jql=$(printf 'project = %s AND labels = "%s" AND labels = "lapilli" AND creator = currentUser()' "$project" "$1" | jq -sRr @uri)
  out=$(jira again GET "/search?jql=$jql&maxResults=20&fields=creator,labels") || return 1
  key=$(printf '%s' "$out" | jq -r --arg m "$1" --arg me "$me" "$ours"'
    [.issues[] | select(ours and (.fields.labels | index("lapilli")))][0].key // empty')
  [ -z "$key" ] || printf '%s/browse/%s\n' "$web" "$key"
}

# block: text in a {noformat} block shows as it is written, wiki markup in
# it read as text: a PromQL selector's braces aren't a macro, [~name]
# mentions no one, !URL! shows no image. Only {noformat} ends the block, so
# one in the text is broken by a zero-width space.
# shellcheck disable=SC2016 # jq's
block='def block: "{noformat}\n" + gsub("(?<b>\\{)(?<n>[Nn][Oo][Ff][Oo][Rr][Mm][Aa][Tt])"; "\(.b)\u200b\(.n)") + "\n{noformat}";'

# wiki_text: the task on stdin as the issue's text, in wiki markup: what
# issue_text writes, the task's text and its names in a block, then the
# link back and the marker line, as issue_text ends them: its last two
# lines, or three with the link, told from the end, so no line of the
# task's text is taken for them.
wiki_text() {
  issue_text | jq -Rsr "$block"'rtrimstr("\n") | split("\n")
    | (if length >= 3 and (.[-3] | startswith("In Lapilli: ")) then length - 3 else length - 2 end) as $i
    | (.[:$i] | join("\n") | rtrimstr("\n")) as $text
    | (if $text == "" then "" else ($text | block) + "\n" end) + (.[$i:] | join("\n"))'
}

t_create() {
  text=$(wiki_text <"$1")
  body=$(jq -c --arg text "$text" --arg project "$project" --arg type "$type" -f "$here/jira-dc.jq" "$1")
  out=$(jira once POST "/issue" "$body") || return 1
  key=$(printf '%s' "$out" | jq -r '.key // empty')
  [ -z "$key" ] || printf '%s/browse/%s\n' "$web" "$key"
}

t_ref() {
  printf '%s' "$1" | jq -Rr --arg w "$web/browse/" --arg p "$project" \
    'if startswith($w) then ltrimstr($w) | select(test("\\A" + $p + "-[0-9]+\\z")) else empty end'
}

t_state() {
  if ! out=$(jira again GET "/issue/$1?fields=status,updated,resolution,creator,labels,statuscategorychangedate,resolutiondate"); then
    # Not there (deleted), in a project the account still browses.
    gone || return 1
    jira again GET "/project/$project" >/dev/null || return 1
    echo gone
    return 0
  fi
  printf '%s' "$out" | jq -r --arg m "$2" --arg me "$me" "$epoch$ours"'
    select(ours) | (if .fields.status.statusCategory.key != "done" then "open"
     elif (.fields.resolution.name // "" | test("won.t|duplicate|declined|cannot|rejected"; "i")) then "dropped"
     else "done" end) + " " + (.fields.updated | epoch | tostring)
     + " " + (.fields.statuscategorychangedate // .fields.resolutiondate // .fields.updated | epoch | tostring)'
}

t_close() {
  out=$(jira again GET "/issue/$1/transitions") || return 1
  to=$(printf '%s' "$out" | jq -r '[.transitions[] | select(.to.statusCategory.key == "done")][0].id // empty')
  if [ -z "$to" ]; then
    say "$1 has no transition into done"
    return 1
  fi
  jira once POST "/issue/$1/transitions" "$(jq -cn --arg id "$to" '{transition: {id: $id}}')" >/dev/null
}

# t_comments: the issue's comments, ten pages of 100 at most, those
# everyone who reads the issue may: not one restricted to a role or a
# group (its visibility). A comment is wiki markup, copied as written.
t_comments() {
  start=0
  while [ "$start" -lt 1000 ]; do
    out=$(jira again GET "/issue/$1/comment?startAt=$start&maxResults=100&orderBy=created") || return 1
    printf '%s' "$out" | jq -c --arg me "$me" '.comments[] | select(.visibility == null) | {id: (.id | tostring), author: (.author.displayName // "someone"),
      mine: ((.author.key // "") == $me), at: (.created | sub("(?<z>[+-][0-9]{2})(?<m>[0-9]{2})$"; "\(.z):\(.m)")),
      edited: (.updated // .created | sub("(?<z>[+-][0-9]{2})(?<m>[0-9]{2})$"; "\(.z):\(.m)")), body: (.body // "")}' || return 1
    n=$(printf '%s' "$out" | jq '.comments | length')
    total=$(printf '%s' "$out" | jq '.total // 0')
    number "Jira's total of comments" "$total"
    start=$((start + n))
    { [ "$n" -gt 0 ] && [ "$start" -lt "$total" ]; } || return 0
  done
  : >"$work/capped"
}

# wiki_comment FILE: a comment as comment_text writes it, in wiki markup:
# the comment, quoted, in a block, so nothing in it becomes a link, a
# mention or a macro, and its last line, the one it is found again by,
# after it. Its first line is the block's too when it names who wrote it
# (COMMENT_NAMES), as the sign-in proxy names them: "Written in Lapilli:"
# alone comes before it.
wiki_comment() {
  jq -Rsc "$block"'rtrimstr("\n") | split("\n")
    | (if .[0] == "Written in Lapilli:" then 1 else 0 end) as $from
    | {body: ((.[0:$from] | map(. + "\n\n") | join(""))
        + (.[$from:-1] | join("\n") | ltrimstr("\n") | rtrimstr("\n") | block) + "\n\n" + .[-1])}' "$1"
}

t_comment() {
  out=$(jira once POST "/issue/$1/comment" "$(wiki_comment "$2")") || return 1
  printf '%s' "$out" | jq -r '.id // empty | tostring'
}

t_edit() {
  case $2 in '' | *[!0-9]*) return 1 ;; esac
  jira again PUT "/issue/$1/comment/$2" "$(wiki_comment "$3")" >/dev/null
}

sync_tasks
