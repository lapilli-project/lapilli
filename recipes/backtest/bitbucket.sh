#!/bin/sh
# Backtests the alert rules a pull request on Bitbucket Cloud changes, and
# comments the report on it (see lib.sh for the rest). The arguments are
# lapilli verdict's: --prometheus URL, --since 14d, and the like.
#
#   BITBUCKET_TOKEN_FILE  a file holding a repository access token with the
#                         pull requests write scope
#   BITBUCKET_REPO        such as example/monitoring: WORKSPACE/REPO
#                         (default: Bitbucket Pipelines'
#                         BITBUCKET_REPO_FULL_NAME)
#   BITBUCKET_API         default https://api.bitbucket.org/2.0
#   PR                    the pull request's id (default: Bitbucket
#                         Pipelines' BITBUCKET_PR_ID)
#   TARGET_BRANCH         default, without TARGET, the pull request's
#                         destination (BITBUCKET_PR_DESTINATION_BRANCH),
#                         fetched: Bitbucket Pipelines clones the pull
#                         request's branch alone
recipe=bitbucket.sh
here=$(dirname "$0")
: "${BITBUCKET_TOKEN_FILE:?a file holding a repository access token}"
[ -r "$BITBUCKET_TOKEN_FILE" ] || { echo "bitbucket.sh: BITBUCKET_TOKEN_FILE $BITBUCKET_TOKEN_FILE can't be read" >&2; exit 1; }
BITBUCKET_REPO=${BITBUCKET_REPO:-${BITBUCKET_REPO_FULL_NAME:-}}
: "${BITBUCKET_REPO:?such as example/monitoring}"
api=${BITBUCKET_API:-https://api.bitbucket.org/2.0}
PR=${PR:-${BITBUCKET_PR_ID:-}}
if [ -z "${TARGET:-}" ]; then
  TARGET_BRANCH=${TARGET_BRANCH:-${BITBUCKET_PR_DESTINATION_BRANCH:-}}
fi
case $BITBUCKET_REPO in */*/* | *[!A-Za-z0-9._/-]* | /* | */) echo "bitbucket.sh: BITBUCKET_REPO $BITBUCKET_REPO: want WORKSPACE/REPO" >&2; exit 1 ;; esac
case $BITBUCKET_REPO in */*) ;; *) echo "bitbucket.sh: BITBUCKET_REPO $BITBUCKET_REPO: want WORKSPACE/REPO" >&2; exit 1 ;; esac
case $api in http://* | https://*) ;; *) echo "bitbucket.sh: BITBUCKET_API $api: want an http or https URL" >&2; exit 1 ;; esac

host_auth() {
  printf 'Authorization: Bearer %s\nAccept: application/json\n' "$(cat "$BITBUCKET_TOKEN_FILE")"
}

# host_me writes the account the token is.
host_me() {
  code=$(host GET "$api/user")
  case $code in 2??) ;; *) return 1 ;; esac
  jq -r '.uuid // ""' "$out"
}

# A pull request's comments, a hundred a page, by page number: a "next"
# link the host gives is never followed. Deleted ones passed over.
find_comment() {
  n=1
  while [ "$n" -le 50 ]; do
    code=$(host GET "$api/repositories/$BITBUCKET_REPO/pullrequests/$PR/comments?pagelen=100&page=$n")
    case $code in 2??) ;; *) say "Bitbucket didn't list the pull request's comments ($code): $(shown "$out")"; return 1 ;; esac
    id=$(jq -r --arg me "$1" --arg m "$marker" '[.values[] | select(.deleted != true and (.user.uuid // "") == $me
      and ((.content.raw // "") | split("\n")[0] | rtrimstr("\r")) == $m)][0].id // ""' "$out")
    case $id in *[!0-9]*) say "Bitbucket gave a comment with no number"; return 1 ;; esac
    if [ -n "$id" ]; then
      printf '%s' "$id"
      return 0
    fi
    [ "$(jq '.values | length' "$out")" -ge 100 ] || return 0
    n=$((n + 1))
  done
}

post_comment() {
  jq -Rs '{content: {raw: .}}' "$1" >"$tmp/body"
  code=$(host POST "$api/repositories/$BITBUCKET_REPO/pullrequests/$PR/comments" "$tmp/body")
  case $code in 2??) ;; *) say "Bitbucket didn't take the comment ($code): $(shown "$out")"; return 1 ;; esac
}

edit_comment() {
  jq -Rs '{content: {raw: .}}' "$2" >"$tmp/body"
  code=$(host PUT "$api/repositories/$BITBUCKET_REPO/pullrequests/$PR/comments/$1" "$tmp/body")
  case $code in 2??) ;; *) say "Bitbucket didn't take the comment ($code): $(shown "$out")"; return 1 ;; esac
}

# shellcheck source=lib.sh
. "$here/lib.sh"
main "$@"
