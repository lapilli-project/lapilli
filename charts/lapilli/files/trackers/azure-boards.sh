#!/bin/sh
# Keeps Lapilli's tasks in step with work items in an Azure Boards project,
# as github.sh does with a GitHub repository. Run it every few minutes.
#
#   AZURE_DEVOPS_ORG         the organization, such as example
#   AZURE_DEVOPS_PROJECT     the project, such as alerts
#   AZURE_DEVOPS_TOKEN_FILE  a file holding a personal access token with
#                            Work Items (read and write)
#   AZURE_DEVOPS_URL         default https://dev.azure.com (Azure DevOps
#                            Server: https://HOST/COLLECTION-PARENT)
#   AZURE_DEVOPS_WEB         where its work items' pages are (default
#                            AZURE_DEVOPS_URL)
#   AZURE_BOARDS_TYPE        the work item type made (default Task)
#   AZURE_BOARDS_DONE        the state an item done is moved to with
#                            CLOSE_ISSUES (default Done; Closed in the Agile
#                            and CMMI processes)
#   AZURE_BOARDS_REMOVED     the state an item dropped is moved to (default
#                            Removed; the Basic process has none: Done)
#
# A work item's description is HTML, so the task's marker is its tag
# lapilli-task-ID, as in Jira. Its state is read by name: Removed (or
# AZURE_BOARDS_REMOVED) closes the task as dropped; Done, Closed (or
# AZURE_BOARDS_DONE) as done; any other is open. The work item made for a
# task is in azure-boards.jq. And Lapilli's side, as recipes/trackers/lib.sh
# says: LAPILLI_URL, LAPILLI_TOKEN_FILE, CLOSE_ISSUES, MAX_ISSUES,
# MAX_PAGES, SYNC_DAYS, COMMENTS.
set -eu
name=azure-boards.sh
here=$(dirname "$0")
# shellcheck source=recipes/trackers/lib.sh
. "$here/lib.sh"
: "${AZURE_DEVOPS_ORG:?the organization, such as example}"
: "${AZURE_DEVOPS_PROJECT:?the project, such as alerts}"
: "${AZURE_DEVOPS_TOKEN_FILE:?a file holding a personal access token}"
base=${AZURE_DEVOPS_URL:-https://dev.azure.com}
base=${base%/}
web=${AZURE_DEVOPS_WEB:-$base}
web=${web%/}
org=$AZURE_DEVOPS_ORG
project=$AZURE_DEVOPS_PROJECT
type=${AZURE_BOARDS_TYPE:-Task}
done_state=${AZURE_BOARDS_DONE:-Done}
removed_state=${AZURE_BOARDS_REMOVED:-Removed}
if ! printf '%s' "$org" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9-]{0,49}$'; then
  say "AZURE_DEVOPS_ORG is an organization's name, such as example"
  exit 1
fi
if ! printf '%s' "$project" | grep -Eq '^[A-Za-z0-9_][A-Za-z0-9 ._-]{0,63}$'; then
  say "AZURE_DEVOPS_PROJECT is a project's name, such as alerts"
  exit 1
fi
for v in "$type" "$done_state" "$removed_state"; do
  if ! printf '%s' "$v" | grep -Eq '^[A-Za-z][A-Za-z0-9 ]{0,63}$'; then
    say "a work item type or state is a name, such as Task or Done, not $(printf '%s' "$v" | jq -Rc .)"
    exit 1
  fi
done
penc=$(printf '%s' "$project" | jq -sRr @uri)
tenc=$(printf '%s' "$type" | jq -sRr @uri)
auth=$(printf ':%s' "$(cat "$AZURE_DEVOPS_TOKEN_FILE")" | base64 | tr -d '\n')

