#!/bin/sh
# Keeps Lapilli's tasks in step with issues in a YouTrack project, Cloud or
# self-hosted, as github.sh does with a GitHub repository. Run it every few
# minutes.
#
#   YOUTRACK_URL          such as https://example.youtrack.cloud (a server of
#                         your own: https://HOST, or https://HOST/youtrack)
#   YOUTRACK_TOKEN_FILE   a file holding a permanent token
#   YOUTRACK_PROJECT      the project's ID, which its issues' IDs start
#                         with, such as OPS
#   YOUTRACK_WEB          where its issues' pages are (default YOUTRACK_URL)
#   YOUTRACK_STATE_FIELD  the project's state field (default State)
#   YOUTRACK_DONE         the state an issue done is moved to with
#                         CLOSE_ISSUES (default Fixed)
#   YOUTRACK_DROPPED      the state an issue dropped is moved to (default
#                         Won't fix)
#
# An issue's text ends with the line it is found again by, as GitHub's
# does, and it is found among those the token's own user reported. YouTrack
# tags are made, and shared, beforehand and given by id, so the issue made
# has none unless youtrack.jq gives them. An issue resolved closes its task:
# dropped when its state is YOUTRACK_DROPPED or says so (Won't fix,
# Duplicate, Obsolete, Incomplete, Can't reproduce), done otherwise; by when
# it was resolved (resolved). The issue made for a task is in youtrack.jq.
# And Lapilli's side, as recipes/trackers/lib.sh says: LAPILLI_URL,
# LAPILLI_TOKEN_FILE, CLOSE_ISSUES, MAX_ISSUES, MAX_PAGES, SYNC_DAYS,
# COMMENTS.
set -eu
name=youtrack.sh
here=$(dirname "$0")
# shellcheck source=recipes/trackers/lib.sh
. "$here/lib.sh"
: "${YOUTRACK_URL:?such as https://example.youtrack.cloud}"
: "${YOUTRACK_TOKEN_FILE:?a file holding a permanent token}"
: "${YOUTRACK_PROJECT:?the ID of the project, such as OPS}"
base=${YOUTRACK_URL%/}
web=${YOUTRACK_WEB:-$base}
web=${web%/}
tracker_api YOUTRACK_URL "$base"
project=$YOUTRACK_PROJECT
if ! printf '%s' "$project" | grep -Eq '^[A-Za-z][A-Za-z0-9_]{0,49}$'; then
  say "YOUTRACK_PROJECT is a project's ID, such as OPS"
  exit 1
fi
field=${YOUTRACK_STATE_FIELD:-State}
done_state=${YOUTRACK_DONE:-Fixed}
dropped_state=${YOUTRACK_DROPPED:-}
[ -n "$dropped_state" ] || dropped_state="Won't fix"
for v in "$field" "$done_state" "$dropped_state"; do
  if ! printf '%s' "$v" | grep -Eq "^[A-Za-z][A-Za-z0-9 ._'-]{0,63}\$"; then
    say "a state field or state is a name, such as State or Fixed, not $(printf '%s' "$v" | jq -Rc .)"
    exit 1
  fi
done
# One assignment the token, so set -e sees it refused. A permanent token is
# perm: or perm- and what follows.
yt_token=$(secret_line YOUTRACK_TOKEN_FILE "$YOUTRACK_TOKEN_FILE")

