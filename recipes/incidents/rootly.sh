#!/bin/sh
# Writes Rootly's incidents to stdout, as Lapilli's incidents file takes
# them. Their services become the label service, for --match service.
#
#   ROOTLY_TOKEN_FILE  a file holding an API key that can read incidents
#   SINCE              only incidents created on or after this: a date such
#                      as 2026-10-01, or an RFC 3339 time
#   MAX_PAGES          of 100 incidents, at most (default 1000)
set -eu
here=$(dirname "$0")
api=${ROOTLY_API:-https://api.rootly.com}
: "${ROOTLY_TOKEN_FILE:?a file holding a Rootly API key}"
# SINCE checked before it reaches a query.
since=
[ -z "${SINCE:-}" ] || since=$(printf '%s' "$SINCE" | jq -sRr '
  if test("\\A[0-9]{4}-[0-9]{2}-[0-9]{2}(T[0-9]{2}:[0-9]{2}:[0-9]{2}(\\.[0-9]+)?(Z|[+-][0-9]{2}:[0-9]{2}))?\\z") then .
  else error("SINCE") end') || {
  printf 'rootly.sh: SINCE is a date such as 2026-10-01, or an RFC 3339 time\n' >&2
  exit 1
}
token=$(cat "$ROOTLY_TOKEN_FILE")
all=$(mktemp)
trap 'rm -f "$all"' EXIT
trap 'exit 1' INT TERM HUP
max=${MAX_PAGES:-1000}
case $max in
  '' | *[!0-9]*)
    printf '%s: MAX_PAGES is a number of pages, not %s\n' "rootly.sh" "$max" >&2
    exit 1
    ;;
esac
number=1
pages=0
while :; do
  pages=$((pages + 1))
  if [ "$pages" -gt "$max" ]; then
    printf 'rootly.sh: more than %s pages; stopping, writing nothing (raise MAX_PAGES, or set SINCE later)\n' "$max" >&2
    exit 1
  fi
  url="$api/v1/incidents?page%5Bnumber%5D=$number&page%5Bsize%5D=100"
  [ -z "$since" ] || url="$url&filter%5Bcreated_at%5D%5Bgte%5D=$(printf '%s' "$since" | jq -sRr @uri)"
  page=$(printf 'Authorization: Bearer %s\n' "$token" | curl -fsS --max-time 30 -H @- -H 'Accept: application/vnd.api+json' "$url")
  printf '%s' "$page" | jq -c '.data[]' >>"$all"
  next=$(printf '%s' "$page" | jq -r '.meta.next_page // empty')
  [ -n "$next" ] || break
  case $next in
    *[!0-9]*)
      printf 'rootly.sh: the API gave a next page that is not a number; stopping, writing nothing\n' >&2
      exit 1
      ;;
  esac
  if [ "$next" -le "$number" ]; then
    printf 'rootly.sh: the API gave the same page again; stopping, writing nothing\n' >&2
    exit 1
  fi
  number=$next
done
jq -s -f "$here/rootly.jq" "$all"
