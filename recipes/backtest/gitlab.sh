#!/bin/sh
# Backtests the alert rules a merge request on GitLab changes, and comments
# the report on it (see lib.sh for the rest). The arguments are lapilli
# verdict's: --prometheus URL, --since 14d, and the like.
#
#   GITLAB_TOKEN_FILE  a file holding a token that may comment on the merge
#                      request (a project access token, scope api, role
#                      Reporter)
#   GITLAB_PROJECT     the project's id, or its path: example/monitoring
#                      (default: GitLab CI's CI_MERGE_REQUEST_PROJECT_ID,
#                      the project the merge request is in, which for a
#                      fork's isn't CI_PROJECT_ID)
#   GITLAB_API         default GitLab CI's CI_API_V4_URL, or
#                      https://gitlab.com/api/v4
#   PR                 the merge request's iid (default: GitLab CI's
#                      CI_MERGE_REQUEST_IID)
#   TARGET             default, in a merged results pipeline, the target
#                      branch's commit (CI_MERGE_REQUEST_TARGET_BRANCH_SHA);
#                      in a merge train's, HEAD^1, the first parent of the
#                      merge the train made (CI_COMMIT_SHA, its second
#                      parent CI_MERGE_REQUEST_SOURCE_BRANCH_SHA), which
#                      HEAD must be; HEAD^1, lib.sh's default, written out
#                      is none given
#   TARGET_BRANCH      default, in another merge request pipeline, the
#                      target branch (CI_MERGE_REQUEST_TARGET_BRANCH_NAME),
#                      fetched from origin with the job's own credentials
#                      unless the clone has it: the rules are compared from
#                      where the two parted, as before, and INCIDENTS is
#                      the branch's as it is now. A name the recipe doesn't
#                      take as a branch's (but letters, digits and . _ / -)
#                      leaves the merge base (CI_MERGE_REQUEST_DIFF_BASE_SHA),
#                      INCIDENTS its, the job's log saying so
#
# With STATUS=true (see lib.sh), a commit status (POST
# /projects/ID/statuses/SHA) on the merge request's source commit
# (CI_MERGE_REQUEST_SOURCE_BRANCH_SHA in a merged results pipeline, else
# CI_COMMIT_SHA), as GitLab's reference says to for a commit of a merge
# request, linking to the job (CI_JOB_URL): an external job of the
# pipeline (CI_PIPELINE_ID) when it is that pipeline's commit, or else of
# the one GitLab finds or makes for it. The token needs the Developer role.
# Of a merge request into a branch other than the project's default
# (CI_DEFAULT_BRANCH), it is lapilli/backtest (into BRANCH). In a pipeline
# of no merge request (a branch's), the branch is TARGET_BRANCH, and with
# none, no status is set.
#
# Through lapilli serve (LAPILLI_SERVE, see lib.sh), the job names its ID
# token for serve's audience under id_tokens:, as LAPILLI_ID_TOKEN:
#
#   id_tokens:
#     LAPILLI_ID_TOKEN:
#       aud: https://lapilli-ci.example.com
recipe=gitlab.sh
here=$(dirname "$0")
: "${GITLAB_TOKEN_FILE:?a file holding a token that may comment on the merge request}"
[ -r "$GITLAB_TOKEN_FILE" ] || { echo "gitlab.sh: GITLAB_TOKEN_FILE $GITLAB_TOKEN_FILE can't be read" >&2; exit 1; }
GITLAB_PROJECT=${GITLAB_PROJECT:-${CI_MERGE_REQUEST_PROJECT_ID:-${CI_PROJECT_ID:-}}}
: "${GITLAB_PROJECT:?the project id or path, such as example/monitoring}"
api=${GITLAB_API:-${CI_API_V4_URL:-https://gitlab.com/api/v4}}
PR=${PR:-${CI_MERGE_REQUEST_IID:-}}
# A merge request's pipeline says what it goes into: TARGET, the script's
# or the job's, is taken as it is (lib.sh's ci_base).
[ -z "${CI_MERGE_REQUEST_IID:-}" ] || ci_base=1
# HEAD^1, lib.sh's default, written out is none given, as lib.sh takes it:
# of a merge request's own commit, a merge its author made, it is
# whatever first parent they chose.
[ "${TARGET:-}" != HEAD^1 ] || TARGET=
# A merge train's pipeline (CI_MERGE_REQUEST_EVENT_TYPE merge_train) is of
# a merge commit, CI_COMMIT_SHA, GitLab's merge request pipelines'
# reference says, of the source branch's head
# (CI_MERGE_REQUEST_SOURCE_BRANCH_SHA) on the target branch with the merge
# requests ahead in the train: its first parent is the commit the
# pipeline is based on, a diff from which to CI_COMMIT_SHA holds the merge
# request's changes alone, as CI_MERGE_REQUEST_PIPELINE_BASE_SHA is
# described. The reference says CI_MERGE_REQUEST_TARGET_BRANCH_SHA of
# merged results pipelines alone, so it isn't taken here. The change is
# that merge, compared against its first parent (lib.sh's ci_merge and
# ci_head), INCIDENTS that parent's; a HEAD that isn't such a merge, its
# second parent the source branch's head, is refused (ci_merge_only),
# gates or not, as its base would be a commit GitLab doesn't say.
train=
if [ "${CI_MERGE_REQUEST_EVENT_TYPE:-}" = merge_train ] && [ -n "${CI_MERGE_REQUEST_IID:-}" ]; then
  train=1
  train_hash() {
    case $1 in '' | *[!0-9a-f]*) return 1 ;; esac
    [ "${#1}" -eq 40 ] || [ "${#1}" -eq 64 ]
  }
  if ! train_hash "${CI_COMMIT_SHA:-}" || ! train_hash "${CI_MERGE_REQUEST_SOURCE_BRANCH_SHA:-}"; then
    echo "gitlab.sh: a merge train's pipeline whose CI_COMMIT_SHA and CI_MERGE_REQUEST_SOURCE_BRANCH_SHA aren't two commits' full hashes: refused, as what the train merged can't be told" >&2
    exit 1
  fi
  ci_head=$CI_MERGE_REQUEST_SOURCE_BRANCH_SHA ci_merge=$CI_COMMIT_SHA
fi
if [ -z "${TARGET:-}" ] && [ -z "${TARGET_BRANCH:-}" ] && [ -n "$train" ]; then
  ci_merge_only=1
elif [ -z "${TARGET:-}" ] && [ -z "${TARGET_BRANCH:-}" ]; then
  if [ -n "${CI_MERGE_REQUEST_TARGET_BRANCH_SHA:-}" ]; then
    TARGET=$CI_MERGE_REQUEST_TARGET_BRANCH_SHA
  else
    # Without merged results, the target branch itself, not the merge base
    # (CI_MERGE_REQUEST_DIFF_BASE_SHA): INCIDENTS is read from what the
    # merge request goes into as it is, which where it branched from isn't.
    # Its name is the target project's, held to what lib.sh takes as a
    # branch's before it is used.
    case ${CI_MERGE_REQUEST_TARGET_BRANCH_NAME:-} in
      '') TARGET=${CI_MERGE_REQUEST_DIFF_BASE_SHA:-} ;;
      -* | *[!A-Za-z0-9._/-]* | *..* | */ | /*)
        echo "gitlab.sh: the target branch's name isn't one taken as TARGET_BRANCH: compared with the merge base, INCIDENTS read from it" >&2
        TARGET=${CI_MERGE_REQUEST_DIFF_BASE_SHA:-}
        ;;
      *) TARGET_BRANCH=$CI_MERGE_REQUEST_TARGET_BRANCH_NAME ;;
    esac
  fi
fi
case $GITLAB_PROJECT in *[!A-Za-z0-9._/-]* | /* | */) echo "gitlab.sh: GITLAB_PROJECT $GITLAB_PROJECT: want an id or a path" >&2; exit 1 ;; esac
host_api=$api
project=$(printf '%s' "$GITLAB_PROJECT" | jq -sRr @uri)
# For STATUS: the merge request's source commit, and the job's page; and,
# for the status's name, the project's default branch and the one the
# merge request goes into (lib.sh).
status_commit=${CI_MERGE_REQUEST_SOURCE_BRANCH_SHA:-${CI_COMMIT_SHA:-}}
status_link=${CI_JOB_URL:-}
ci_default=${CI_DEFAULT_BRANCH:-}
ci_into=${CI_MERGE_REQUEST_TARGET_BRANCH_NAME:-}

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