# az METHOD PATH [BODY [TYPE]]: Azure DevOps's answer, or a failure for
# anything but a 2xx. Taken whole before it is read, so a failure isn't
# read as nothing.
az() {
  if [ $# -gt 2 ]; then
    printf 'Authorization: Basic %s\n' "$auth" | fetch -X "$1" -H @- -H 'Accept: application/json' \
      -H "Content-Type: ${4:-application/json}" --data-binary "$3" "$base/$org$2"
  else
    printf 'Authorization: Basic %s\n' "$auth" | fetch -X "$1" -H @- -H 'Accept: application/json' "$base/$org$2"
  fi
}

# ours: whether the work item on stdin is one this job made for the marker
# $m: created by $me, tagged lapilli, its one tag lapilli-task-ID $m.
# shellcheck disable=SC2016 # $me and $m are jq's
ours='def tags: .fields["System.Tags"] // "" | split(";") | map(gsub("^ +| +$"; "")) | map(select(. != ""));
  def ours: (.fields["System.CreatedBy"].id // "" | ascii_downcase) == ($me | ascii_downcase)
  and (tags | index("lapilli")) and ([tags[] | select(startswith("lapilli-task-"))] == [$m]);'

t_me() {
  out=$(az GET "/_apis/connectionData") || return 1
  printf '%s' "$out" | jq -r '.authenticatedUser.id // empty'
}

t_find() {
  q=$(jq -cn --arg m "$1" '([39] | implode) as $q | {query: ("SELECT [System.Id] FROM workitems WHERE [System.TeamProject] = @project AND [System.Tags] CONTAINS " + $q + $m + $q + " AND [System.CreatedBy] = @Me")}')
  out=$(az POST "/$penc/_apis/wit/wiql?api-version=7.1" "$q") || return 1
  ids=$(printf '%s' "$out" | jq -r '[.workItems[]?.id | numbers][0:20] | map(tostring) | join(",")')
  [ -n "$ids" ] || return 0
  out=$(az GET "/$penc/_apis/wit/workitems?ids=$ids&fields=System.Tags,System.CreatedBy&api-version=7.1") || return 1
  id=$(printf '%s' "$out" | jq -r --arg m "$1" --arg me "$me" "$ours"'[.value[] | select(ours)][0].id // empty')
  [ -z "$id" ] || printf '%s/%s/%s/_workitems/edit/%s\n' "$web" "$org" "$penc" "$id"
}

t_create() {
  text=$(issue_text <"$1")
  marker=lapilli-task-$(jq -r '.id' "$1")
  body=$(jq -c --arg text "$text" --arg marker "$marker" -f "$here/azure-boards.jq" "$1")
  out=$(az POST "/$penc/_apis/wit/workitems/%24$tenc?api-version=7.1" "$body" application/json-patch+json) || return 1
  # Its id as given: t_ref checks it is one before it goes anywhere.
  id=$(printf '%s' "$out" | jq -r '.id // empty | tostring')
  [ -z "$id" ] || printf '%s/%s/%s/_workitems/edit/%s\n' "$web" "$org" "$penc" "$id"
}

t_ref() {
  printf '%s' "$1" | jq -Rr --arg w "$web/$org/$penc/_workitems/edit/" \
    'if startswith($w) then ltrimstr($w) | select(test("\\A[0-9]{1,10}\\z")) else empty end'
}

t_state() {
  if ! out=$(az GET "/$penc/_apis/wit/workitems/$1?api-version=7.1"); then
    # Not there (deleted), in a project the token still reads.
    gone || return 1
    az GET "/_apis/projects/$penc?api-version=7.1" >/dev/null || return 1
    echo gone
    return 0
  fi
  printf '%s' "$out" | jq -r --arg m "$2" --arg me "$me" --arg d "$done_state" --arg x "$removed_state" "$epoch$ours"'
    select(ours) | .fields["System.State"] as $s
    | (if $s == $x or $s == "Removed" then "dropped" elif $s == $d or $s == "Done" or $s == "Closed" then "done" else "open" end)
    + " " + (.fields["System.ChangedDate"] | epoch | tostring)'
}

t_close() {
  to=$done_state
  [ "$2" != dropped ] || to=$removed_state
  az PATCH "/$penc/_apis/wit/workitems/$1?api-version=7.1" \
    "$(jq -cn --arg s "$to" '[{op: "add", path: "/fields/System.State", value: $s}]')" application/json-patch+json >/dev/null
}

# t_comments: the work item's comments, ten pages at most, by a
# continuation token checked before it is sent back; its text is HTML,
# read as text.
t_comments() {
  token=
  pages=0
  while [ "$pages" -lt 10 ]; do
    pages=$((pages + 1))
    url="/$penc/_apis/wit/workItems/$1/comments?api-version=7.1-preview.4&order=asc&%24top=200"
    [ -z "$token" ] || url="$url&continuationToken=$token"
    out=$(az GET "$url") || return 1
    printf '%s' "$out" | jq -c --arg me "$me" '.comments[]? | select(.isDeleted | not)
      | {id: ((.id // .commentId) | tostring), author: (.createdBy.displayName // "someone"),
         mine: ((.createdBy.id // "" | ascii_downcase) == ($me | ascii_downcase)), at: .createdDate,
         body: (.text // "" | gsub("<br */?>|</p>|</div>"; "\n") | gsub("<[^>]*>"; "")
           | gsub("&lt;"; "<") | gsub("&gt;"; ">") | gsub("&quot;"; "\"") | gsub("&#39;"; ([39] | implode)) | gsub("&nbsp;"; " ") | gsub("&amp;"; "&")
           | rtrimstr("\n"))}'
    token=$(printf '%s' "$out" | jq -r '.continuationToken // empty')
    [ -n "$token" ] || return 0
    printf '%s' "$token" | grep -Eq '^[A-Za-z0-9+/=_-]{1,500}$' || return 1
    token=$(printf '%s' "$token" | jq -sRr @uri)
  done
}

# t_comment: in Markdown, as Azure Boards takes it with format=markdown.
t_comment() {
  az POST "/$penc/_apis/wit/workItems/$1/comments?format=markdown&api-version=7.1-preview.4" "$(jq -Rsc '{text: .}' "$2")" >/dev/null
}

sync_tasks
