#!/bin/sh
# Writes the Jira issues a search finds to stdout, as Lapilli's incidents file
# takes them. Works for Jira Service Management's incidents, and for
# anything else kept in Jira Cloud. An issue's start is when it was created.
#
#   JIRA_URL         such as https://example.atlassian.net
#   JIRA_EMAIL       the account's email
#   JIRA_TOKEN_FILE  a file holding its API token
#   JIRA_JQL         such as: project = OPS AND issuetype = Incident AND created >= -30d
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
next=
while :; do
  url="$JIRA_URL/rest/api/3/search/jql?jql=$jql&maxResults=100&fields=summary,created,resolutiondate,components"
  [ -z "$next" ] || url="$url&nextPageToken=$(printf '%s' "$next" | jq -sRr @uri)"
  page=$(printf 'Authorization: Basic %s\n' "$auth" | curl -fsS --max-time 30 -H @- -H 'Accept: application/json' "$url")
  printf '%s' "$page" | jq -c '.issues[]' >>"$all"
  next=$(printf '%s' "$page" | jq -r 'if .isLast then empty else .nextPageToken // empty end')
  [ -n "$next" ] || break
done
jq -s -f "$here/jira.jq" "$all"
