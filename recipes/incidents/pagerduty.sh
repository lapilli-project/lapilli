#!/bin/sh
# Writes PagerDuty's incidents to stdout, as Lapilli's incidents file takes
# them. Their services become the label service, for --match service.
#
#   PAGERDUTY_TOKEN_FILE  a file holding a read-only REST API key
#   SINCE, UNTIL          the period (ISO 8601; six months at most); PagerDuty's
#                         default, the last month, when not given
#   MAX_PAGES             of 100 incidents, at most (default 1000)
set -eu
here=$(dirname "$0")
api=${PAGERDUTY_API:-https://api.pagerduty.com}
: "${PAGERDUTY_TOKEN_FILE:?a file holding a PagerDuty API key}"
token=$(cat "$PAGERDUTY_TOKEN_FILE")
all=$(mktemp)
trap 'rm -f "$all"' EXIT
trap 'exit 1' INT TERM HUP
max=${MAX_PAGES:-1000}
case $max in
  '' | *[!0-9]*)
    printf '%s: MAX_PAGES is a number of pages, not %s\n' "pagerduty.sh" "$max" >&2
    exit 1
    ;;
esac
offset=0
pages=0
while :; do
  pages=$((pages + 1))
  if [ "$pages" -gt "$max" ]; then
    printf 'pagerduty.sh: more than %s pages; stopping, writing nothing (raise MAX_PAGES, or shorten the period)\n' "$max" >&2
    exit 1
  fi
  url="$api/incidents?limit=100&offset=$offset&sort_by=created_at:asc"
  [ -z "${SINCE:-}" ] || url="$url&since=$(printf '%s' "$SINCE" | jq -sRr @uri)"
  [ -z "${UNTIL:-}" ] || url="$url&until=$(printf '%s' "$UNTIL" | jq -sRr @uri)"
  # The token goes on stdin, not the command line, where ps would show it.
  page=$(printf 'Authorization: Token token=%s\n' "$token" |
    curl -fsS --max-time 30 -H @- -H 'Accept: application/vnd.pagerduty+json;version=2' "$url")
  printf '%s' "$page" | jq -c '.incidents[]' >>"$all"
  [ "$(printf '%s' "$page" | jq -r '.more')" = true ] || break
  offset=$((offset + 100))
done
jq -s -f "$here/pagerduty.jq" "$all"
