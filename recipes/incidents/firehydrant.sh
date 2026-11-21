#!/bin/sh
# Writes FireHydrant's incidents to stdout, as Lapilli's incidents file takes
# them. Their services become the label service, for --match service, and
# the alerts attached to each whose labels say alertname, its alerts
# (docs/design/links.md).
#
#   FIREHYDRANT_TOKEN_FILE  a file holding a bot token
#   SINCE                   only incidents that started on or after this: a
#                           date such as 2026-10-01, or an RFC 3339 time
#   MAX_PAGES               of 200 incidents, at most (default 1000)
#   ALERTS                  yes (default) to read each incident's alerts, or no
#   ALERT_LABELS            the labels an alert keeps, comma-separated, such
#                           as service,namespace (default all its labels give)
#   MAX_ALERT_REQUESTS      for alerts, at most (default 2000)
set -eu
here=$(dirname "$0")
api=${FIREHYDRANT_API:-https://api.firehydrant.io}
# The token goes where api points: over https, or to this machine, for
# tests, and curl speaks no other scheme (proto). Neither is said: a user
# and password before @ would be in the log.
case $api in
  *@*)
    printf 'firehydrant.sh: FIREHYDRANT_API holds a user or token before @, refused\n' >&2
    exit 1
    ;;
  https://*) proto='=https' ;;
  http://127.0.0.1:* | http://localhost:*) proto='=http' ;;
  *)
    printf 'firehydrant.sh: FIREHYDRANT_API is not https\n' >&2
    exit 1
    ;;
esac
: "${FIREHYDRANT_TOKEN_FILE:?a file holding a FireHydrant bot token}"
# SINCE checked before it reaches a query.
since=
[ -z "${SINCE:-}" ] || since=$(printf '%s' "$SINCE" | jq -sRr '
  if test("\\A[0-9]{4}-[0-9]{2}-[0-9]{2}(T[0-9]{2}:[0-9]{2}:[0-9]{2}(\\.[0-9]+)?(Z|[+-][0-9]{2}:[0-9]{2}))?\\z") then .
  else error("SINCE") end') || {
  printf 'firehydrant.sh: SINCE is a date such as 2026-10-01, or an RFC 3339 time\n' >&2
  exit 1
}
# secret NAME FILE: what FILE holds, one line of the characters a token
# is written in (a Bearer token's, RFC 6750), as it goes in a header.
# Neither the token nor what else the file holds is said.
secret() {
  s=$(cat "$2")
  case $s in
    '' | *[!A-Za-z0-9._~+/=-]*)
      printf '%s\n' "firehydrant.sh: $1 doesn't hold a token: one line of letters, digits and - . _ ~ + / =" >&2
      exit 1
      ;;
  esac
  printf '%s' "$s"
}
token=$(secret FIREHYDRANT_TOKEN_FILE "$FIREHYDRANT_TOKEN_FILE")
all=$(mktemp)
ids=$(mktemp)
linked=$(mktemp)
out=$(mktemp)
got=$(mktemp)
heads=$(mktemp)
naps=$(mktemp)
trap 'rm -f "$all" "$ids" "$linked" "$out" "$got" "$heads" "$naps"' EXIT
trap 'exit 1' INT TERM HUP
# get URL: a page, on stdout. FireHydrant takes 50 requests an account
# every 10 seconds: a 429 is waited out as its Retry-After says, or else 5
# seconds, then twice as long each time; two minutes at most a wait, five
# waits a request and ten minutes in all a run. Past them, and on any
# answer but a 2xx, or one over 16 MiB, it fails. Its waits are counted in
# a file, as each $(get ...) is a shell of its own.
get() {
  waits=0
  backoff=5
  while :; do
    code=$(printf 'Authorization: Bearer %s\n' "$token" |
      curl -q -sS --proto "$proto" --max-time 30 --max-filesize 16777216 -H @- -H 'Accept: application/json' -D "$heads" -o "$got" -w '%{http_code}' "$1") || return 1
    case $code in
      2??)
        cat "$got"
        return 0
        ;;
      429) ;;
      *)
        printf 'firehydrant.sh: FireHydrant answered %s; writing nothing\n' "$code" >&2
        return 1
        ;;
    esac
    waits=$((waits + 1))
    wait=$(tr -d '\r' <"$heads" | awk 'tolower($1) == "retry-after:" { v = $2 } END { print v }')
    case $wait in
      '' | *[!0-9]*)
        wait=$backoff
        backoff=$((backoff * 2))
        ;;
    esac
    # Leading zeros off: $((...)) would read 08 as octal, and fail.
    wait=${wait#"${wait%%[!0]*}"}
    wait=${wait:-0}
    [ "${#wait}" -le 3 ] && [ "$wait" -le 120 ] || wait=120
    waited=$(awk '{ s += $1 } END { print s + 0 }' "$naps")
    if [ "$waits" -gt 5 ] || [ $((waited + wait)) -gt 600 ]; then
      printf 'firehydrant.sh: FireHydrant still says too many requests after %s waits, %s seconds in all; stopping, writing nothing\n' "$(wc -l <"$naps" | tr -d ' ')" "$waited" >&2
      return 1
    fi
    sleep "$wait"
    printf '%s\n' "$wait" >>"$naps"
  done
}
max=${MAX_PAGES:-1000}
case $max in
  '' | *[!0-9]*)
    printf '%s: MAX_PAGES is a number of pages, not %s\n' "firehydrant.sh" "$max" >&2
    exit 1
    ;;
