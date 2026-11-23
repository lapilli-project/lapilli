#!/bin/sh
# Backtests the alert rules a pull request on Bitbucket Cloud changes, and
# comments the report on it (see lib.sh for the rest). The arguments are
# lapilli verdict's: --prometheus URL, --since 14d, and the like.
#
#   BITBUCKET_TOKEN_FILE  a file holding a repository access token with the
#                         pull requests write scope
#   BITBUCKET_USER_UUID   the account the token comments as, {...}: a
#                         repository access token can't ask (GET /user needs
#                         the account scope, which it can't have). The first
#                         comment it posts says it in the job's log; without
#                         it, each run posts a comment of its own
#   BITBUCKET_REPO        such as example/monitoring: WORKSPACE/REPO
#                         (default: Bitbucket Pipelines'
#                         BITBUCKET_REPO_FULL_NAME)
#   BITBUCKET_API         default https://api.bitbucket.org/2.0
#   PR                    the pull request's id (default: Bitbucket
#                         Pipelines' BITBUCKET_PR_ID)
#   TARGET_BRANCH         default, without TARGET (or with HEAD^1, lib.sh's
#                         default, written out), the pull request's
#                         destination (BITBUCKET_PR_DESTINATION_BRANCH),
#                         fetched: Bitbucket Pipelines clones the pull
#                         request's branch alone, and lets the step fetch
#
# With STATUS=true (see lib.sh), a build status (POST
# /repositories/WORKSPACE/REPO/commit/SHA/statuses/build) on the commit the
# pipeline ran for (BITBUCKET_COMMIT), its key lapilli-backtest, its
# refname the pull request's source branch, as GET .../pullrequests/ID
# says, which associates it with the pull request, as Bitbucket's
# reference says; linking to the pipeline's result. The token's pull
# requests scope covers it (Bitbucket's reference asks the repository
# scope, which pull requests implies).
#
# Through lapilli serve (LAPILLI_SERVE, see lib.sh), the step's ID token,
# from a step that asks for serve's audience alone, given as
# LAPILLI_ID_TOKEN:
#
#   - step:
#       oidc:
#         audiences:
#           - https://lapilli-ci.example.com
#       script:
#         - LAPILLI_ID_TOKEN=$BITBUCKET_STEP_OIDC_TOKEN sh ci/backtest/bitbucket.sh --since 14d
#
# The token names the workspace's own audience beside serve's, as
# Bitbucket writes every token, so it is one other services that trust the
# workspace take too: the step asks for no other audience, those services
# trust an audience of their own rather than the workspace's, and the
# recipe doesn't take BITBUCKET_STEP_OIDC_TOKEN of itself; one whose aud
# names the workspace's audience alone isn't sent (lib.sh). Or a plain
# token, LAPILLI_SERVE_TOKEN_FILE.
recipe=bitbucket.sh
here=$(dirname "$0")
: "${BITBUCKET_TOKEN_FILE:?a file holding a repository access token}"
[ -r "$BITBUCKET_TOKEN_FILE" ] || { echo "bitbucket.sh: BITBUCKET_TOKEN_FILE $BITBUCKET_TOKEN_FILE can't be read" >&2; exit 1; }
BITBUCKET_REPO=${BITBUCKET_REPO:-${BITBUCKET_REPO_FULL_NAME:-}}
: "${BITBUCKET_REPO:?such as example/monitoring}"
api=${BITBUCKET_API:-https://api.bitbucket.org/2.0}
PR=${PR:-${BITBUCKET_PR_ID:-}}
# HEAD^1, lib.sh's default, written out is none given, as lib.sh takes it.
case ${TARGET:-HEAD^1} in
  HEAD^1) TARGET_BRANCH=${TARGET_BRANCH:-${BITBUCKET_PR_DESTINATION_BRANCH:-}} ;;
esac
# A pull request's pipeline says what it goes into: a TARGET the job gives
# is its own (lib.sh's ci_base).
[ -z "${BITBUCKET_PR_DESTINATION_BRANCH:-}" ] || ci_base=1
case $BITBUCKET_REPO in */*/* | *[!A-Za-z0-9._/-]* | /* | */) echo "bitbucket.sh: BITBUCKET_REPO $BITBUCKET_REPO: want WORKSPACE/REPO" >&2; exit 1 ;; esac
case $BITBUCKET_REPO in */*) ;; *) echo "bitbucket.sh: BITBUCKET_REPO $BITBUCKET_REPO: want WORKSPACE/REPO" >&2; exit 1 ;; esac
case ${BITBUCKET_USER_UUID:-} in
  '') ;;
  \{*[!0-9a-fA-F-]*\} | \{\}) echo "bitbucket.sh: BITBUCKET_USER_UUID $BITBUCKET_USER_UUID: want {UUID}" >&2; exit 1 ;;
  \{*\}) ;;
  *) echo "bitbucket.sh: BITBUCKET_USER_UUID $BITBUCKET_USER_UUID: want {UUID}" >&2; exit 1 ;;
esac
host_api=$api
# For STATUS: the commit the pipeline ran for, and its result's page.
status_commit=${BITBUCKET_COMMIT:-}
status_link=
case ${BITBUCKET_BUILD_NUMBER:-}/${BITBUCKET_REPO_FULL_NAME:-} in
  [0-9]*/?*/?*) status_link=https://bitbucket.org/$BITBUCKET_REPO_FULL_NAME/pipelines/results/$BITBUCKET_BUILD_NUMBER ;;
