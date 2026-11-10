#!/bin/sh
# Writes Grafana IRM's incidents to stdout, as Lapilli's incidents file takes
# them. See grafana-irm.jq for the names to change for your organisation.
#
#   GRAFANA_URL             your Grafana stack, such as https://example.grafana.net
#   GRAFANA_IRM_TOKEN_FILE  a file holding a service account token that can
#                           read incidents
#   SINCE                   only incidents created on or after this: a date
#                           such as 2026-10-01, or an RFC 3339 time
#   MAX_PAGES               of 100 incidents, at most (default 1000)
set -eu
here=$(dirname "$0")
: "${GRAFANA_URL:?your Grafana stack, such as https://example.grafana.net}"
: "${GRAFANA_IRM_TOKEN_FILE:?a file holding a Grafana service account token}"
# SINCE checked before it reaches a query, and written as RFC 3339.
since=
[ -z "${SINCE:-}" ] || since=$(printf '%s' "$SINCE" | jq -sRr '
  if test("\\A[0-9]{4}-[0-9]{2}-[0-9]{2}\\z") then . + "T00:00:00Z"
  elif test("\\A[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\\.[0-9]+)?(Z|[+-][0-9]{2}:[0-9]{2})\\z") then .
  else error("SINCE") end') || {
  printf 'grafana-irm.sh: SINCE is a date such as 2026-10-01, or an RFC 3339 time\n' >&2
  exit 1
}
url="${GRAFANA_URL%/}/api/plugins/grafana-irm-app/resources/api/v1/IncidentsService.QueryIncidents"
token=$(cat "$GRAFANA_IRM_TOKEN_FILE")
all=$(mktemp)
trap 'rm -f "$all"' EXIT
trap 'exit 1' INT TERM HUP
max=${MAX_PAGES:-1000}
case $max in
  '' | *[!0-9]*)
    printf '%s: MAX_PAGES is a number of pages, not %s\n' "grafana-irm.sh" "$max" >&2
    exit 1
    ;;
esac
next=
pages=0
while :; do
  pages=$((pages + 1))
  if [ "$pages" -gt "$max" ]; then
    printf 'grafana-irm.sh: more than %s pages; stopping, writing nothing (raise MAX_PAGES, or set SINCE later)\n' "$max" >&2
    exit 1
  fi
  body=$(jq -nc --arg from "$since" --arg next "$next" \
    '{query: ({limit: 100, orderDirection: "ASC"} + (if $from == "" then {} else {dateFrom: $from} end))}
      + (if $next == "" then {} else {cursor: {nextValue: $next, hasMore: true}} end)')
  page=$(printf 'Authorization: Bearer %s\n' "$token" |
    curl -fsS --max-time 30 -H @- -H 'Content-Type: application/json; charset=utf-8' --data-binary "$body" "$url")
  # The API can answer 200 and say what went wrong.
  err=$(printf '%s' "$page" | jq -r '.error // empty')
  if [ -n "$err" ]; then
    printf 'grafana-irm.sh: Grafana IRM says %s; writing nothing\n' "$(printf '%s' "$err" | jq -Rsc .)" >&2
    exit 1
  fi
  printf '%s' "$page" | jq -c '(.incidents // [])[]' >>"$all"
  was=$next
  next=$(printf '%s' "$page" | jq -r 'if .cursor.hasMore then .cursor.nextValue // empty else empty end')
  [ -n "$next" ] || break
  if [ "$next" = "$was" ]; then
    printf 'grafana-irm.sh: the API gave the same page again; stopping, writing nothing\n' >&2
    exit 1
  fi
done
jq -s -f "$here/grafana-irm.jq" "$all"
