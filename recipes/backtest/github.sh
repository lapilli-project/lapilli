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
# With STATUS=true (see lib.sh), a commit status (POST
# /repos/OWNER/REPO/statuses/SHA) on the event's head commit
# (pull_request.head.sha), linking to the run: the token needs statuses:
# write (a fine-grained token: commit statuses, write). Of a pull request
# into a branch other than the repository's default, as the event says
# (repository.default_branch), it is lapilli/backtest (into BRANCH).
#
# Through lapilli serve (LAPILLI_SERVE, see lib.sh), LAPILLI_SERVE_AUDIENCE
# asks GitHub Actions for the job's ID token for that audience, as
# core.getIDToken does, which the job's permissions: id-token: write
# allows: ACTIONS_ID_TOKEN_REQUEST_URL, with ACTIONS_ID_TOKEN_REQUEST_TOKEN
# given to curl on stdin. In GitHub Actions (GITHUB_ACTIONS=true), the
# token is masked in the job's log (::add-mask::) as soon as it is had, as
# core.getIDToken masks it.
#
#   GITHUB_TOKEN_FILE=/run/secrets/github GITHUB_REPO=example/monitoring PR=42 \
#   LAPILLI_VERSION=v0.33.0 PATHS=alerts ./github.sh --prometheus http://prometheus:9090 --since 14d
recipe=github.sh
here=$(dirname "$0")
# The job's ID token is asked of Actions (lib.sh's actions_token), and
# masked in GitHub Actions' log (::add-mask::).
ci_tokens=actions
ci_masks=github
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
# For STATUS: the pull request's head commit, as the event says, and the
# run's page.
status_commit=
if [ -n "${GITHUB_EVENT_PATH:-}" ] && [ -f "$GITHUB_EVENT_PATH" ]; then
  status_commit=$(jq -r '.pull_request.head.sha // empty | strings' "$GITHUB_EVENT_PATH" 2>/dev/null) || status_commit=
fi
# The branch the pull request goes into, in a pull request's run: TARGET
# where HEAD isn't the merge GitHub makes, as a checkout of the pull
# request's own commit (ref: pull_request.head.sha) has (lib.sh).
ci_branch=${GITHUB_BASE_REF:-}
# ... and its head, as the event says: HEAD^2 of the merge GitHub makes of
# it, or HEAD itself where the job checks the head out.
ci_head=$status_commit
# ... and that merge, which a pull_request event's run is of: GITHUB_SHA,
# where GITHUB_REF is refs/pull/N/merge. A merge with the head for its
# second parent isn't GitHub's by that alone: one the pull request's
# author pushed since has whatever first parent its author chose.
ci_merge=
case ${GITHUB_REF:-} in
  refs/pull/*/merge) ci_merge=${GITHUB_SHA:-} ;;
esac
# ... and, for the status's name, the repository's default branch and the
# one the pull request goes into, as the event says: a pull request into
# another branch sets lapilli/backtest (into BRANCH), not the default's
# (lib.sh).
ci_default='' ci_into=${GITHUB_BASE_REF:-}
if [ -n "${GITHUB_EVENT_PATH:-}" ] && [ -f "$GITHUB_EVENT_PATH" ]; then
  ci_default=$(jq -r '.repository.default_branch // empty | strings' "$GITHUB_EVENT_PATH" 2>/dev/null) || ci_default=
  [ -n "$ci_into" ] || ci_into=$(jq -r '.pull_request.base.ref // empty | strings' "$GITHUB_EVENT_PATH" 2>/dev/null) || ci_into=
fi
status_link=
if [ -n "${GITHUB_SERVER_URL:-}" ] && [ -n "${GITHUB_REPOSITORY:-}" ] && [ -n "${GITHUB_RUN_ID:-}" ]; then
  status_link=$GITHUB_SERVER_URL/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID
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

# A commit status: pending, success or failure, as lib.sh says, GitHub's
# words for them too.
post_status() {
  jq -n --arg state "$1" --arg text "$2" --arg context "$status_context" --arg url "$status_url" \
    '{state: $state, description: $text, context: $context} + (if $url != "" then {target_url: $url} else {} end)' >"$tmp/body"
  code=$(host POST "$api/repos/$GITHUB_REPO/statuses/$status_sha" "$tmp/body")
  case $code in 2??) ;; *) say "GitHub didn't take the status ($code): $(shown "$out")"; return 1 ;; esac
}

# shellcheck source=lib.sh
. "$here/lib.sh"
main "$@"