# A commit status: running, success, or failed, as lib.sh says pending,
# success or failure. Asked again once when GitLab answers 409: another
# status of the commit was being set.
post_status() {
  case $1 in
    success) st=success ;;
    pending) st=running ;;
    *) st=failed ;;
  esac
  pipeline=
  if [ "$status_sha" = "${CI_COMMIT_SHA:-}" ]; then
    case ${CI_PIPELINE_ID:-} in '' | *[!0-9]*) ;; *) pipeline=$CI_PIPELINE_ID ;; esac
  fi
  jq -n --arg state "$st" --arg text "$2" --arg name "$status_context" --arg url "$status_url" --arg pipeline "$pipeline" \
    '{state: $state, description: $text, name: $name} + (if $url != "" then {target_url: $url} else {} end)
      + (if $pipeline != "" then {pipeline_id: ($pipeline | tonumber)} else {} end)' >"$tmp/body"
  code=$(host POST "$api/projects/$project/statuses/$status_sha" "$tmp/body")
  if [ "$code" = 409 ]; then
    sleep 1
    code=$(host POST "$api/projects/$project/statuses/$status_sha" "$tmp/body")
  fi
  case $code in 2??) ;; *) say "GitLab didn't take the status ($code): $(shown "$out")"; return 1 ;; esac
}

# shellcheck source=lib.sh
. "$here/lib.sh"
main "$@"
