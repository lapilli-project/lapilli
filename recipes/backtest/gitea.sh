#!/bin/sh
# Backtests the alert rules a pull request on Gitea or Forgejo changes, and
# comments the report on it (see lib.sh for the rest). The arguments are
# lapilli verdict's: --prometheus URL, --since 14d, and the like.
#
#   GITEA_API         such as https://git.example.com/api/v1 (default, in
#                     Gitea or Forgejo Actions: GITHUB_API_URL)
#   GITEA_REPO        such as example/monitoring (default, in Actions:
#                     GITHUB_REPOSITORY)
#   GITEA_TOKEN_FILE  a file holding a token that may comment on the pull
#                     request, and say whose it is (scopes write:issue,
#                     read:repository and read:user)
#   PR                the pull request's number (default, in Actions: the
#                     event's)
#   TARGET_BRANCH     default, in Actions without TARGET, or with lib.sh's
#                     default HEAD^1 written out, the branch the
#                     pull request goes into (GITHUB_BASE_REF), as
#                     fetch-depth: 0 fetched it: Actions checks out the
#                     pull request's own commit. Fetched only when the
#                     checkout didn't, which needs its credentials kept
#
# With STATUS=true (see lib.sh), a commit status (POST
# /repos/OWNER/REPO/statuses/SHA) on the pull request's head commit, the
# event's in Actions (pull_request.head.sha): the token needs
# write:repository, its account write access to the repository. In
# Actions, of a pull request into a branch other than the repository's
# default (the event's repository.default_branch), it is lapilli/backtest
# (into BRANCH); in an event of no pull request (push), the branch is
# TARGET_BRANCH, and with none, no status is set.
#
# Through lapilli serve (LAPILLI_SERVE, see lib.sh), LAPILLI_SERVE_AUDIENCE
# asks Forgejo Actions for the job's ID token for that audience, which the
# workflow's enable-openid-connect: true allows, as GitHub Actions is asked
# (ACTIONS_ID_TOKEN_REQUEST_URL). Gitea Actions gives no ID token: a plain
# token, LAPILLI_SERVE_TOKEN_FILE.
recipe=gitea.sh
here=$(dirname "$0")
# Forgejo's ID token is asked of Actions (lib.sh's actions_token). Its
# runner, act's, reads ::add-mask:: as GitHub's does, but the token isn't
# masked here (ci_masks isn't github): nothing the recipe runs prints it,
# as it goes to curl on stdin. Forgejo's FORGEJO_ACTIONS is GITHUB_ACTIONS
# too, as each FORGEJO_ variable is a GITHUB_ one, so ci_masks, not
# GITHUB_ACTIONS, says which recipe masks it.
ci_tokens=actions
if [ "${GITEA_ACTIONS:-}" = true ] || [ "${FORGEJO_ACTIONS:-}" = true ]; then
  GITEA_API=${GITEA_API:-${GITHUB_API_URL:-}}
  GITEA_REPO=${GITEA_REPO:-${GITHUB_REPOSITORY:-}}
  if [ -z "${PR:-}" ] && [ -n "${GITHUB_EVENT_PATH:-}" ] && [ -f "$GITHUB_EVENT_PATH" ]; then
    PR=$(jq -r '.pull_request.number // empty' "$GITHUB_EVENT_PATH")
  fi
  # For STATUS: the pull request's head commit, as the event says; and,
  # for the status's name, the repository's default branch and the one the
  # pull request goes into (lib.sh).
  ci_into=${GITHUB_BASE_REF:-}
  if [ -n "${GITHUB_EVENT_PATH:-}" ] && [ -f "$GITHUB_EVENT_PATH" ]; then
    status_commit=$(jq -r '.pull_request.head.sha // empty | strings' "$GITHUB_EVENT_PATH" 2>/dev/null) || status_commit=
    ci_default=$(jq -r '.repository.default_branch // empty | strings' "$GITHUB_EVENT_PATH" 2>/dev/null) || ci_default=
    [ -n "$ci_into" ] || ci_into=$(jq -r '.pull_request.base.ref // empty | strings' "$GITHUB_EVENT_PATH" 2>/dev/null) || ci_into=
  fi
  # Actions checks out the pull request's own commit, not a merge: the
  # branch it goes into is fetched, and compared from where they parted.
  # A TARGET the job gives is its own, as the CI says the base (lib.sh);
  # HEAD^1, lib.sh's default, written out is none given, as lib.sh takes
  # it: of the pull request's own commit, a merge its author made, it is
  # whatever first parent they chose.
  ci_base=1
  case ${TARGET:-HEAD^1} in
    HEAD^1) TARGET_BRANCH=${TARGET_BRANCH:-${GITHUB_BASE_REF:-}} ;;
  esac
  # ... and makes no merge, in any event: HEAD a merge is one its author
  # made, and its first parent isn't taken for the base, but the branch
  # the pull request goes into (lib.sh's ci_merges and ci_branch).
  ci_merges=1
  ci_branch=${GITHUB_BASE_REF:-}
