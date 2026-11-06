#!/bin/sh
# Writes PagerDuty's incidents to stdout, as Lapilli's incidents file takes
# them. Their services become the label service, for --match service.
#
#   PAGERDUTY_TOKEN_FILE  a file holding a read-only REST API key
#   SINCE, UNTIL          the period (ISO 8601; six months at most); PagerDuty's
#                         default, the last month, when not given
set -eu
here=$(dirname "$0")
api=${PAGERDUTY_API:-https://api.pagerduty.com}
: "${PAGERDUTY_TOKEN_FILE:?a file holding a PagerDuty API key}"
token=$(cat "$PAGERDUTY_TOKEN_FILE")
all=$(mktemp)
trap 'rm -f "$all"' EXIT
offset=0
while :; do
  url="$api/incidents?limit=100&offset=$offset&sort_by=created_at:asc"
  [ -z "${SINCE:-}" ] || url="$url&since=$SINCE"
  [ -z "${UNTIL:-}" ] || url="$url&until=$UNTIL"
  # The token goes on stdin, not the command line, where ps would show it.
  page=$(printf 'Authorization: Token token=%s\n' "$token" |
    curl -fsS --max-time 30 -H @- -H 'Accept: application/vnd.pagerduty+json;version=2' "$url")
  printf '%s' "$page" | jq -c '.incidents[]' >>"$all"
  [ "$(printf '%s' "$page" | jq -r '.more')" = true ] || break
  offset=$((offset + 100))
done
jq -s -f "$here/pagerduty.jq" "$all"
