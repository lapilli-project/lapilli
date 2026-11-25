#!/bin/sh
# Writes what Compass says of the pages of a period to stdout, as Lapilli's
# responses file takes it (docs/design/responses.md): each alert created in
# the period is a page, with the alert Alertmanager sent it for (its labels
# in its extra properties, as ../incidents/compass.sh reads them), when it
# was acknowledged, and when and by what it was closed. A person is never
# named. ops.jq says what each is decided from.
#
#   COMPASS_CLOUD_ID    the site's cloud id (not the organisation's id)
#   COMPASS_EMAIL       the account's email
#   COMPASS_TOKEN_FILE  a file holding its API token
#   SINCE, UNTIL        the period (RFC 3339 in whole seconds, such as
#                       2026-11-08T00:00:00Z), both needed: put.sh writes the
#                       period it was given
#   COMPASS_QUERY       optional, Compass's alert search, such as tag: prometheus
#   MAX_PAGES           of 100 alerts, at most (default 1000); Compass gives
#                       20,000 at most
#   MAX_LOGS            alerts whose logs are read, at most (default 500):
#                       the acknowledged ones and the closed ones
#
# An alert's log says who closed it, so it is read for each alert that is
# acknowledged or closed, five pages at most. Past MAX_LOGS the script stops,
# writing nothing: a page left without its log would be taken for one nobody
# answered.
set -eu
here=$(dirname "$0")
inc="$here/../incidents"
: "${COMPASS_CLOUD_ID:?the cloud id of the site}"
: "${COMPASS_EMAIL:?the email of the account}"
: "${COMPASS_TOKEN_FILE:?a file holding its API token}"
case $COMPASS_CLOUD_ID in
  '' | *[!0-9a-fA-F-]*)
    printf 'compass.sh: COMPASS_CLOUD_ID is a cloud id: hex digits and dashes\n' >&2
    exit 1
    ;;
esac
: "${SINCE:?the start of the period, such as 2026-11-08T00:00:00Z}"
: "${UNTIL:?the end of the period, such as 2026-11-09T00:00:00Z}"
# The period checked before it reaches a query, and in seconds for Compass's
# from and to, which are 32-bit.
from=$(printf '%s' "$SINCE" | jq -sRr -L "$here" 'include "responses"; epoch // error("SINCE") | floor') || {
  printf 'compass.sh: SINCE is an RFC 3339 time, such as 2026-11-08T00:00:00Z\n' >&2
  exit 1
}
to=$(printf '%s' "$UNTIL" | jq -sRr -L "$here" 'include "responses"; epoch // error("UNTIL") | floor') || {
  printf 'compass.sh: UNTIL is an RFC 3339 time, such as 2026-11-09T00:00:00Z\n' >&2
  exit 1
}
if [ "$from" -ge "$to" ] || [ "$from" -lt 0 ] || [ "$to" -gt 2147483647 ]; then
  printf 'compass.sh: SINCE is before UNTIL, and both are from 1970 to 2038\n' >&2
  exit 1
fi
api=${COMPASS_API:-https://api.atlassian.com/compass/cloud/$COMPASS_CLOUD_ID/ops/v1}
# The token goes where api points: over https, or to this machine, for
# tests, and curl speaks no other scheme (proto). Neither is said: a user
# and password before @ would be in the log.
# The scheme may be in capitals, as curl takes it. This machine is
# 127.0.0.1, [::1] or localhost, with a port or not, and no name that only
# starts with one, such as localhost.example.com.
# PLAIN_HTTP=true takes an address over plain http, to any host, for a
# self-managed tool reached inside a cluster: the token is read by
# whatever is on the way there. Off by default.
case ${PLAIN_HTTP:-false} in
  true | false) ;;
  *)
    printf '%s: PLAIN_HTTP is true or false\n' "compass.sh" >&2
    exit 1
    ;;
esac
proto=
case $api in
  *@*)
    printf 'compass.sh: COMPASS_API holds a user or token before @, refused\n' >&2
    exit 1
    ;;
  [Hh][Tt][Tt][Pp][Ss]://*) proto='=https' ;;
  [Hh][Tt][Tt][Pp]://*)
    case ${api#*://} in
      127.0.0.1 | '[::1]' | [Ll][Oo][Cc][Aa][Ll][Hh][Oo][Ss][Tt]) proto='=http' ;;
      127.0.0.1[/:]* | '[::1]'[/:]* | [Ll][Oo][Cc][Aa][Ll][Hh][Oo][Ss][Tt][/:]*) proto='=http' ;;
    esac
    [ "${PLAIN_HTTP:-false}" = false ] || proto='=http'
    ;;
esac
if [ -z "$proto" ]; then
  printf 'compass.sh: COMPASS_API is not https (PLAIN_HTTP=true takes plain http, to any host)\n' >&2
  exit 1
