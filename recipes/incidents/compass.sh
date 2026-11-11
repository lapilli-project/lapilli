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
#   MAX_LOGS            closed alerts whose logs are read for when they
#                       closed, at most (default 500); the rest end at their
#                       last update
#
# Compass's alerts don't say when they closed; their logs do. For each closed
# alert that counts (compass.jq), its log is read, five pages at most, and
# the latest entry saying it was closed (compass-closed.jq) is its end.
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
closes=$(mktemp)
trap 'rm -f "$all" "$closes" "$closes.json"' EXIT
trap 'exit 1' INT TERM HUP
max=${MAX_PAGES:-1000}
case $max in
  '' | *[!0-9]*)
    printf '%s: MAX_PAGES is a number of pages, not %s\n' "compass.sh" "$max" >&2
    exit 1
    ;;
esac
maxlogs=${MAX_LOGS:-500}
case $maxlogs in
  '' | *[!0-9]*)
    printf 'compass.sh: MAX_LOGS is a number of alerts, not %s\n' "$maxlogs" >&2
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
  # Compass answers the first 20,000 alerts of a search and no more: a page
  # reaching the 20,000th is refused.
  if [ $((offset + 100)) -ge 20000 ]; then
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
# The closed alerts that count, then each one's log: the next page is built
# from the address, the alert's id and the cursor, each checked, never
# followed as Compass gives it.
printf 'null' >"$closes.json"
read=0
over=0
for id in $(jq -s -r --arg from "$from" --slurpfile closes "$closes.json" -f "$here/compass.jq" "$all"); do
  case $id in
    *[!0-9A-Za-z-]* | '')
      printf 'compass.sh: an alert id %s that isn'"'"'t one; stopping, writing nothing\n' "$id" >&2
      exit 1
      ;;
  esac
  if [ "$read" -ge "$maxlogs" ]; then
    over=$((over + 1))
    continue
  fi
  read=$((read + 1))
  after=
  closed=
  pages=0
  while [ "$pages" -lt 5 ]; do
    pages=$((pages + 1))
    url="$api/alerts/$id/logs?size=100"
    [ -z "$after" ] || url="$url&after=$after"
    page=$(printf 'Authorization: Basic %s\n' "$auth" | curl -fsS --max-time 30 -H @- -H 'Accept: application/json' "$url")
    closed=$(printf '%s' "$page" | jq -r --arg since "$closed" -f "$here/compass-closed.jq")
    next=$(printf '%s' "$page" | jq -r '.links.next // empty')
    [ -n "$next" ] || break
    after=$(printf '%s' "$next" | jq -Rr 'capture("[?&]after=(?<a>[0-9_]+)(&|$)").a // empty')
    if [ -z "$after" ]; then
      printf 'compass.sh: the log of alert %s gives a next page without a cursor; its first pages are taken\n' "$id" >&2
      break
    fi
  done
  [ -z "$closed" ] || printf '%s\t%s\n' "$id" "$closed" >>"$closes"
done
[ "$over" -eq 0 ] || printf 'compass.sh: %s closed alerts past MAX_LOGS (%s) end at their last update\n' "$over" "$maxlogs" >&2
jq -R -s 'split("\n") | map(select(. != "") | split("\t") | {key: .[0], value: (.[1] | tonumber)}) | from_entries' "$closes" >"$closes.json"
jq -s --arg from "$from" --slurpfile closes "$closes.json" -f "$here/compass.jq" "$all"
