#!/bin/sh
# Backtests the alert rules a pull request on GitHub changes, and comments
# the report on it (see lib.sh for the rest). The arguments are lapilli
# verdict's: --prometheus URL, --since 14d, and the like. action.yml runs it
# in GitHub Actions.
#
#   GITHUB_TOKEN_FILE  a file holding a token that may comment on the pull
#                      request (GitHub Actions' own, with pull-requests:
#                      write; a fine-grained token: pull requests, write)
#   GITHUB_REPO        such as example/monitoring (default: GitHub
#                      Actions' GITHUB_REPOSITORY)
#   GITHUB_API         default GitHub Actions' GITHUB_API_URL, or
#                      https://api.github.com; a GitHub Enterprise
#                      server's https://HOST/api/v3
#   GITHUB_LOGIN       the account the token comments as, when GET /user
#                      can't say: NAME[bot] for a GitHub App's token. In
#                      GitHub Actions, a token that can't say is taken as
#                      the job's own, github-actions[bot]
#   PR                 the pull request's number (default: the event's)
#
#   GITHUB_TOKEN_FILE=/run/secrets/github GITHUB_REPO=example/monitoring PR=42 \
#   LAPILLI_VERSION=v0.32.0 PATHS=alerts ./github.sh --prometheus http://prometheus:9090 --since 14d
recipe=github.sh
here=$(dirname "$0")
: "${GITHUB_TOKEN_FILE:?a file holding a token that may comment on the pull request}"
[ -r "$GITHUB_TOKEN_FILE" ] || { echo "github.sh: GITHUB_TOKEN_FILE $GITHUB_TOKEN_FILE can't be read" >&2; exit 1; }
GITHUB_REPO=${GITHUB_REPO:-${GITHUB_REPOSITORY:-}}
: "${GITHUB_REPO:?such as example/monitoring}"
api=${GITHUB_API:-${GITHUB_API_URL:-https://api.github.com}}
case $GITHUB_REPO in */*/* | *[!A-Za-z0-9._/-]* | /* | */) echo "github.sh: GITHUB_REPO $GITHUB_REPO: want OWNER/REPO" >&2; exit 1 ;; esac
case $GITHUB_REPO in */*) ;; *) echo "github.sh: GITHUB_REPO $GITHUB_REPO: want OWNER/REPO" >&2; exit 1 ;; esac
host_api=$api
if [ -z "${PR:-}" ] && [ -n "${GITHUB_EVENT_PATH:-}" ] && [ -f "$GITHUB_EVENT_PATH" ]; then
  PR=$(jq -r '.pull_request.number // empty' "$GITHUB_EVENT_PATH")
fi

host_auth() {
  printf 'Authorization: Bearer %s\nAccept: application/vnd.github+json\nX-GitHub-Api-Version: 2022-11-28\n' "$(cat "$GITHUB_TOKEN_FILE")"
}

# host_me writes the account the token is: GITHUB_LOGIN, or as GET /user
# says. A job's own token in GitHub Actions can't ask, and comments as
# github-actions[bot].
host_me() {
  if [ -n "${GITHUB_LOGIN:-}" ]; then
    printf '%s' "$GITHUB_LOGIN"
    return
  fi
  code=$(host GET "$api/user")
  case $code in
    2??) jq -r '.login // ""' "$out" ;;
    403)
      [ "${GITHUB_ACTIONS:-}" = true ] || return 1
      say "GitHub didn't say who the token is (GET /user): taken as GitHub Actions' own, github-actions[bot]; set GITHUB_LOGIN for a GitHub App's"
      printf 'github-actions[bot]'
      ;;
    *) return 1 ;;
  esac
}

# A pull request's comments are its issue's, a hundred a page.
find_comment() {
  n=1
  while [ "$n" -le 50 ]; do
    code=$(host GET "$api/repos/$GITHUB_REPO/issues/$PR/comments?per_page=100&page=$n")
    case $code in 2??) ;; *) say "GitHub didn't list the pull request's comments ($code): $(shown "$out")"; return 1 ;; esac
    id=$(jq -r --arg me "$1" --arg m "$marker" '[.[] | select((.user.login // "" | ascii_downcase) == ($me | ascii_downcase)
      and ((.body // "") | split("\n")[0] | rtrimstr("\r")) == $m)][0].id // ""' "$out")
    case $id in *[!0-9]*) say "GitHub gave a comment with no number"; return 1 ;; esac
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
  code=$(host POST "$api/repos/$GITHUB_REPO/issues/$PR/comments" "$tmp/body")
  case $code in 2??) ;; *) say "GitHub didn't take the comment ($code): $(shown "$out")"; return 1 ;; esac
}

edit_comment() {
  jq -Rs '{body: .}' "$2" >"$tmp/body"
  code=$(host PATCH "$api/repos/$GITHUB_REPO/issues/comments/$1" "$tmp/body")
  case $code in 2??) ;; *) say "GitHub didn't take the comment ($code): $(shown "$out")"; return 1 ;; esac
}

# shellcheck source=lib.sh
. "$here/lib.sh"
main "$@"
