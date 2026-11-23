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
# The scheme may be in capitals, as curl takes it. This machine is
# 127.0.0.1, [::1] or localhost, with a port or not, and no name that only
# starts with one, such as localhost.example.com.
proto=
case $api in
  *@*)
    printf 'firehydrant.sh: FIREHYDRANT_API holds a user or token before @, refused\n' >&2
    exit 1
    ;;
  [Hh][Tt][Tt][Pp][Ss]://*) proto='=https' ;;
  [Hh][Tt][Tt][Pp]://*)
    case ${api#*://} in
      127.0.0.1 | '[::1]' | [Ll][Oo][Cc][Aa][Ll][Hh][Oo][Ss][Tt]) proto='=http' ;;
      127.0.0.1[/:]* | '[::1]'[/:]* | [Ll][Oo][Cc][Aa][Ll][Hh][Oo][Ss][Tt][/:]*) proto='=http' ;;
    esac
    ;;
esac
if [ -z "$proto" ]; then
  printf 'firehydrant.sh: FIREHYDRANT_API is not https\n' >&2
  exit 1
fi
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
  # Spaces, tabs and a carriage return after it, as an editor may leave
  # them, are dropped.
  while :; do
    case $s in
      *[[:space:]]) s=${s%?} ;;
      *) break ;;
    esac
  done
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
failed=$(mktemp)
trap 'rm -f "$all" "$ids" "$linked" "$out" "$got" "$heads" "$naps" "$failed"' EXIT
trap 'exit 1' INT TERM HUP
# into FILE CURL-ARGUMENTS: curl's answer into FILE, 16 MiB of it at most
# whatever curl's version (before 8.4.0, --max-filesize caps only an
# answer that says its length first), and its HTTP status, as the headers
# curl wrote to $heads (-D) say, on stdout. It fails when curl does, or
# past 16 MiB. curl's stdin is its own, and it reads no .curlrc (-q).
into() {
  i_file=$1
  shift
  : >"$failed"
  { curl -q "$@" -o - || echo x >"$failed"; } | head -c 16777217 >"$i_file"
  if [ "$(wc -c <"$i_file")" -gt 16777216 ]; then
    printf 'firehydrant.sh: an answer is more than 16 MiB; writing nothing\n' >&2
    return 1
  fi
  [ ! -s "$failed" ] || return 1
  i_code=$(sed -n 's/^HTTP\/[0-9.]* \([0-9][0-9][0-9]\).*/\1/p' "$heads" | tail -n 1)
  printf '%s' "${i_code:-000}"
}
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
      into "$got" -sS --proto "$proto" --max-time 30 --max-filesize 16777216 -H @- -H 'Accept: application/json' -D "$heads" "$1") || return 1
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
