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
#   GITHUB_LOGIN       the account the token writes issues as, when GET /user
#                      can't say: a GitHub App's installation token, or
#                      Actions' GITHUB_TOKEN (github-actions[bot]), is
#                      NAME[bot]
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

# gh METHOD PATH [BODY]: GitHub's answer, or a failure for anything but a
# 2xx. Taken whole before it is read, so a failure isn't read as nothing.
gh() {
  if [ $# -gt 2 ]; then
    printf 'Authorization: Bearer %s\n' "$gh_token" | fetch -X "$1" -H @- \
      -H 'Accept: application/vnd.github+json' -H 'X-GitHub-Api-Version: 2022-11-28' \
      -H 'Content-Type: application/json' --data-binary "$3" "$api$2"
  else
    printf 'Authorization: Bearer %s\n' "$gh_token" | fetch -X "$1" -H @- \
      -H 'Accept: application/vnd.github+json' -H 'X-GitHub-Api-Version: 2022-11-28' "$api$2"
  fi
}

# ours: whether the issue on stdin is one this job made for the marker $m:
# written by $me (GitHub's logins are the same in any case), marked for $m.
# shellcheck disable=SC2016 # $me and $m are jq's
ours=$kept'def ours: (.user.login // "" | ascii_downcase) == ($me | ascii_downcase)
  and (.body | kept($m));'

t_me() {
  if [ -n "${GITHUB_LOGIN:-}" ]; then
    printf '%s' "$GITHUB_LOGIN"
    return 0
  fi
  if ! out=$(gh GET /user); then
    say "GET /user didn't say who the token is (a GitHub App's or Actions' token can't): set GITHUB_LOGIN, such as NAME[bot]"
    return 1
  fi
  printf '%s' "$out" | jq -r '.login // empty'
}

t_find() {
  # A GitHub App's issues are searched for as app/NAME.
  author=$me
  case $me in *\[bot\]) author=app/${me%\[bot\]} ;; esac
  q=$(printf 'repo:%s is:issue in:body "%s" author:%s label:lapilli' "$repo" "$1" "$author" | jq -sRr @uri)
  out=$(gh GET "/search/issues?q=$q&per_page=20") || return 1
  printf '%s' "$out" | jq -r --arg m "$1" --arg me "$me" "$ours"'
    [.items[] | select(ours and ([.labels[]? | .name? // .] | index("lapilli")))][0].html_url // empty'
}

t_create() {
  text=$(issue_text <"$1")
  body=$(jq -c --arg text "$text" -f "$here/github.jq" "$1")
  out=$(gh POST "/repos/$repo/issues" "$body") || return 1
  printf '%s' "$out" | jq -r '.html_url // empty'
}

t_ref() {
  # GitHub may spell the repository's name in other cases than GITHUB_REPO.
  printf '%s' "$1" | jq -Rr --arg w "$web/$repo/issues/" \
    'if ascii_downcase | startswith($w | ascii_downcase) then .[($w | length):] | select(test("\\A[0-9]+\\z")) else empty end'
}

t_state() {
  if ! out=$(gh GET "/repos/$repo/issues/$1"); then
    # Not there (deleted: 410), in a repository the token still reads.
    gone || return 1
    gh GET "/repos/$repo" >/dev/null || return 1
    echo gone
    return 0
  fi
  printf '%s' "$out" | jq -r --arg m "$2" --arg me "$me" "$epoch$ours"'
    select(ours) | (if .state == "open" then "open" elif .state_reason == "not_planned" then "dropped" else "done" end)
    + " " + (.updated_at | epoch | tostring)'
}

t_close() {
  reason=completed
  [ "$2" != dropped ] || reason=not_planned
  gh PATCH "/repos/$repo/issues/$1" "$(jq -cn --arg r "$reason" '{state: "closed", state_reason: $r}')" >/dev/null
}

sync_tasks
