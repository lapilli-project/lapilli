#!/bin/sh
# Keeps Lapilli's tasks in step with stories in a Shortcut workspace, as
# github.sh does with a GitHub repository. Run it every few minutes.
#
#   SHORTCUT_TOKEN_FILE  a file holding an API token
#   SHORTCUT_STATE       the id of the workflow state stories are made in
#   SHORTCUT_DONE_STATE  the id of the state a story done is moved to with
#                        CLOSE_ISSUES (default: the first done state of its
#                        workflow); a story dropped is archived
#   SHORTCUT_API         default https://api.app.shortcut.com/api/v3
#   SHORTCUT_WEB         default https://app.shortcut.com
#
# A story in a done state closes its task as done; one archived, as
# dropped. The story made for a task is in shortcut.jq. And Lapilli's side,
# as recipes/trackers/lib.sh says: LAPILLI_URL, LAPILLI_TOKEN_FILE,
# CLOSE_ISSUES, MAX_ISSUES, MAX_PAGES, SYNC_DAYS, COMMENTS.
set -eu
name=shortcut.sh
here=$(dirname "$0")
# shellcheck source=recipes/trackers/lib.sh
. "$here/lib.sh"
: "${SHORTCUT_TOKEN_FILE:?a file holding a Shortcut API token}"
: "${SHORTCUT_STATE:?the id of the workflow state stories are made in}"
api=${SHORTCUT_API:-https://api.app.shortcut.com/api/v3}
api=${api%/}
web=${SHORTCUT_WEB:-https://app.shortcut.com}
web=${web%/}
number SHORTCUT_STATE "$SHORTCUT_STATE"
[ -z "${SHORTCUT_DONE_STATE:-}" ] || number SHORTCUT_DONE_STATE "$SHORTCUT_DONE_STATE"
sc_token=$(cat "$SHORTCUT_TOKEN_FILE")

# sc METHOD PATH [BODY]: Shortcut's answer, or a failure for anything but a
# 2xx. Taken whole before it is read, so a failure isn't read as nothing.
sc() {
  if [ $# -gt 2 ]; then
    printf 'Shortcut-Token: %s\n' "$sc_token" | fetch -X "$1" -H @- \
      -H 'Content-Type: application/json' --data-binary "$3" "$api$2"
  else
    printf 'Shortcut-Token: %s\n' "$sc_token" | fetch -X "$1" -H @- "$api$2"
  fi
}

# ours: whether the story on stdin is one this job made for the marker $m:
# requested by $me, labelled lapilli, marked for $m.
# shellcheck disable=SC2016 # $me and $m are jq's
ours=$kept'def ours: (.requested_by_id // "") == $me
  and ([.labels[]?.name] | index("lapilli")) and (.description | kept($m));'

# t_me: the token's member; its mention name, which search takes, and its
# workspace, which a story's link names, are kept.
t_me() {
  out=$(sc GET /member) || return 1
  printf '%s' "$out" | jq -r '.mention_name // empty' >"$work/mention"
  printf '%s' "$out" | jq -r '.workspace2.url_slug // empty' >"$work/slug"
  printf '%s' "$out" | jq -r '.id // empty'
}

# slug: the workspace's, which a story's link names: asked of /member the
# first time it is needed, which may be before who the token is.
slug() {
  if [ ! -s "$work/slug" ]; then
    out=$(sc GET /member) || return 1
    printf '%s' "$out" | jq -r '.workspace2.url_slug // empty' >"$work/slug"
  fi
  s=$(cat "$work/slug")
  printf '%s' "$s" | grep -Eq '^[a-z0-9][a-z0-9-]{0,63}$' || return 1
  printf '%s' "$s"
}

t_find() {
  mention=$(cat "$work/mention")
  printf '%s' "$mention" | grep -Eq '^[A-Za-z0-9._-]{1,64}$' || return 1
  q=$(printf 'label:lapilli requester:%s description:"%s"' "$mention" "$1" | jq -sRr @uri)
  out=$(sc GET "/search/stories?query=$q&page_size=20&detail=full") || return 1
  printf '%s' "$out" | jq -r --arg m "$1" --arg me "$me" "$ours"'[.data[] | select(ours)][0].id // empty' >"$work/found"
  id=$(cat "$work/found")
  [ -z "$id" ] || printf '%s/%s/story/%s\n' "$web" "$(slug)" "$id"
}

t_create() {
  text=$(issue_text <"$1")
  body=$(jq -c --arg text "$text" --arg state "$SHORTCUT_STATE" -f "$here/shortcut.jq" "$1")
  out=$(sc POST /stories "$body") || return 1
  # Its id as given: t_ref checks it is one before it goes anywhere.
  id=$(printf '%s' "$out" | jq -r '.id // empty | tostring')
  [ -z "$id" ] || printf '%s/%s/story/%s\n' "$web" "$(slug)" "$id"
}

# A story's link is WEB/WORKSPACE/story/ID, and a title after it.
t_ref() {
  s=$(slug) || return 0
  printf '%s' "$1" | jq -Rr --arg w "$web/$s/story/" \
    'if startswith($w) then ltrimstr($w) | capture("\\A(?<id>[0-9]{1,12})(/[A-Za-z0-9_-]*)?\\z").id else empty end'
}

# done_states: the ids of every workflow's done states, asked once a run.
done_states() {
  if [ ! -s "$work/done" ]; then
    out=$(sc GET /workflows) || return 1
    printf '%s' "$out" | jq -c '[.[].states[]? | select(.type == "done") | .id]' >"$work/done"
  fi
  cat "$work/done"
}

t_state() {
  if ! out=$(sc GET "/stories/$1"); then
    # Not there (deleted), in a workspace the token still reads.
    gone || return 1
    sc GET /workflows >/dev/null || return 1
    echo gone
    return 0
  fi
  printf '%s' "$out" >"$work/story"
  dones=$(done_states) || return 1
  jq -r --arg m "$2" --arg me "$me" --argjson dones "$dones" "$epoch$ours"'
    select(ours) | (if .archived then "dropped" elif (.workflow_state_id as $s | $dones | index($s)) then "done" else "open" end)
    + " " + (.updated_at | epoch | tostring)' "$work/story"
}

t_close() {
  if [ "$2" = dropped ]; then
    sc PUT "/stories/$1" '{"archived":true}' >/dev/null
    return
  fi
  to=${SHORTCUT_DONE_STATE:-}
  if [ -z "$to" ]; then
    out=$(sc GET /workflows) || return 1
    story=$(sc GET "/stories/$1") || return 1
    wf=$(printf '%s' "$story" | jq -r '.workflow_id // empty')
    to=$(printf '%s' "$out" | jq -r --arg wf "$wf" '[.[] | select((.id | tostring) == $wf) | .states[] | select(.type == "done")][0].id // empty')
  fi
  if [ -z "$to" ]; then
    say "story $1's workflow has no done state: set SHORTCUT_DONE_STATE"
    return 1
  fi
  number "a workflow state" "$to"
  sc PUT "/stories/$1" "$(jq -cn --argjson s "$to" '{workflow_state_id: $s}')" >/dev/null
}

# t_comments: the story's comments, as the story gives them, people's;
# who wrote each by their mention name, from the workspace's members,
# asked once a run.
t_comments() {
  out=$(sc GET "/stories/$1") || return 1
  if [ ! -s "$work/members" ]; then
    sc GET /members | jq -c 'map({key: .id, value: (.profile.mention_name // .profile.name // "someone")}) | from_entries' >"$work/members" || return 1
  fi
  printf '%s' "$out" | jq -c --arg me "$me" --slurpfile who "$work/members" '.comments[]? | select(.deleted | not)
    | {id: (.id | tostring), author: ($who[0][.author_id // ""] // "someone"), mine: (.author_id == $me),
       at: .created_at, body: (.text // "")}'
}

t_comment() {
  sc POST "/stories/$1/comments" "$(jq -Rsc '{text: .}' "$2")" >/dev/null
}

sync_tasks
