#!/bin/sh
# Writes the Jira issues a search finds to stdout, as Lapilli's incidents file
# takes them. Works for Jira Service Management's incidents, and for
# anything else kept in Jira Cloud. By default an issue's start is when it
# was created, its end its resolution date, and its components the label
# service; where your incidents keep these in fields of their own, say so:
#
#   JIRA_URL          such as https://example.atlassian.net
#   JIRA_EMAIL        the account's email
#   JIRA_TOKEN_FILE   a file holding its API token
#   JIRA_JQL          such as: project = OPS AND issuetype = Incident AND created >= -30d
#   JIRA_START_FIELD  the field an incident starts at, such as
#                     customfield_10001 (default created); several,
#                     comma-separated, are tried in turn
#   JIRA_END_FIELD    the same for its end (default resolutiondate)
#   JIRA_LABELS       labels from fields (default service=components), such
#                     as service=components,team=customfield_10004; empty
#                     for none; 32 labels at most
#   MAX_PAGES         of 100 issues, at most (default 1000)
#
# A date is never taken for a time, alone or with something after it that
# isn't one (jira.jq); a later field's time counts only on its day. An issue
# whose start isn't a time is skipped; one whose end isn't is kept with no
# end; label values serve would refuse are left out. Each is said on stderr
# by its key, with a count of them by why. When every issue found is
# skipped, it writes nothing and exits 1.
set -eu
here=$(dirname "$0")
: "${JIRA_URL:?such as https://example.atlassian.net}"
: "${JIRA_EMAIL:?the email of the account}"
: "${JIRA_TOKEN_FILE:?a file holding its API token}"
: "${JIRA_JQL:?the search that finds your incidents}"
start=${JIRA_START_FIELD:-created}
end=${JIRA_END_FIELD:-resolutiondate}
labels=${JIRA_LABELS-service=components}
# The settings checked, and only the fields they name asked for.
fields=$(jq -nr --arg start "$start" --arg end "$end" --arg labels "$labels" -f "$here/jira.jq") || exit 1
auth=$(printf '%s:%s' "$JIRA_EMAIL" "$(cat "$JIRA_TOKEN_FILE")" | base64 | tr -d '\n')
jql=$(printf '%s' "$JIRA_JQL" | jq -sRr @uri)
all=$(mktemp)
out=$(mktemp)
trap 'rm -f "$all" "$out"' EXIT
trap 'exit 1' INT TERM HUP
max=${MAX_PAGES:-1000}
case $max in
  '' | *[!0-9]*)
    printf '%s: MAX_PAGES is a number of pages, not %s\n' "jira.sh" "$max" >&2
    exit 1
    ;;
esac
next=
pages=0
while :; do
  pages=$((pages + 1))
  if [ "$pages" -gt "$max" ]; then
    printf 'jira.sh: more than %s pages; stopping, writing nothing (raise MAX_PAGES, or narrow JIRA_JQL)\n' "$max" >&2
    exit 1
  fi
  url="$JIRA_URL/rest/api/3/search/jql?jql=$jql&maxResults=100&fields=$fields"
  [ -z "$next" ] || url="$url&nextPageToken=$(printf '%s' "$next" | jq -sRr @uri)"
  page=$(printf 'Authorization: Basic %s\n' "$auth" | curl -q -fsS --max-time 30 -H @- -H 'Accept: application/json' "$url")
  printf '%s' "$page" | jq -c '.issues[]' >>"$all"
  was=$next
  next=$(printf '%s' "$page" | jq -r 'if .isLast then empty else .nextPageToken // empty end')
  [ -n "$next" ] || break
  if [ "$next" = "$was" ]; then
    printf 'jira.sh: the API gave the same page again; stopping, writing nothing\n' >&2
    exit 1
  fi
done
jq -s --arg start "$start" --arg end "$end" --arg labels "$labels" -f "$here/jira.jq" "$all" >"$out"
# Keys come from Jira: shown as JSON, so nothing in one acts on a terminal.
# Titles aren't shown.
jq -r 'def by: [group_by(.why)[] | "\(.[0].why) (\(length))"] | join("; ");
  (.skipped[] | "jira.sh: \(.key | tojson) skipped: \(.why)"),
  (.noend[] | "jira.sh: \(.key | tojson) kept with no end, taken to last an hour: \(.why)"),
  (.cut[] | "jira.sh: \(.key | tojson) kept with label values left out: \(.why)"),
  (select(.skipped != []) | "jira.sh: skipped \(.skipped | length) of \(.found) issues: \(.skipped | by)"),
  (select(.noend != []) | "jira.sh: kept \(.noend | length) of \(.found) issues with no end: \(.noend | by)")' "$out" >&2
if [ "$(jq '.found > 0 and .incidents == []' "$out")" = true ]; then
  printf 'jira.sh: every issue found was skipped; writing nothing (see JIRA_START_FIELD, or narrow JIRA_JQL to issues whose start is filled in)\n' >&2
  exit 1
fi
jq '{incidents}' "$out"
