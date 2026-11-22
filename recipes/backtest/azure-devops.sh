#!/bin/sh
# Backtests the alert rules a pull request in Azure Repos (Azure DevOps)
# changes, and comments the report on it (see lib.sh for the rest). The
# arguments are lapilli verdict's: --prometheus URL, --since 14d, and the
# like.
#
#   AZURE_DEVOPS_TOKEN_FILE  a file holding a token that may comment on the
#                            pull request: the pipeline's System.AccessToken
#                            (its build service with Contribute to pull
#                            requests), or a personal access token with Code
#                            (read and write)
#   AZURE_DEVOPS_ORG_URL     such as https://dev.azure.com/example (default:
#                            Azure Pipelines' SYSTEM_COLLECTIONURI)
#   AZURE_DEVOPS_PROJECT     the project's name (default: SYSTEM_TEAMPROJECT)
#   AZURE_DEVOPS_REPO        the repository's name (default:
#                            BUILD_REPOSITORY_NAME)
#   PR                       the pull request's id (default:
#                            SYSTEM_PULLREQUEST_PULLREQUESTID)
#   TARGET_BRANCH            default, without TARGET, the branch it goes
#                            into (SYSTEM_PULLREQUEST_TARGETBRANCH), as the
#                            checkout fetched it: the order of a merge
#                            commit's parents isn't documented. Fetched only
#                            when the checkout didn't, which needs
#                            persistCredentials: true
#
# The comment is a thread of its own, closed: resolved, which the policy
# that comments be resolved doesn't wait for. The token's identity
# (connectionData's authenticatedUser) and a comment's author are told
# apart by their id, as Azure DevOps documents both.
#
# With STATUS=true (see lib.sh), a pull request status (POST
# .../pullRequests/ID/statuses) on the iteration whose source commit is
# the pull request's (SYSTEM_PULLREQUEST_SOURCECOMMITID), as GET
# .../pullRequests/ID/iterations says: genre lapilli, name backtest, which
# a branch policy's status check names lapilli/backtest; linking to the
# build. None is set when no iteration is at that commit: one on the pull
# request alone would stay after a later push. The token needs Code
# (status) (vso.code_status), or the build service Contribute to pull
# requests; not checked against an account.
#
# Through lapilli serve (LAPILLI_SERVE, see lib.sh), a plain token,
# LAPILLI_SERVE_TOKEN_FILE: a job's ID token in Azure Pipelines is for a
# service connection, made for Microsoft Entra (docs/design/backtests.md).
recipe=azure-devops.sh
here=$(dirname "$0")
: "${AZURE_DEVOPS_TOKEN_FILE:?a file holding a token that may comment on the pull request}"
[ -r "$AZURE_DEVOPS_TOKEN_FILE" ] || { echo "azure-devops.sh: AZURE_DEVOPS_TOKEN_FILE $AZURE_DEVOPS_TOKEN_FILE can't be read" >&2; exit 1; }
AZURE_DEVOPS_ORG_URL=${AZURE_DEVOPS_ORG_URL:-${SYSTEM_COLLECTIONURI:-}}
AZURE_DEVOPS_PROJECT=${AZURE_DEVOPS_PROJECT:-${SYSTEM_TEAMPROJECT:-}}
AZURE_DEVOPS_REPO=${AZURE_DEVOPS_REPO:-${BUILD_REPOSITORY_NAME:-}}
PR=${PR:-${SYSTEM_PULLREQUEST_PULLREQUESTID:-}}
if [ -z "${TARGET:-}" ] && [ -n "${SYSTEM_PULLREQUEST_TARGETBRANCH:-}" ]; then
  TARGET_BRANCH=${TARGET_BRANCH:-${SYSTEM_PULLREQUEST_TARGETBRANCH#refs/heads/}}
fi
# A pull request's build says what it goes into: a TARGET the job gives is
# its own (lib.sh's ci_base).
[ -z "${SYSTEM_PULLREQUEST_TARGETBRANCH:-}" ] || ci_base=1
: "${AZURE_DEVOPS_ORG_URL:?such as https://dev.azure.com/example}"
: "${AZURE_DEVOPS_PROJECT:?the project}"
: "${AZURE_DEVOPS_REPO:?the repository}"
org=${AZURE_DEVOPS_ORG_URL%/}
host_api=$org
project=$(printf '%s' "$AZURE_DEVOPS_PROJECT" | jq -sRr @uri)
repo_name=$(printf '%s' "$AZURE_DEVOPS_REPO" | jq -sRr @uri)
threads=$org/$project/_apis/git/repositories/$repo_name/pullRequests
# For STATUS: the pull request's source commit, and the build's page.
status_commit=${SYSTEM_PULLREQUEST_SOURCECOMMITID:-}
status_link=
case ${BUILD_BUILDID:-} in
  '' | *[!0-9]*) ;;
  *) status_link="$org/$project/_build/results?buildId=$BUILD_BUILDID" ;;