fi
: "${GITEA_API:?such as https://git.example.com/api/v1}"
: "${GITEA_REPO:?such as example/monitoring}"
: "${GITEA_TOKEN_FILE:?a file holding a token that may comment on the pull request}"
[ -r "$GITEA_TOKEN_FILE" ] || { echo "gitea.sh: GITEA_TOKEN_FILE $GITEA_TOKEN_FILE can't be read" >&2; exit 1; }
case $GITEA_REPO in */*/* | *[!A-Za-z0-9._/-]* | /* | */) echo "gitea.sh: GITEA_REPO $GITEA_REPO: want OWNER/REPO" >&2; exit 1 ;; esac
case $GITEA_REPO in */*) ;; *) echo "gitea.sh: GITEA_REPO $GITEA_REPO: want OWNER/REPO" >&2; exit 1 ;; esac
host_api=$GITEA_API

host_auth() {
  printf 'Authorization: token %s\nAccept: application/json\n' "$(cat "$GITEA_TOKEN_FILE")"
}

# host_me writes the account the token is.
host_me() {
  code=$(host GET "$GITEA_API/user")
  case $code in 2??) ;; *) return 1 ;; esac
  jq -r '.login // ""' "$out"
}

# A pull request's comments are its issue's, all of them in one answer:
# Gitea takes no page for them (since and before only).
find_comment() {
  code=$(host GET "$GITEA_API/repos/$GITEA_REPO/issues/$PR/comments")
  case $code in 2??) ;; *) say "Gitea didn't list the pull request's comments ($code): $(shown "$out")"; return 1 ;; esac
  id=$(jq -r --arg me "$1" --arg m "$marker" '[.[] | select((.user.login // "" | ascii_downcase) == ($me | ascii_downcase)
    and ((.body // "") | split("\n")[0] | rtrimstr("\r")) == $m)][0].id // ""' "$out")
  case $id in *[!0-9]*) say "Gitea gave a comment with no number"; return 1 ;; esac
  printf '%s' "$id"
}

post_comment() {
  jq -Rs '{body: .}' "$1" >"$tmp/body"
  code=$(host POST "$GITEA_API/repos/$GITEA_REPO/issues/$PR/comments" "$tmp/body")
  case $code in 2??) ;; *) say "Gitea didn't take the comment ($code): $(shown "$out")"; return 1 ;; esac
}

edit_comment() {
  jq -Rs '{body: .}' "$2" >"$tmp/body"
  code=$(host PATCH "$GITEA_API/repos/$GITEA_REPO/issues/comments/$1" "$tmp/body")
  case $code in 2??) ;; *) say "Gitea didn't take the comment ($code): $(shown "$out")"; return 1 ;; esac
}

# A commit status: pending, success or failure, as lib.sh says, Gitea's
# words for them too.
post_status() {
  jq -n --arg state "$1" --arg text "$2" --arg context "$status_context" --arg url "$status_url" \
    '{state: $state, description: $text, context: $context} + (if $url != "" then {target_url: $url} else {} end)' >"$tmp/body"
  code=$(host POST "$GITEA_API/repos/$GITEA_REPO/statuses/$status_sha" "$tmp/body")
  case $code in 2??) ;; *) say "Gitea didn't take the status ($code): $(shown "$out")"; return 1 ;; esac
}

# shellcheck source=lib.sh
. "$here/lib.sh"
main "$@"
