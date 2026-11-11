#!/bin/sh
# Keeps Lapilli's tasks in step with issues in a Linear team, as github.sh
# does with a GitHub repository. Run it every few minutes. An issue is
# closed into the team's first completed state, or canceled for a task
# dropped. Labels aren't given: Linear takes them by id (see linear.jq).
#
#   LINEAR_TOKEN_FILE  a file holding a personal API key
#   LINEAR_TEAM        the team's id
#   LINEAR_API         default https://api.linear.app/graphql
#   LINEAR_WEB         default https://linear.app
#
# The issue made for a task is in linear.jq. And Lapilli's side, as
# recipes/trackers/lib.sh says: LAPILLI_URL, LAPILLI_TOKEN_FILE,
# CLOSE_ISSUES, MAX_ISSUES, MAX_PAGES, SYNC_DAYS.
# shellcheck disable=SC2016 # $names in the GraphQL are its variables, not the shell's
set -eu
name=linear.sh
here=$(dirname "$0")
# shellcheck source=recipes/trackers/lib.sh
. "$here/lib.sh"
: "${LINEAR_TOKEN_FILE:?a file holding a Linear API key}"
: "${LINEAR_TEAM:?a team id}"
api=${LINEAR_API:-https://api.linear.app/graphql}
web=${LINEAR_WEB:-https://linear.app}
web=${web%/}
team=$LINEAR_TEAM
if ! printf '%s' "$team" | grep -Eq '^[0-9a-fA-F-]{36}$'; then
  say "LINEAR_TEAM is a team id, a UUID"
  exit 1
fi
li_token=$(cat "$LINEAR_TOKEN_FILE")

# gql QUERY VARIABLES: Linear's data, or a failure for anything but a 2xx
# without errors, what Linear said kept in $work/errors. Taken whole before
# it is read, so a failure isn't read as nothing.
gql() {
  printf '[]' >"$work/errors"
  out=$(printf 'Authorization: %s\n' "$li_token" | fetch -X POST -H @- \
    -H 'Content-Type: application/json' --data-binary "$(jq -cn --arg q "$1" --argjson v "$2" '{query: $q, variables: $v}')" "$api") || return 1
  if printf '%s' "$out" | jq -e '.errors // [] | length > 0' >/dev/null; then
    printf '%s' "$out" | jq -c '[.errors[]? | .message? | strings]' >"$work/errors"
    say "Linear said: $(cat "$work/errors")"
    return 1
  fi
  printf '%s' "$out" | jq -c '.data'
}

# ours: whether the issue on stdin is one this job made for the marker $m:
# made by $me, in the team, marked for $m.
ours=$kept'def ours: .creator.id == $me and .team.id == $team
  and (.description | kept($m));'

t_me() {
  out=$(gql 'query { viewer { id } }' '{}') || return 1
  printf '%s' "$out" | jq -r '.viewer.id // empty'
}

t_find() {
  out=$(gql 'query($team: ID!, $me: ID!, $m: String!) { issues(first: 20, filter: {team: {id: {eq: $team}}, creator: {id: {eq: $me}}, description: {contains: $m}}) { nodes { url description team { id } creator { id } } } }' \
    "$(jq -cn --arg team "$team" --arg me "$me" --arg m "$1" '{team: $team, me: $me, m: $m}')") || return 1
  printf '%s' "$out" | jq -r --arg m "$1" --arg me "$me" --arg team "$team" "$ours"'[.issues.nodes[] | select(ours)][0].url // empty'
}

t_create() {
  text=$(issue_text <"$1")
  vars=$(jq -c --arg text "$text" --arg team "$team" -f "$here/linear.jq" "$1")
  out=$(gql 'mutation($in: IssueCreateInput!) { issueCreate(input: $in) { success issue { url } } }' "$vars") || return 1
  printf '%s' "$out" | jq -r 'if .issueCreate.success then .issueCreate.issue.url else empty end'
}

t_ref() {
  printf '%s' "$1" | jq -Rr --arg w "$web/" \
    'if startswith($w) then ltrimstr($w) | capture("\\A[A-Za-z0-9_-]+/issue/(?<id>[A-Z][A-Z0-9]*-[0-9]+)(/[A-Za-z0-9_-]*)?\\z").id else empty end'
}

t_state() {
  if ! out=$(gql 'query($id: String!) { issue(id: $id) { updatedAt description team { id } creator { id } state { type } } }' "$(jq -cn --arg id "$1" '{id: $id}')"); then
    # Not there (deleted), in a team the key still reads.
    jq -e 'any(.[]; startswith("Entity not found"))' "$work/errors" >/dev/null || return 1
    gql 'query($team: String!) { team(id: $team) { id } }' "$(jq -cn --arg team "$team" '{team: $team}')" >/dev/null || return 1
    echo gone
    return 0
  fi
  printf '%s' "$out" | jq -r --arg m "$2" --arg me "$me" --arg team "$team" "$epoch$ours"'
    .issue | select(ours) | (.state.type as $t | if $t == "completed" then "done" elif $t == "canceled" then "dropped" else "open" end)
    + " " + (.updatedAt | epoch | tostring)'
}

t_close() {
  want=completed
  [ "$2" != dropped ] || want=canceled
  out=$(gql 'query($team: String!) { team(id: $team) { states { nodes { id type position } } } }' "$(jq -cn --arg team "$team" '{team: $team}')") || return 1
  state=$(printf '%s' "$out" | jq -r --arg t "$want" '[.team.states.nodes[] | select(.type == $t)] | sort_by(.position) | .[0].id // empty')
  if [ -z "$state" ]; then
    say "the team has no $want state"
    return 1
  fi
  out=$(gql 'mutation($id: String!, $state: String!) { issueUpdate(id: $id, input: {stateId: $state}) { success } }' \
    "$(jq -cn --arg id "$1" --arg state "$state" '{id: $id, state: $state}')") || return 1
  printf '%s' "$out" | jq -e '.issueUpdate.success' >/dev/null
}

# t_comments: the issue's comments, ten pages of 50 at most, by a cursor
# Linear gives, checked to be one before it is sent back.
t_comments() {
  after=
  pages=0
  while [ "$pages" -lt 10 ]; do
    pages=$((pages + 1))
    out=$(gql 'query($id: String!, $after: String) { issue(id: $id) { comments(first: 50, after: $after) { nodes { id body createdAt user { id name } botActor { id } } pageInfo { hasNextPage endCursor } } } }' \
      "$(jq -cn --arg id "$1" --arg a "$after" '{id: $id, after: (if $a == "" then null else $a end)}')") || return 1
    printf '%s' "$out" | jq -c --arg me "$me" '.issue.comments.nodes[] | {id: .id, author: (.user.name // "someone"),
      mine: (.user.id == $me), at: .createdAt, body: (.body // "")}'
    printf '%s' "$out" | jq -e '.issue.comments.pageInfo.hasNextPage' >/dev/null || return 0
    after=$(printf '%s' "$out" | jq -r '.issue.comments.pageInfo.endCursor // empty')
    printf '%s' "$after" | grep -Eq '^[A-Za-z0-9_=+/-]{1,200}$' || return 1
  done
}

t_comment() {
  out=$(gql 'mutation($in: CommentCreateInput!) { commentCreate(input: $in) { success } }' \
    "$(jq -Rsc --arg id "$1" '{in: {issueId: $id, body: .}}' "$2")") || return 1
  printf '%s' "$out" | jq -e '.commentCreate.success' >/dev/null
}

sync_tasks
