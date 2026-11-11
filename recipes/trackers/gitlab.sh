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

# gl METHOD PATH [BODY]: GitLab's answer, or a failure for anything but a 2xx.
gl() {
  if [ $# -gt 2 ]; then
    printf 'PRIVATE-TOKEN: %s\n' "$gl_token" | curl -fsS --max-time 30 --retry 3 -X "$1" -H @- \
      -H 'Content-Type: application/json' --data-binary "$3" "$base/api/v4/projects/$enc$2"
  else
    printf 'PRIVATE-TOKEN: %s\n' "$gl_token" | curl -fsS --max-time 30 --retry 3 -X "$1" -H @- "$base/api/v4/projects/$enc$2"
  fi
}

t_find() {
  gl GET "/issues?search=$1&in=description&scope=all&per_page=20" | jq -r --arg m "$1" \
    '[.[] | select(.description // "" | test("\\b" + $m + "\\b"))][0].web_url // empty'
}

t_create() {
  text=$(issue_text <"$1")
  body=$(jq -c --arg text "$text" -f "$here/gitlab.jq" "$1")
  gl POST "/issues" "$body" | jq -r '.web_url // empty'
}

t_ref() {
  printf '%s' "$1" | jq -Rr --arg w "$web/$project/-/issues/" \
    'if startswith($w) then ltrimstr($w) | select(test("\\A[0-9]+\\z")) else empty end'
}

t_state() {
  gl GET "/issues/$1" | jq -r "$epoch"'
    (if .state == "closed" then "done" else "open" end) + " " + (.updated_at | epoch | tostring)'
}

t_close() {
  gl PUT "/issues/$1" '{"state_event":"close"}' >/dev/null
}

sync_tasks
