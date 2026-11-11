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
# without errors.
gql() {
  out=$(printf 'Authorization: %s\n' "$li_token" | curl -fsS --max-time 30 --retry 3 -X POST -H @- \
    -H 'Content-Type: application/json' --data-binary "$(jq -cn --arg q "$1" --argjson v "$2" '{query: $q, variables: $v}')" "$api") || return 1
  if printf '%s' "$out" | jq -e '.errors // [] | length > 0' >/dev/null; then
    say "Linear said: $(printf '%s' "$out" | jq -c '[.errors[].message]')"
    return 1
  fi
  printf '%s' "$out" | jq -c '.data'
}

t_find() {
  gql 'query($team: ID!, $m: String!) { issues(first: 20, filter: {team: {id: {eq: $team}}, description: {contains: $m}}) { nodes { url description } } }' \
    "$(jq -cn --arg team "$team" --arg m "$1" '{team: $team, m: $m}')" | jq -r --arg m "$1" \
    '[.issues.nodes[] | select(.description // "" | test("\\b" + $m + "\\b"))][0].url // empty'
}

t_create() {
  text=$(issue_text <"$1")
  vars=$(jq -c --arg text "$text" --arg team "$team" -f "$here/linear.jq" "$1")
  gql 'mutation($in: IssueCreateInput!) { issueCreate(input: $in) { success issue { url } } }' "$vars" |
    jq -r 'if .issueCreate.success then .issueCreate.issue.url else empty end'
}

t_ref() {
  printf '%s' "$1" | jq -Rr --arg w "$web/" \
    'if startswith($w) then ltrimstr($w) | capture("\\A[A-Za-z0-9_-]+/issue/(?<id>[A-Z][A-Z0-9]*-[0-9]+)(/[A-Za-z0-9_-]*)?\\z").id else empty end'
}

t_state() {
  gql 'query($id: String!) { issue(id: $id) { updatedAt state { type } } }' "$(jq -cn --arg id "$1" '{id: $id}')" | jq -r "$epoch"'
    (.issue.state.type as $t | if $t == "completed" then "done" elif $t == "canceled" then "dropped" else "open" end)
    + " " + (.issue.updatedAt | epoch | tostring)'
}

t_close() {
  want=completed
  [ "$2" != dropped ] || want=canceled
  state=$(gql 'query($team: String!) { team(id: $team) { states { nodes { id type position } } } }' "$(jq -cn --arg team "$team" '{team: $team}')" |
    jq -r --arg t "$want" '[.team.states.nodes[] | select(.type == $t)] | sort_by(.position) | .[0].id // empty')
  if [ -z "$state" ]; then
    say "the team has no $want state"
    return 1
  fi
  gql 'mutation($id: String!, $state: String!) { issueUpdate(id: $id, input: {stateId: $state}) { success } }' \
    "$(jq -cn --arg id "$1" --arg state "$state" '{id: $id, state: $state}')" | jq -e '.issueUpdate.success' >/dev/null
}

sync_tasks
