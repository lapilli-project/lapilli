#!/bin/sh
# Keeps Lapilli's tasks in step with issues in a GitHub repository: an
# issue for each task without one, its link put back on the task, a task
# closed or reopened as its issue was, and with CLOSE_ISSUES an issue closed
# as its task was. Run it every few minutes.
#
#   GITHUB_REPO        such as example/alerts
#   GITHUB_TOKEN_FILE  a file holding a token that may read and write the
#                      repository's issues
#   GITHUB_API         default https://api.github.com (GitHub Enterprise
#                      Server: https://HOST/api/v3)
#   GITHUB_WEB         default https://github.com (Enterprise: https://HOST)
#
# The issue made for a task is in github.jq. And Lapilli's side, as
# recipes/trackers/lib.sh says: LAPILLI_URL, LAPILLI_TOKEN_FILE,
# CLOSE_ISSUES, MAX_ISSUES, MAX_PAGES, SYNC_DAYS.
set -eu
name=github.sh
here=$(dirname "$0")
# shellcheck source=recipes/trackers/lib.sh
. "$here/lib.sh"
: "${GITHUB_REPO:?such as example/alerts}"
: "${GITHUB_TOKEN_FILE:?a file holding a GitHub token}"
api=${GITHUB_API:-https://api.github.com}
web=${GITHUB_WEB:-https://github.com}
api=${api%/}
web=${web%/}
repo=$GITHUB_REPO
if ! printf '%s' "$repo" | grep -Eq '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$'; then
  say "GITHUB_REPO is OWNER/NAME"
  exit 1
fi
gh_token=$(cat "$GITHUB_TOKEN_FILE")

# gh METHOD PATH [BODY]: GitHub's answer, or a failure for anything but a 2xx.
gh() {
  if [ $# -gt 2 ]; then
    printf 'Authorization: Bearer %s\n' "$gh_token" | curl -fsS --max-time 30 --retry 3 -X "$1" -H @- \
      -H 'Accept: application/vnd.github+json' -H 'X-GitHub-Api-Version: 2022-11-28' \
      -H 'Content-Type: application/json' --data-binary "$3" "$api$2"
  else
    printf 'Authorization: Bearer %s\n' "$gh_token" | curl -fsS --max-time 30 --retry 3 -X "$1" -H @- \
      -H 'Accept: application/vnd.github+json' -H 'X-GitHub-Api-Version: 2022-11-28' "$api$2"
  fi
}

t_find() {
  q=$(printf 'repo:%s is:issue in:body "%s"' "$repo" "$1" | jq -sRr @uri)
  gh GET "/search/issues?q=$q&per_page=20" | jq -r --arg m "$1" \
    '[.items[] | select(.body // "" | test("\\b" + $m + "\\b"))][0].html_url // empty'
}

t_create() {
  text=$(issue_text <"$1")
  body=$(jq -c --arg text "$text" -f "$here/github.jq" "$1")
  gh POST "/repos/$repo/issues" "$body" | jq -r '.html_url // empty'
}

t_ref() {
  # GitHub may spell the repository's name in other cases than GITHUB_REPO.
  printf '%s' "$1" | jq -Rr --arg w "$web/$repo/issues/" \
    'if ascii_downcase | startswith($w | ascii_downcase) then .[($w | length):] | select(test("\\A[0-9]+\\z")) else empty end'
}

t_state() {
  gh GET "/repos/$repo/issues/$1" | jq -r "$epoch"'
    (if .state == "open" then "open" elif .state_reason == "not_planned" then "dropped" else "done" end)
    + " " + (.updated_at | epoch | tostring)'
}

t_close() {
  reason=completed
  [ "$2" != dropped ] || reason=not_planned
  gh PATCH "/repos/$repo/issues/$1" "$(jq -cn --arg r "$reason" '{state: "closed", state_reason: $r}')" >/dev/null
}

sync_tasks
