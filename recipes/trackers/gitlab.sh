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
    printf 'PRIVATE-TOKEN: %s\n' "$gl_token" | curl -fsS --max-time 30 --retry 3 -X "$1" -H @- \
      -H 'Content-Type: application/json' --data-binary "$3" "$base/api/v4$2"
  else
    printf 'PRIVATE-TOKEN: %s\n' "$gl_token" | curl -fsS --max-time 30 --retry 3 -X "$1" -H @- "$base/api/v4$2"
  fi
}

# ours: whether the issue on stdin is one this job made for the marker $m:
# written by $me (its user id), holding $m.
# shellcheck disable=SC2016 # $me and $m are jq's
ours='def ours: (.author.id // "" | tostring) == $me
  and (.description // "" | test("\\b" + $m + "\\b"));'

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

t_ref() {
  printf '%s' "$1" | jq -Rr --arg w "$web/$project/-/issues/" \
    'if startswith($w) then ltrimstr($w) | select(test("\\A[0-9]+\\z")) else empty end'
}

t_state() {
  out=$(gl GET "/projects/$enc/issues/$1") || return 1
  printf '%s' "$out" | jq -r --arg m "$2" --arg me "$me" "$epoch$ours"'
    select(ours) | (if .state == "closed" then "done" else "open" end) + " " + (.updated_at | epoch | tostring)'
}

t_close() {
  gl PUT "/projects/$enc/issues/$1" '{"state_event":"close"}' >/dev/null
}

sync_tasks
