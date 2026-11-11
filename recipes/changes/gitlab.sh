#!/bin/sh
# Opens a merge request on GitLab for each change Lapilli was approved to
# make, and tells Lapilli what became of it (see lib.sh for the rest).
#
#   GITLAB_PROJECT     the project's id, or its path: example/monitoring
#   GITLAB_TOKEN_FILE  a file holding a token that may open merge requests
#                      in it (a project access token, scope api, role
#                      Developer)
#   GITLAB_API         default https://gitlab.com/api/v4
recipe=gitlab.sh
here=$(dirname "$0")
: "${GITLAB_PROJECT:?the project id or path, such as example/monitoring}"
: "${GITLAB_TOKEN_FILE:?a file holding a token that may open merge requests}"
api=${GITLAB_API:-https://gitlab.com/api/v4}
case $GITLAB_PROJECT in *[!A-Za-z0-9._/-]* | /* | */) echo "gitlab.sh: GITLAB_PROJECT $GITLAB_PROJECT: want an id or a path" >&2; exit 1 ;; esac
project=$(printf '%s' "$GITLAB_PROJECT" | jq -sRr @uri)

host_auth() {
  printf 'PRIVATE-TOKEN: %s\n' "$(cat "$GITLAB_TOKEN_FILE")"
}

find_pr() {
  b=$(printf '%s' "$1" | jq -sRr @uri)
  code=$(host GET "$api/projects/$project/merge_requests?state=opened&source_branch=$b")
  case $code in 2??) ;; *) say "GitLab didn't list merge requests ($code): $(head -c 300 "$out")"; return 1 ;; esac
  jq -r '.[0].web_url // ""' "$out"
}

create_pr() {
  jq --arg head "$1" --arg base "$BASE" '{title, description: .body, source_branch: $head, target_branch: $base, remove_source_branch: true}' "$2" >"$tmp/body"
  code=$(host POST "$api/projects/$project/merge_requests" "$tmp/body")
  case $code in 2??) ;; *) say "GitLab didn't open a merge request ($code): $(head -c 300 "$out")"; return 1 ;; esac
  jq -r '.web_url // ""' "$out"
}

pr_state() {
  code=$(host GET "$api/projects/$project/merge_requests/$1")
  case $code in 2??) ;; *) say "GitLab didn't give merge request $1 ($code)"; return 1 ;; esac
  jq -r 'if .state == "merged" then "merged" elif .state == "closed" then "closed" else "open" end' "$out"
}

# shellcheck source=lib.sh
. "$here/lib.sh"
main
