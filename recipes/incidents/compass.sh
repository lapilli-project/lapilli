#!/bin/sh
# Writes Compass's alerts to stdout, as Lapilli's incidents file takes them:
# for teams whose Opsgenie alerting moved to Compass. Compass keeps alerts,
# not incidents; Opsgenie's incidents move to Jira Service Management, which
# jira.sh reads. Which alerts count as incidents is in compass.jq.
#
#   COMPASS_CLOUD_ID    the site's cloud id (not the organisation's id)
#   COMPASS_EMAIL       the account's email
#   COMPASS_TOKEN_FILE  a file holding its API token
#   SINCE               only alerts created on or after this: a date such as
#                       2026-10-01, or an RFC 3339 time; all of them when not given
#   COMPASS_QUERY       optional, Compass's alert search, such as tag: incident
#   MAX_PAGES           of 100 alerts, at most (default 1000); Compass gives
#                       20,000 at most
set -eu
here=$(dirname "$0")
: "${COMPASS_CLOUD_ID:?the cloud id of the site}"
: "${COMPASS_EMAIL:?the email of the account}"
: "${COMPASS_TOKEN_FILE:?a file holding its API token}"
case $COMPASS_CLOUD_ID in
  '' | *[!0-9a-fA-F-]*)
    printf 'compass.sh: COMPASS_CLOUD_ID is a cloud id: hex digits and dashes\n' >&2
    exit 1
    ;;
esac
# SINCE checked before it reaches a query, and as seconds for Compass's from.
from=0
[ -z "${SINCE:-}" ] || from=$(printf '%s' "$SINCE" | jq -sRr '
  if test("\\A[0-9]{4}-[0-9]{2}-[0-9]{2}\\z") then . + "T00:00:00Z" else . end
  | capture("\\A(?<d>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(?:\\.[0-9]+)?(?<z>Z|[+-][0-9]{2}:[0-9]{2})\\z")
  // error("SINCE is a date such as 2026-10-01, or an RFC 3339 time")
  | (.d + "Z" | fromdateiso8601) - (if .z == "Z" then 0 else ((.z[1:3] | tonumber) * 3600 + (.z[4:6] | tonumber) * 60) * (if .z[0:1] == "-" then -1 else 1 end) end)') || {
  printf 'compass.sh: SINCE is a date such as 2026-10-01, or an RFC 3339 time\n' >&2
  exit 1
}
api=${COMPASS_API:-https://api.atlassian.com/compass/cloud/$COMPASS_CLOUD_ID/ops/v1}
auth=$(printf '%s:%s' "$COMPASS_EMAIL" "$(cat "$COMPASS_TOKEN_FILE")" | base64 | tr -d '\n')
all=$(mktemp)
trap 'rm -f "$all"' EXIT
trap 'exit 1' INT TERM HUP
max=${MAX_PAGES:-1000}
case $max in
  '' | *[!0-9]*)
    printf '%s: MAX_PAGES is a number of pages, not %s\n' "compass.sh" "$max" >&2
    exit 1
    ;;
esac
offset=0
pages=0
while :; do
  pages=$((pages + 1))
  if [ "$pages" -gt "$max" ]; then
    printf 'compass.sh: more than %s pages; stopping, writing nothing (raise MAX_PAGES, or set SINCE later)\n' "$max" >&2
    exit 1
  fi
  # Compass answers the first 20,000 alerts of a search and no more.
  if [ $((offset + 100)) -gt 20000 ]; then
    printf 'compass.sh: more than 20,000 alerts, as many as Compass gives; stopping, writing nothing (set SINCE later, or narrow COMPASS_QUERY)\n' >&2
    exit 1
  fi
  url="$api/alerts?size=100&offset=$offset&sort=createdAt&order=asc"
  [ "$from" = 0 ] || url="$url&from=$from"
  [ -z "${COMPASS_QUERY:-}" ] || url="$url&query=$(printf '%s' "$COMPASS_QUERY" | jq -sRr @uri)"
  page=$(printf 'Authorization: Basic %s\n' "$auth" | curl -fsS --max-time 30 -H @- -H 'Accept: application/json' "$url")
  printf '%s' "$page" | jq -c '.values[]' >>"$all"
  # The next page is the next offset: a link the API gives isn't followed.
  [ -n "$(printf '%s' "$page" | jq -r '.links.next // empty')" ] || break
  offset=$((offset + 100))
done
jq -s --arg from "$from" -f "$here/compass.jq" "$all"