fi
# secret NAME FILE: what FILE holds, one line with no space. Atlassian
# says nothing of what its tokens are written in, and this one goes
# base64'd, with the email. Neither the token nor what else the file holds
# is said.
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
    '' | *[[:space:][:cntrl:]]*)
      printf '%s\n' "compass.sh: $1 doesn't hold a token: one line, with no space" >&2
      exit 1
      ;;
  esac
  printf '%s' "$s"
}
token=$(secret COMPASS_TOKEN_FILE "$COMPASS_TOKEN_FILE")
auth=$(printf '%s:%s' "$COMPASS_EMAIL" "$token" | base64 | tr -d '\n')
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
all=$(mktemp)
ids=$(mktemp)
logs=$(mktemp)
out=$(mktemp)
got=$(mktemp)
heads=$(mktemp)
failed=$(mktemp)
trap 'rm -f "$all" "$ids" "$logs" "$out" "$got" "$heads" "$failed"' EXIT
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
    printf 'compass.sh: an answer is more than 16 MiB; writing nothing\n' >&2
    return 1
  fi
  [ ! -s "$failed" ] || return 1
  i_code=$(sed -n 's/^HTTP\/[0-9.]* \([0-9][0-9][0-9]\).*/\1/p' "$heads" | tail -n 1)
  printf '%s' "${i_code:-000}"
}
# get URL: the answer in $got, its status in $code. A 429 is waited out as
# its Retry-After says, two minutes at most, five times a request and ten
# minutes in all a run; without one, 5 seconds, then twice as long each
# time. Called in this shell, not in $(...), which would forget the code.
naps=0
waited=0
get() {
  waits=0
  backoff=5
  while :; do
    code=$(printf 'Authorization: Basic %s\n' "$auth" |
      into "$got" -sS --proto "$proto" --max-time 30 --max-filesize 16777216 -H @- -H 'Accept: application/json' -D "$heads" "$1") || exit 1
    [ "$code" = 429 ] || return 0
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
    if [ "$waits" -gt 5 ] || [ $((waited + wait)) -gt 600 ]; then
      printf 'compass.sh: Compass still says too many requests after %s waits, %s seconds in all; stopping, writing nothing\n' "$naps" "$waited" >&2
      exit 1
    fi
    sleep "$wait"
    naps=$((naps + 1))
    waited=$((waited + wait))
  done
}
# ok WHAT: the answer in $got is a 2xx, or the script stops.
ok() {
  case $code in
    2??) ;;
    *)
      printf 'compass.sh: Compass answered %s %s; writing nothing\n' "$code" "$1" >&2
      exit 1
      ;;
  esac
}
offset=0
pages=0
while :; do
  pages=$((pages + 1))
  if [ "$pages" -gt "$max" ]; then
    printf 'compass.sh: more than %s pages; stopping, writing nothing (raise MAX_PAGES, or shorten the period)\n' "$max" >&2
    exit 1
  fi
  # Compass answers the first 20,000 alerts of a search and no more: a page
  # reaching the 20,000th is refused.
  if [ $((offset + 100)) -ge 20000 ]; then
    printf 'compass.sh: more than 20,000 alerts, as many as Compass gives; stopping, writing nothing (shorten the period, or narrow COMPASS_QUERY)\n' >&2
    exit 1
  fi
  url="$api/alerts?size=100&offset=$offset&sort=createdAt&order=asc&from=$from&to=$to"
  [ -z "${COMPASS_QUERY:-}" ] || url="$url&query=$(printf '%s' "$COMPASS_QUERY" | jq -sRr @uri)"
  get "$url"
  ok "for the alerts"
  if ! jq -e '(.values | type) == "array"' "$got" >/dev/null 2>&1; then
    printf 'compass.sh: Compass answered something that is not a list of alerts; writing nothing\n' >&2
    exit 1
  fi
  jq -c '.values[]' "$got" >>"$all"
  # The next page is the next offset: a link the API gives isn't followed.
  [ -n "$(jq -r '.links.next // empty' "$got")" ] || break
  offset=$((offset + 100))
done
jq -r -s -L "$here" -L "$inc" -f "$here/compass.jq" --argjson from "$from" --argjson to "$to" --argjson ids true "$all" >"$ids"
# The logs of the acknowledged and closed alerts: the next page is built
# from the address, the alert's id and the cursor, each checked, never
# followed as Compass gives it.
read=0
gone=0
while IFS= read -r id; do
  case $id in
    *[!0-9A-Za-z-]* | '')
      printf 'compass.sh: an alert id that is not letters, digits and dashes; stopping, writing nothing\n' >&2
      exit 1
      ;;
  esac
  read=$((read + 1))
  if [ "$read" -gt "$maxlogs" ]; then
    printf 'compass.sh: more than %s alerts to read the logs of; stopping, writing nothing (raise MAX_LOGS, or shorten the period)\n' "$maxlogs" >&2
    exit 1
  fi
  after=
  pages=0
  while [ "$pages" -lt 5 ]; do
    pages=$((pages + 1))
    url="$api/alerts/$id/logs?size=100"
    [ -z "$after" ] || url="$url&after=$after"
    get "$url"
    if [ "$code" = 404 ]; then
      # Deleted since it was listed.
      gone=$((gone + 1))
      break
    fi
    ok "for the log of an alert"
    if ! jq -e '(.values // []) | type == "array"' "$got" >/dev/null 2>&1; then
      printf 'compass.sh: Compass answered something that is not a log; writing nothing\n' >&2
      exit 1
    fi
    jq -c --arg id "$id" '{id: $id, logs: [.values[]?]}' "$got" >>"$logs"
    next=$(jq -r '.links.next // empty' "$got")
    [ -n "$next" ] || break
    after=$(printf '%s' "$next" | jq -Rr 'capture("[?&]after=(?<a>[0-9_]+)(&|$)").a // empty')
    if [ -z "$after" ]; then
      printf 'compass.sh: the log of alert %s gives a next page without a cursor; its first pages are taken\n' "$id" >&2
      break
    fi
  done
done <"$ids"
[ "$gone" -eq 0 ] || printf 'compass.sh: %s alerts were gone when their logs were asked for\n' "$gone" >&2
jq -s -L "$here" -L "$inc" -f "$here/compass.jq" --argjson from "$from" --argjson to "$to" --slurpfile logs "$logs" "$all" >"$out"
jq -r '.said | (select(.noalert > 0) | "compass.sh: left out \(.noalert) of the \(.alerts) alerts: their extra properties carry no alertname"),
  (select(.unread > 0) | "compass.sh: \(.unread) closed alerts whose log gives no close by a user or system: kept with no resolve")' "$out" >&2
jq '{responses}' "$out"