# yt HOW METHOD PATH [BODY]: YouTrack's answer, or a failure for anything
# but a 2xx, the request asked again or once (HOW, as lib.sh's ask_again
# says). Taken whole before it is read, so a failure isn't read as nothing.
# YouTrack gives only the attributes fields= names.
yt() {
  if [ $# -gt 3 ]; then
    printf 'Authorization: Bearer %s\n' "$yt_token" | fetch "$1" -X "$2" -H @- \
      -H 'Accept: application/json' -H 'Content-Type: application/json' --data-binary "$4" "$base/api$3"
  else
    printf 'Authorization: Bearer %s\n' "$yt_token" | fetch "$1" -X "$2" -H @- \
      -H 'Accept: application/json' "$base/api$3"
  fi
}

# ours: whether the issue on stdin is one this job made for the marker $m:
# reported by $me (which YouTrack doesn't let be changed), in the project,
# marked for $m.
# shellcheck disable=SC2016 # $me, $p and $m are jq's
ours=$kept'def ours: (.reporter.id // "") == $me and (.project.shortName // "") == $p
  and (.description | kept($m));'

t_me() {
  out=$(yt again GET "/users/me?fields=id,login") || return 1
  printf '%s' "$out" | jq -r '.id // empty'
}

# project_id: the project's database id, which an issue is made in, from
# the projects the token reads, ten pages of 100 at most, asked once a run.
# Nothing when it isn't among them.
project_id() {
  if [ ! -e "$work/project" ]; then
    skip=0
    found=
    while [ "$skip" -lt 1000 ]; do
      out=$(yt again GET "/admin/projects?fields=id,shortName&\$top=100&\$skip=$skip") || return 1
      found=$(printf '%s' "$out" | jq -r --arg p "$project" '[.[] | select(.shortName == $p) | .id | strings][0] // empty') || return 1
      [ -z "$found" ] || break
      [ "$(printf '%s' "$out" | jq 'length')" -eq 100 ] || break
      skip=$((skip + 100))
    done
    printf '%s' "$found" >"$work/project"
  fi
  id=$(cat "$work/project")
  [ -z "$id" ] || printf '%s' "$id" | grep -Eq '^[0-9]{1,10}-[0-9]{1,10}$' || return 1
  printf '%s' "$id"
}

t_find() {
  q=$(printf 'project: {%s} reporter: me "%s"' "$project" "$1" | jq -sRr @uri)
  out=$(yt again GET "/issues?query=$q&fields=idReadable,reporter(id),project(shortName),description&\$top=20") || return 1
  key=$(printf '%s' "$out" | jq -r --arg m "$1" --arg me "$me" --arg p "$project" "$ours"'[.[] | select(ours)][0].idReadable // empty')
  [ -z "$key" ] || printf '%s/issue/%s\n' "$web" "$key"
}

t_create() {
  if ! pid=$(project_id) || [ -z "$pid" ]; then
    say "the token doesn't read the project $project"
    return 1
  fi
  text=$(issue_text <"$1")
  body=$(jq -c --arg text "$text" --arg project "$pid" -f "$here/youtrack.jq" "$1")
  out=$(yt once POST "/issues?fields=idReadable" "$body") || return 1
  # Its ID as given: t_ref checks it is one before it goes anywhere.
  key=$(printf '%s' "$out" | jq -r '.idReadable // empty')
  [ -z "$key" ] || printf '%s/issue/%s\n' "$web" "$key"
}

# An issue's page is WEB/issue/OPS-12, and its summary after it.
t_ref() {
  printf '%s' "$1" | jq -Rr --arg w "$web/issue/" --arg p "$project" \
    'if startswith($w) then ltrimstr($w) | capture("\\A(?<k>" + $p + "-[0-9]{1,10})(/[A-Za-z0-9_.~%-]*)?\\z").k else empty end'
}

t_state() {
  if ! out=$(yt again GET "/issues/$1?fields=idReadable,reporter(id),project(shortName),description,updated,resolved,customFields(name,value(name))"); then
    # Not there (deleted), in a project the token still reads.
    gone || return 1
    pid=$(project_id) || return 1
    [ -n "$pid" ] || return 1
    echo gone
    return 0
  fi
  printf '%s' "$out" | jq -r --arg m "$2" --arg me "$me" --arg p "$project" --arg f "$field" \
    --arg d "$done_state" --arg x "$dropped_state" "$ours"'
    select(ours) | ([.customFields[]? | select(.name == $f) | .value.name? | strings][0] // "") as $s
    | (if .resolved == null then "open" elif $s == $d then "done"
       elif $s == $x or ($s | test("won.t|duplicate|obsolete|incomplete|can.t|cannot|rejected|declined|invalid"; "i")) then "dropped"
       else "done" end)
    + " " + (.updated / 1000 | floor | tostring)
    + " " + (.resolved // .updated | . / 1000 | floor | tostring)'
}

# t_close: the state field set to the state, as an issue's custom field is
# (POST /api/issues/ID). A field a state-machine workflow keeps is moved by
# its transitions alone, and refuses this: close those issues in YouTrack.
t_close() {
  to=$done_state
  [ "$2" != dropped ] || to=$dropped_state
  yt again POST "/issues/$1?fields=id" "$(jq -cn --arg f "$field" --arg s "$to" \
    '{customFields: [{name: $f, "$type": "StateIssueCustomField", value: {name: $s, "$type": "StateBundleElement"}}]}')" >/dev/null
}

# t_comments: the issue's comments, ten pages of 100 at most, those
# everyone who reads the issue may: not one deleted, nor one limited to
# some groups or users (its visibility).
t_comments() {
  skip=0
  while [ "$skip" -lt 1000 ]; do
    out=$(yt again GET "/issues/$1/comments?fields=id,text,created,updated,deleted,author(id,login,fullName),visibility(\$type)&\$top=100&\$skip=$skip") || return 1
    printf '%s' "$out" | jq -c --arg me "$me" '.[] | select((.deleted // false | not) and ((.visibility // {})["$type"] // "") != "LimitedVisibility")
      | {id: (.id | tostring), author: (.author.fullName // .author.login // "someone"), mine: ((.author.id // "") == $me),
         at: (.created / 1000 | floor | todate), edited: (.updated // .created | . / 1000 | floor | todate), body: (.text // "")}' || return 1
    [ "$(printf '%s' "$out" | jq 'length')" -eq 100 ] || return 0
    skip=$((skip + 100))
  done
  : >"$work/capped"
}

t_comment() {
  out=$(yt once POST "/issues/$1/comments?fields=id" "$(jq -Rsc '{text: .}' "$2")") || return 1
  printf '%s' "$out" | jq -r '.id // empty | tostring'
}

# t_edit: a comment is changed by a POST to it.
t_edit() {
  printf '%s' "$2" | grep -Eq '^[0-9]{1,10}-[0-9]{1,10}$' || return 1
  yt again POST "/issues/$1/comments/$2?fields=id" "$(jq -Rsc '{text: .}' "$3")" >/dev/null
}

sync_tasks
