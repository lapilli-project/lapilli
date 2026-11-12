#!/bin/sh
# Backtests the alert rules a merge request on GitLab changes, and comments
# the report on it (see lib.sh for the rest). The arguments are lapilli
# verdict's: --prometheus URL, --since 14d, and the like.
#
#   GITLAB_TOKEN_FILE  a file holding a token that may comment on the merge
#                      request (a project access token, scope api, role
#                      Reporter)
#   GITLAB_PROJECT     the project's id, or its path: example/monitoring
#                      (default: GitLab CI's CI_PROJECT_ID)
#   GITLAB_API         default GitLab CI's CI_API_V4_URL, or
#                      https://gitlab.com/api/v4
#   PR                 the merge request's iid (default: GitLab CI's
#                      CI_MERGE_REQUEST_IID)
#   TARGET             default, in a merge request pipeline, the target
#                      branch's commit (CI_MERGE_REQUEST_TARGET_BRANCH_SHA,
#                      in a merged results pipeline) or the merge base
#                      (CI_MERGE_REQUEST_DIFF_BASE_SHA)
recipe=gitlab.sh
here=$(dirname "$0")
: "${GITLAB_TOKEN_FILE:?a file holding a token that may comment on the merge request}"
[ -r "$GITLAB_TOKEN_FILE" ] || { echo "gitlab.sh: GITLAB_TOKEN_FILE $GITLAB_TOKEN_FILE can't be read" >&2; exit 1; }
GITLAB_PROJECT=${GITLAB_PROJECT:-${CI_PROJECT_ID:-}}
: "${GITLAB_PROJECT:?the project id or path, such as example/monitoring}"
api=${GITLAB_API:-${CI_API_V4_URL:-https://gitlab.com/api/v4}}
PR=${PR:-${CI_MERGE_REQUEST_IID:-}}
if [ -z "${TARGET:-}" ] && [ -z "${TARGET_BRANCH:-}" ]; then
  TARGET=${CI_MERGE_REQUEST_TARGET_BRANCH_SHA:-${CI_MERGE_REQUEST_DIFF_BASE_SHA:-}}
fi
case $GITLAB_PROJECT in *[!A-Za-z0-9._/-]* | /* | */) echo "gitlab.sh: GITLAB_PROJECT $GITLAB_PROJECT: want an id or a path" >&2; exit 1 ;; esac
case $api in http://* | https://*) ;; *) echo "gitlab.sh: GITLAB_API $api: want an http or https URL" >&2; exit 1 ;; esac
project=$(printf '%s' "$GITLAB_PROJECT" | jq -sRr @uri)

host_auth() {
  printf 'PRIVATE-TOKEN: %s\n' "$(cat "$GITLAB_TOKEN_FILE")"
}

# host_me writes the account the token is: a project access token's bot.
host_me() {
  code=$(host GET "$api/user")
  case $code in 2??) ;; *) return 1 ;; esac
  jq -r '.id // "" | tostring' "$out"
}

# A merge request's notes, the oldest first, a hundred a page; GitLab's
# own (system notes) passed over.
find_comment() {
  n=1
  while [ "$n" -le 50 ]; do
    code=$(host GET "$api/projects/$project/merge_requests/$PR/notes?sort=asc&order_by=created_at&per_page=100&page=$n")
    case $code in 2??) ;; *) say "GitLab didn't list the merge request's notes ($code): $(shown "$out")"; return 1 ;; esac
    id=$(jq -r --arg me "$1" --arg m "$marker" '[.[] | select(.system != true and (.author.id // "" | tostring) == $me
      and ((.body // "") | split("\n")[0] | rtrimstr("\r")) == $m)][0].id // ""' "$out")
    case $id in *[!0-9]*) say "GitLab gave a note with no number"; return 1 ;; esac
    if [ -n "$id" ]; then
      printf '%s' "$id"
      return 0
    fi
    [ "$(jq 'length' "$out")" -ge 100 ] || return 0
    n=$((n + 1))
  done
}

post_comment() {
  jq -Rs '{body: .}' "$1" >"$tmp/body"
  code=$(host POST "$api/projects/$project/merge_requests/$PR/notes" "$tmp/body")
  case $code in 2??) ;; *) say "GitLab didn't take the note ($code): $(shown "$out")"; return 1 ;; esac
}

edit_comment() {
  jq -Rs '{body: .}' "$2" >"$tmp/body"
  code=$(host PUT "$api/projects/$project/merge_requests/$PR/notes/$1" "$tmp/body")
  case $code in 2??) ;; *) say "GitLab didn't take the note ($code): $(shown "$out")"; return 1 ;; esac
}

# shellcheck source=lib.sh
. "$here/lib.sh"
main "$@"
