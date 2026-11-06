#!/bin/sh
# Writes the Jira issues a search finds to stdout, as Lapilli's incidents file
# takes them. Works for Jira Service Management's incidents, and for
# anything else kept in Jira Cloud. An issue's start is when it was created.
#
#   JIRA_URL         such as https://example.atlassian.net
#   JIRA_EMAIL       the account's email
#   JIRA_TOKEN_FILE  a file holding its API token
#   JIRA_JQL         such as: project = OPS AND issuetype = Incident AND created >= -30d
#   MAX_PAGES        of 100 issues, at most (default 1000)
set -eu
here=$(dirname "$0")
: "${JIRA_URL:?such as https://example.atlassian.net}"
: "${JIRA_EMAIL:?the email of the account}"
: "${JIRA_TOKEN_FILE:?a file holding its API token}"
: "${JIRA_JQL:?the search that finds your incidents}"
auth=$(printf '%s:%s' "$JIRA_EMAIL" "$(cat "$JIRA_TOKEN_FILE")" | base64 | tr -d '\n')
jql=$(printf '%s' "$JIRA_JQL" | jq -sRr @uri)
all=$(mktemp)
trap 'rm -f "$all"' EXIT
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
  url="$JIRA_URL/rest/api/3/search/jql?jql=$jql&maxResults=100&fields=summary,created,resolutiondate,components"
  [ -z "$next" ] || url="$url&nextPageToken=$(printf '%s' "$next" | jq -sRr @uri)"
  page=$(printf 'Authorization: Basic %s\n' "$auth" | curl -fsS --max-time 30 -H @- -H 'Accept: application/json' "$url")
  printf '%s' "$page" | jq -c '.issues[]' >>"$all"
  was=$next
  next=$(printf '%s' "$page" | jq -r 'if .isLast then empty else .nextPageToken // empty end')
  [ -n "$next" ] || break
  if [ "$next" = "$was" ]; then
    printf 'jira.sh: the API gave the same page again; stopping, writing nothing\n' >&2
    exit 1
  fi
done
jq -s -f "$here/jira.jq" "$all"
