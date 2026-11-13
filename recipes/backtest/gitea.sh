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
#   TARGET_BRANCH     default, in Actions without TARGET, the branch the
#                     pull request goes into (GITHUB_BASE_REF), fetched:
#                     Actions checks out the pull request's own commit
recipe=gitea.sh
here=$(dirname "$0")
if [ "${GITEA_ACTIONS:-}" = true ] || [ "${FORGEJO_ACTIONS:-}" = true ]; then
  GITEA_API=${GITEA_API:-${GITHUB_API_URL:-}}
  GITEA_REPO=${GITEA_REPO:-${GITHUB_REPOSITORY:-}}
  if [ -z "${PR:-}" ] && [ -n "${GITHUB_EVENT_PATH:-}" ] && [ -f "$GITHUB_EVENT_PATH" ]; then
    PR=$(jq -r '.pull_request.number // empty' "$GITHUB_EVENT_PATH")
  fi
  # Actions checks out the pull request's own commit, not a merge: the
  # branch it goes into is fetched, and compared from where they parted.
  if [ -z "${TARGET:-}" ]; then
    TARGET_BRANCH=${TARGET_BRANCH:-${GITHUB_BASE_REF:-}}
  fi
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

# shellcheck source=lib.sh
. "$here/lib.sh"
main "$@"
