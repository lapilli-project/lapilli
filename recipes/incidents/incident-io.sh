#!/bin/sh
# Writes incident.io's incidents to stdout, as Lapilli's incidents file takes
# them. See incident-io.jq for the names to change for your organisation.
#
#   INCIDENT_IO_TOKEN_FILE  a file holding an API key that can view incidents
#   SINCE                   only those created on or after this date,
#                           like 2026-10-01
#   MAX_PAGES               of 100 incidents, at most (default 1000)
set -eu
here=$(dirname "$0")
api=${INCIDENT_IO_API:-https://api.incident.io}
: "${INCIDENT_IO_TOKEN_FILE:?a file holding an incident.io API key}"
token=$(cat "$INCIDENT_IO_TOKEN_FILE")
all=$(mktemp)
trap 'rm -f "$all"' EXIT
trap 'exit 1' INT TERM HUP
after=
pages=0
while :; do
  pages=$((pages + 1))
  if [ "$pages" -gt "${MAX_PAGES:-1000}" ]; then
    printf 'incident-io.sh: more than %s pages; stopping, writing nothing (raise MAX_PAGES, or set SINCE)\n' "${MAX_PAGES:-1000}" >&2
    exit 1
  fi
  url="$api/v2/incidents?page_size=100"
  [ -z "$after" ] || url="$url&after=$(printf '%s' "$after" | jq -sRr @uri)"
  [ -z "${SINCE:-}" ] || url="$url&created_at%5Bgte%5D=$(printf '%s' "$SINCE" | jq -sRr @uri)"
  page=$(printf 'Authorization: Bearer %s\n' "$token" | curl -fsS --max-time 30 -H @- "$url")
  printf '%s' "$page" | jq -c '.incidents[]' >>"$all"
  was=$after
  after=$(printf '%s' "$page" | jq -r '.pagination_meta.after // empty')
  [ -n "$after" ] || break
  if [ "$after" = "$was" ]; then
    printf 'incident-io.sh: the API gave the same page again; stopping, writing nothing\n' >&2
    exit 1
  fi
done
jq -s -f "$here/incident-io.jq" "$all"