esac
alerts=${ALERTS:-yes}
maxreq=${MAX_ALERT_REQUESTS:-2000}
case $alerts in
  yes | no) ;;
  *)
    printf 'firehydrant.sh: ALERTS is yes or no\n' >&2
    exit 1
    ;;
esac
case $maxreq in
  '' | *[!0-9]*)
    printf 'firehydrant.sh: MAX_ALERT_REQUESTS is a number of requests\n' >&2
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
  page=$(get "$url")
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
if [ "$alerts" = yes ]; then
  jq -r -s -L "$here" -f "$here/firehydrant.jq" --argjson ids true "$all" >"$ids"
  requests=0
  while IFS= read -r id; do
    # An id goes in a path: FireHydrant's are letters, digits and dashes.
    case $id in
      '' | *[!A-Za-z0-9-]*)
        printf 'firehydrant.sh: an incident id that is not letters, digits and dashes; stopping, writing nothing\n' >&2
        exit 1
        ;;
    esac
    number=1
    while :; do
      requests=$((requests + 1))
      if [ "$requests" -gt "$maxreq" ]; then
        printf 'firehydrant.sh: more than %s requests for alerts; stopping, writing nothing (raise MAX_ALERT_REQUESTS, set SINCE later, or set ALERTS=no)\n' "$maxreq" >&2
        exit 1
      fi
      # Its reference documents no page for an incident's alerts, but
      # answers with pages: a next one is asked for as the incidents' are.
      url="$api/v1/incidents/$id/alerts"
      [ "$number" = 1 ] || url="$url?page=$number"
      page=$(get "$url")
      if ! printf '%s' "$page" | jq -e 'type == "object" and ((.data // []) | type == "array")' >/dev/null 2>&1; then
        printf 'firehydrant.sh: the API answered something that is not a list of an incident'"'"'s alerts; writing nothing\n' >&2
        exit 1
      fi
      printf '%s' "$page" | jq -c --arg id "$id" '{id: $id, alerts: [(.data // [])[] | .alert | objects]}' >>"$linked"
      next=$(printf '%s' "$page" | jq -r '.pagination.next // empty')
      [ -n "$next" ] || break
      case $next in
        *[!0-9]*)
          printf 'firehydrant.sh: the API gave a next page of alerts that is not a number; stopping, writing nothing\n' >&2
          exit 1
          ;;
      esac
      if [ "$next" -le "$number" ]; then
        printf 'firehydrant.sh: the API gave the same page of alerts again; stopping, writing nothing\n' >&2
        exit 1
      fi
      number=$next
    done
  done <"$ids"
fi
jq -s -L "$here" -f "$here/firehydrant.jq" --slurpfile linked "$linked" --arg alert_labels "${ALERT_LABELS:-}" "$all" >"$out"
jq -r -L "$here" 'include "alerts"; said("firehydrant.sh"; "their labels carry no alertname")' "$out" >&2
jq '{incidents}' "$out"