esac

host_auth() {
  printf 'Authorization: Basic %s\nAccept: application/json\n' "$(printf ':%s' "$(cat "$AZURE_DEVOPS_TOKEN_FILE")" | base64 | tr -d '\n')"
}

# host_me writes the identity the token is.
host_me() {
  code=$(host GET "$org/_apis/connectionData")
  case $code in 2??) ;; *) return 1 ;; esac
  jq -r '.authenticatedUser.id // ""' "$out"
}

# A pull request's threads, all of them in one answer; the comment is a
# thread's, so it is named THREAD/COMMENT.
find_comment() {
  code=$(host GET "$threads/$PR/threads?api-version=7.1")
  case $code in 2??) ;; *) say "Azure DevOps didn't list the pull request's threads ($code): $(shown "$out")"; return 1 ;; esac
  id=$(jq -r --arg me "$1" --arg m "$marker" '[.value[] | select(.isDeleted != true) | .id as $t | .comments[]?
    | select(.isDeleted != true and (.author.id // "") == $me and ((.content // "") | split("\n")[0] | rtrimstr("\r")) == $m)
    | "\($t)/\(.id)"][0] // ""' "$out")
  case $id in
    '') ;;
    *[!0-9/]* | */*/* | /* | */) say "Azure DevOps gave a comment with no number"; return 1 ;;
    */*) printf '%s' "$id" ;;
    *) say "Azure DevOps gave a comment with no number"; return 1 ;;
  esac
}

post_comment() {
  jq -Rs '{comments: [{parentCommentId: 0, content: ., commentType: 1}], status: "closed"}' "$1" >"$tmp/body"
  code=$(host POST "$threads/$PR/threads?api-version=7.1" "$tmp/body")
  case $code in 2??) ;; *) say "Azure DevOps didn't take the comment ($code): $(shown "$out")"; return 1 ;; esac
}

edit_comment() {
  jq -Rs '{content: .}' "$2" >"$tmp/body"
  code=$(host PATCH "$threads/$PR/threads/${1%/*}/comments/${1#*/}?api-version=7.1" "$tmp/body")
  case $code in 2??) ;; *) say "Azure DevOps didn't take the comment ($code): $(shown "$out")"; return 1 ;; esac
}

# A pull request status: pending, succeeded or failed, as lib.sh says
# pending, success or failure, on the latest iteration at the commit.
post_status() {
  case $1 in
    success) st=succeeded ;;
    pending) st=pending ;;
    *) st=failed ;;
  esac
  code=$(host GET "$threads/$PR/iterations?api-version=7.1")
  case $code in 2??) ;; *) say "Azure DevOps didn't list the pull request's iterations ($code): $(shown "$out")"; return 1 ;; esac
  it=$(jq -r --arg c "$status_sha" '[(if type == "object" then .value else . end)[]? | objects
    | select((.sourceRefCommit.commitId // "" | ascii_downcase) == $c) | .id | numbers] | max // empty' "$out" 2>/dev/null) || it=
  case $it in
    '' | *[!0-9]*)
      say "no iteration of the pull request is at $status_sha: no status set, as one on the pull request alone would stay after a later push"
      return 1
      ;;
  esac
  jq -n --arg state "$st" --arg text "$2" --arg name "${status_context#lapilli/}" --arg url "$status_url" --argjson it "$it" \
    '{state: $state, description: $text, context: {genre: "lapilli", name: $name}, iterationId: $it}
      + (if $url != "" then {targetUrl: $url} else {} end)' >"$tmp/body"
  code=$(host POST "$threads/$PR/statuses?api-version=7.1" "$tmp/body")
  case $code in 2??) ;; *) say "Azure DevOps didn't take the status ($code): $(shown "$out")"; return 1 ;; esac
}

# shellcheck source=lib.sh
. "$here/lib.sh"
main "$@"
