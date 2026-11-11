#!/bin/sh
# Keeps Lapilli's tasks in step with issues in a GitLab project, as
# github.sh does with a GitHub repository. Run it every few minutes.
#
#   GITLAB_PROJECT     the project's path, such as example/alerts
#   GITLAB_TOKEN_FILE  a file holding a token with the api scope
#   GITLAB_URL         default https://gitlab.com
#   GITLAB_WEB         where its issues' pages are (default GITLAB_URL)
#
# The issue made for a task is in gitlab.jq. And Lapilli's side, as
# recipes/trackers/lib.sh says: LAPILLI_URL, LAPILLI_TOKEN_FILE,
# CLOSE_ISSUES, MAX_ISSUES, MAX_PAGES, SYNC_DAYS.
set -eu
name=gitlab.sh
here=$(dirname "$0")
# shellcheck source=recipes/trackers/lib.sh
. "$here/lib.sh"
: "${GITLAB_PROJECT:?a project path, such as example/alerts}"
: "${GITLAB_TOKEN_FILE:?a file holding a GitLab token}"
base=${GITLAB_URL:-https://gitlab.com}
base=${base%/}
web=${GITLAB_WEB:-$base}
web=${web%/}
project=$GITLAB_PROJECT
if ! printf '%s' "$project" | grep -Eq '^[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)+$'; then
  say "GITLAB_PROJECT is a project path, such as example/alerts"
  exit 1
fi
enc=$(printf '%s' "$project" | jq -sRr @uri)
gl_token=$(cat "$GITLAB_TOKEN_FILE")

# gl METHOD PATH [BODY]: GitLab's answer, or a failure for anything but a
# 2xx. Taken whole before it is read, so a failure isn't read as nothing.
gl() {
  if [ $# -gt 2 ]; then
    printf 'PRIVATE-TOKEN: %s\n' "$gl_token" | fetch -X "$1" -H @- \
      -H 'Content-Type: application/json' --data-binary "$3" "$base/api/v4$2"
  else
    printf 'PRIVATE-TOKEN: %s\n' "$gl_token" | fetch -X "$1" -H @- "$base/api/v4$2"
  fi
}

# ours: whether the issue on stdin is one this job made for the marker $m:
# written by $me (its user id), marked for $m.
# shellcheck disable=SC2016 # $me and $m are jq's
ours=$kept'def ours: (.author.id // "" | tostring) == $me
  and (.description | kept($m));'

t_me() {
  out=$(gl GET /user) || return 1
  printf '%s' "$out" | jq -r '.id | numbers'
}

t_find() {
  out=$(gl GET "/projects/$enc/issues?search=$1&in=description&author_id=$me&labels=lapilli&scope=all&per_page=20") || return 1
  printf '%s' "$out" | jq -r --arg m "$1" --arg me "$me" "$ours"'[.[] | select(ours and ([.labels[]? | .name? // .] | index("lapilli")))][0].web_url // empty'
}

t_create() {
  text=$(issue_text <"$1")
  body=$(jq -c --arg text "$text" -f "$here/gitlab.jq" "$1")
  out=$(gl POST "/projects/$enc/issues" "$body") || return 1
  printf '%s' "$out" | jq -r '.web_url // empty'
}

# An issue's link: -/work_items/N since GitLab 18 made issues work items,
# -/issues/N before.
t_ref() {
  printf '%s' "$1" | jq -Rr --arg w "$web/$project/-/" \
    'if startswith($w) then ltrimstr($w) | select(test("\\A(issues|work_items)/[0-9]+\\z")) | sub("\\A[a-z_]+/"; "") else empty end'
}

t_state() {
  if ! out=$(gl GET "/projects/$enc/issues/$1"); then
    # Not there (deleted), in a project the token still reads.
    gone || return 1
    gl GET "/projects/$enc" >/dev/null || return 1
    echo gone
    return 0
  fi
  printf '%s' "$out" | jq -r --arg m "$2" --arg me "$me" "$epoch$ours"'
    select(ours) | (if .state == "closed" then "done" else "open" end) + " " + (.updated_at | epoch | tostring)
    + " " + (.closed_at // .updated_at | epoch | tostring)'
}

t_close() {
  gl PUT "/projects/$enc/issues/$1" '{"state_event":"close"}' >/dev/null
}

# t_comments: the issue's notes, ten pages of 100 at most, people's: not
# GitLab's system notes (a label changed, the issue closed), nor internal
# notes, which only the project's members may read.
t_comments() {
  page=1
  while [ "$page" -le 10 ]; do
    out=$(gl GET "/projects/$enc/issues/$1/notes?sort=asc&order_by=created_at&per_page=100&page=$page") || return 1
    printf '%s' "$out" | jq -c --arg me "$me" '.[] | select((.system | not) and (.internal // false | not) and (.confidential // false | not))
      | {id: (.id | tostring), author: (.author.username // "someone"),
      mine: ((.author.id // "" | tostring) == $me), at: .created_at, body: (.body // "")}'
    [ "$(printf '%s' "$out" | jq 'length')" -eq 100 ] || return 0
    page=$((page + 1))
  done
  : >"$work/capped"
}

# t_comment: a note, each line starting with / escaped as \/, as the
# issue's text is: GitLab carries out quick actions in a note the API is
# given, as the job's account.
t_comment() {
  out=$(gl POST "/projects/$enc/issues/$1/notes" "$(jq -Rsc '{body: (split("\n") | map(sub("^(?<s>[ \\t]*)/"; "\(.s)\\/")) | join("\n"))}' "$2")") || return 1
  printf '%s' "$out" | jq -r '.id // empty | tostring'
}

sync_tasks