esac

host_auth() {
  printf 'Authorization: Bearer %s\nAccept: application/json\n' "$(cat "$BITBUCKET_TOKEN_FILE")"
}

# host_me writes the account the token is: BITBUCKET_USER_UUID, or as GET
# /user says, for a token that may ask.
host_me() {
  if [ -n "${BITBUCKET_USER_UUID:-}" ]; then
    printf '%s' "$BITBUCKET_USER_UUID"
    return
  fi
  code=$(host GET "$api/user")
  case $code in 2??) ;; *) return 1 ;; esac
  jq -r '.uuid // ""' "$out"
}

# for_bitbucket FILE: the comment, with Bitbucket's Markdown, which shows
# HTML as text: a <details> Lapilli writes, alone on its lines, its summary
# in bold.
for_bitbucket() {
  sed -e 's|^<details><summary>\(.*\)</summary>$|**\1**|' -e '/^<\/details>$/d' "$1" | jq -Rs '{content: {raw: .}}' >"$tmp/body"
}

# A pull request's comments, a hundred a page, by page number, while
# Bitbucket says there is a next: the link it gives is never followed.
# Deleted ones passed over.
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
    [ -n "$(jq -r '.next // empty' "$out")" ] || return 0
    n=$((n + 1))
  done
}

post_comment() {
  for_bitbucket "$1"
  code=$(host POST "$api/repositories/$BITBUCKET_REPO/pullrequests/$PR/comments" "$tmp/body")
  case $code in 2??) ;; *) say "Bitbucket didn't take the comment ($code): $(shown "$out")"; return 1 ;; esac
  if [ -z "${BITBUCKET_USER_UUID:-}" ]; then
    uuid=$(jq -r '.user.uuid // ""' "$out")
    case $uuid in
      \{*\}) say "posted as $uuid: set BITBUCKET_USER_UUID to it, and later runs edit this comment rather than post another" ;;
    esac
  fi
}

edit_comment() {
  for_bitbucket "$2"
  code=$(host PUT "$api/repositories/$BITBUCKET_REPO/pullrequests/$PR/comments/$1" "$tmp/body")
  case $code in 2??) ;; *) say "Bitbucket didn't take the comment ($code): $(shown "$out")"; return 1 ;; esac
}

# A build status: INPROGRESS, SUCCESSFUL or FAILED, as lib.sh says pending,
# success or failure, keyed by the status's name (lapilli-backtest,
# lapilli-backtest-KEY), on the pull request's source branch: a key's
# status is written over by the next, as Bitbucket's reference says.
post_status() {
  case $1 in
    success) st=SUCCESSFUL ;;
    pending) st=INPROGRESS ;;
    *) st=FAILED ;;
  esac
  code=$(host GET "$api/repositories/$BITBUCKET_REPO/pullrequests/$PR")
  case $code in 2??) ;; *) say "Bitbucket didn't give the pull request ($code): $(shown "$out")"; return 1 ;; esac
  ref=$(jq -r '.source.branch.name // empty | strings' "$out" 2>/dev/null) || ref=
  if [ -z "$ref" ] || [ "${#ref}" -gt 255 ]; then
    say "Bitbucket didn't say the pull request's source branch"
    return 1
  fi
  jq -n --arg state "$st" --arg key "$(printf '%s' "$status_context" | tr / -)" --arg name "$status_context" --arg text "$2" \
    --arg ref "$ref" --arg url "$status_url" \
    '{key: $key, state: $state, name: $name, description: $text, refname: $ref} + (if $url != "" then {url: $url} else {} end)' >"$tmp/body"
  code=$(host POST "$api/repositories/$BITBUCKET_REPO/commit/$status_sha/statuses/build" "$tmp/body")
  case $code in 2??) ;; *) say "Bitbucket didn't take the status ($code): $(shown "$out")"; return 1 ;; esac
}

# shellcheck source=lib.sh
. "$here/lib.sh"
main "$@"
