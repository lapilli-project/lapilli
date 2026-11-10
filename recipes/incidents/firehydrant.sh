#!/bin/sh
# Writes FireHydrant's incidents to stdout, as Lapilli's incidents file takes
# them. Their services become the label service, for --match service.
#
#   FIREHYDRANT_TOKEN_FILE  a file holding a bot token
#   SINCE                   only incidents that started on or after this: a
#                           date such as 2026-10-01, or an RFC 3339 time
#   MAX_PAGES               of 200 incidents, at most (default 1000)
set -eu
here=$(dirname "$0")
api=${FIREHYDRANT_API:-https://api.firehydrant.io}
: "${FIREHYDRANT_TOKEN_FILE:?a file holding a FireHydrant bot token}"
# SINCE checked before it reaches a query.
since=
[ -z "${SINCE:-}" ] || since=$(printf '%s' "$SINCE" | jq -sRr '
  if test("\\A[0-9]{4}-[0-9]{2}-[0-9]{2}(T[0-9]{2}:[0-9]{2}:[0-9]{2}(\\.[0-9]+)?(Z|[+-][0-9]{2}:[0-9]{2}))?\\z") then .
  else error("SINCE") end') || {
  printf 'firehydrant.sh: SINCE is a date such as 2026-10-01, or an RFC 3339 time\n' >&2
  exit 1
}
token=$(cat "$FIREHYDRANT_TOKEN_FILE")
all=$(mktemp)
trap 'rm -f "$all"' EXIT
trap 'exit 1' INT TERM HUP
max=${MAX_PAGES:-1000}
case $max in
  '' | *[!0-9]*)
    printf '%s: MAX_PAGES is a number of pages, not %s\n' "firehydrant.sh" "$max" >&2
    exit 1
    ;;
esac
number=1
pages=0
while :; do
  pages=$((pages + 1))
  if [ "$pages" -gt "$max" ]; then
    printf 'firehydrant.sh: more than %s pages; stopping, writing nothing (raise MAX_PAGES, or set SINCE later)\n' "$max" >&2
    exit 1
  fi
  url="$api/v1/incidents?page=$number&per_page=200"
  [ -z "$since" ] || url="$url&start_date=$(printf '%s' "$since" | jq -sRr @uri)"
  page=$(printf 'Authorization: Bearer %s\n' "$token" | curl -fsS --max-time 30 -H @- -H 'Accept: application/json' "$url")
  printf '%s' "$page" | jq -c '(.data // [])[]' >>"$all"
  next=$(printf '%s' "$page" | jq -r '.pagination.next // empty')
  [ -n "$next" ] || break
  case $next in
    *[!0-9]*)
      printf 'firehydrant.sh: the API gave a next page that is not a number; stopping, writing nothing\n' >&2
      exit 1
      ;;
  esac
  if [ "$next" -le "$number" ]; then
    printf 'firehydrant.sh: the API gave the same page again; stopping, writing nothing\n' >&2
    exit 1
  fi
  number=$next
done
jq -s -f "$here/firehydrant.jq" "$all"
