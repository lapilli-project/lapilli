#!/bin/sh
# Writes when Datadog monitors were triggered to stdout, from the events
# they wrote (GET /api/v2/events), as lapilli verdict --firings takes them:
# one firing a line.
#
#   SINCE, UNTIL          the period (ISO 8601; UNTIL defaults to now).
#                         Datadog keeps events 15 months. A day before
#                         SINCE is read too, to know which monitors were
#                         on already.
#   DATADOG_SITE          your site, such as datadoghq.eu or
#                         us3.datadoghq.com (default datadoghq.com): the
#                         API is https://api.SITE
#   DATADOG_API_KEY_FILE  a file holding an API key, and
#   DATADOG_APP_KEY_FILE  a file holding an application key; or instead
#   DATADOG_TOKEN_FILE    a file holding a personal or service access token
#   MAX_PAGES             of 1000 events, at most (default 1000)
#
# It needs events_read.
set -eu
here=$(dirname "$0")
say() { printf 'datadog.sh: %s\n' "$*" >&2; }
site=${DATADOG_SITE:-datadoghq.com}
c=$site
case $site in *[!a-z0-9.-]* | '') c=x ;; esac
if ! printf '%s' "$c" | grep -Eq '^[a-z0-9-]+(\.[a-z0-9-]+)+$'; then
  say "DATADOG_SITE $site is not a site such as datadoghq.com"
  exit 1
fi
api=${DATADOG_API:-https://api.$site}
# The keys go where api points: over https, or to this machine, for
# tests, and curl speaks no other scheme (proto). Neither is said: a user
# and key before @ would be in the log.
# The scheme may be in capitals, as curl takes it. This machine is
# 127.0.0.1, [::1] or localhost, with a port or not, and no name that only
# starts with one, such as localhost.example.com.
proto=
case $api in
  *@*)
    say "DATADOG_API holds a user or key before @, refused"
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
  say "DATADOG_API is not https"
  exit 1
fi
: "${SINCE:?the start of the period, such as 2026-11-01T00:00:00Z}"
for t in "$SINCE" "${UNTIL:-$SINCE}"; do
  # grep reads lines: a second line could carry anything past it.
  c=$t
  case $t in *[!0-9TZ:.+-]* | '') c=x ;; esac
  if ! printf '%s' "$c" | grep -Eq '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?(Z|[+-][0-9]{2}:[0-9]{2})$'; then
    say "$t is not a time such as 2026-11-01T00:00:00Z"
    exit 1
  fi
done
max=${MAX_PAGES:-1000}
case $max in
  '' | *[!0-9]*)
    say "MAX_PAGES is a number of pages, not $max"
    exit 1
    ;;
esac
# key NAME FILE: what FILE holds, one line of the characters a key is
# written in, as it goes in a header: a second line would be a header of
# its own. Neither the key nor what else the file holds is said.
key() {
  k=$(cat "$2")
  # Spaces, tabs and a carriage return after it, as an editor may leave
  # them, are dropped.
  while :; do
    case $k in
      *[[:space:]]) k=${k%?} ;;
      *) break ;;
    esac
  done
  case $k in
    '' | *[!A-Za-z0-9._~+/=-]*)
      say "$1 doesn't hold a key: one line of letters, digits and - . _ ~ + / ="
      exit 1
      ;;
  esac
  printf '%s' "$k"
}
# One key an assignment, so set -e sees each fail.
if [ -n "${DATADOG_TOKEN_FILE:-}" ]; then
  token=$(key DATADOG_TOKEN_FILE "$DATADOG_TOKEN_FILE")
  headers="Authorization: Bearer $token"
else
  : "${DATADOG_API_KEY_FILE:?a file holding an API key, or DATADOG_TOKEN_FILE}"
  : "${DATADOG_APP_KEY_FILE:?a file holding an application key, or DATADOG_TOKEN_FILE}"
  apikey=$(key DATADOG_API_KEY_FILE "$DATADOG_API_KEY_FILE")
  appkey=$(key DATADOG_APP_KEY_FILE "$DATADOG_APP_KEY_FILE")
  headers="DD-API-KEY: $apikey
DD-APPLICATION-KEY: $appkey"
fi
# ms TIME: milliseconds since 1970 of a time the check above let through.
ms() {
  jq -rn --arg t "$1" '$t
    | capture("^(?<d>.{19})(?<f>\\.[0-9]+)?(?<z>Z|[+-][0-9]{2}:[0-9]{2})$")
    | ((.d + "Z") | fromdateiso8601) + (("0" + (.f // "")) | tonumber)
      - (if .z == "Z" then 0 else (.z[0:1] + "1" | tonumber) * ((.z[1:3] | tonumber) * 3600 + (.z[4:6] | tonumber) * 60) end)
    | . * 1000 | floor'
}
since=$(ms "$SINCE")
now=$(jq -n 'now * 1000 | floor')
until=$now
[ -z "${UNTIL:-}" ] || until=$(ms "$UNTIL")
# Events from a day before the period, to know which monitors were on when
# it starts, and up to now, to know when those that started in it ended.
from=$((since - 86400000))
all=$(mktemp)
page=$(mktemp)
heads=$(mktemp)
trap 'rm -f "$all" "$page" "$heads"' EXIT
trap 'exit 1' INT TERM HUP
# header NAME: the whole seconds the last header NAME (lowercase) of the
# answer says, or nothing; its leading zeros off, as $((...)) would read
# 08 as octal, and fail.
header() {
  v=$(tr -d '\r' <"$heads" | awk -v h="$1:" 'tolower($1) == h { v = $2 } END { print v }')
  case $v in
    '' | *[!0-9]*) v= ;;
    *)
      v=${v#"${v%%[!0]*}"}
      v=${v:-0}
      ;;
  esac
  printf '%s' "$v"
}
# fetch URL: the Events API's answer in $page. Past its rate limit it
# answers 429: waited out as its Retry-After says, or else its
# X-RateLimit-Reset, the seconds until the limit resets; without either,
# 5 seconds, then twice as long each time. Two minutes at most a wait,
# five waits a request and ten minutes in all a run; past them, and on any
# answer but a 2xx, it stops. Called in this shell, not in $(...), which
# would forget the waits.
naps=0
waited=0
fetch() {
  waits=0
  backoff=5
  while :; do
    # The keys go on stdin, not the command line, where ps would show them.
    # An answer is 16 MiB at most (max-filesize): past it, curl fails, and
    # nothing is written.
    code=$(printf '%s\n' "$headers" |
      curl -q -sS --proto "$proto" --max-time 30 --max-filesize 16777216 -H @- -H 'Accept: application/json' -D "$heads" -o "$page" -w '%{http_code}' "$1") || exit 1
    case $code in
      2??) return 0 ;;
      429) ;;
      *)
        say "the Events API answered $code; writing nothing"
        exit 1
        ;;
    esac
    waits=$((waits + 1))
    wait=$(header retry-after)
    [ -n "$wait" ] || wait=$(header x-ratelimit-reset)
    if [ -z "$wait" ]; then
      wait=$backoff
      backoff=$((backoff * 2))
    fi
    [ "${#wait}" -le 3 ] && [ "$wait" -le 120 ] || wait=120
    if [ "$waits" -gt 5 ] || [ $((waited + wait)) -gt 600 ]; then
      say "the Events API still says too many requests after $naps waits, $waited seconds in all; stopping, writing nothing"
      exit 1
    fi
    sleep "$wait"
    naps=$((naps + 1))
    waited=$((waited + wait))
  done
}
cursor=
pages=0
while :; do
  pages=$((pages + 1))
  if [ "$pages" -gt "$max" ]; then
    say "more than $max pages; stopping, writing nothing (raise MAX_PAGES, or shorten the period)"
    exit 1
  fi
  # filter[query]=@monitor.id:*, the events a monitor wrote.
  url="$api/api/v2/events?filter%5Bquery%5D=%40monitor.id%3A%2A&filter%5Bfrom%5D=$from&filter%5Bto%5D=$now&sort=timestamp&page%5Blimit%5D=1000"
  [ -z "$cursor" ] || url="$url&page%5Bcursor%5D=$(printf '%s' "$cursor" | jq -sRr @uri)"
  fetch "$url"
  if ! jq -e 'type == "object" and (.data == null or (.data | type) == "array")' "$page" >/dev/null 2>&1; then
    say "the Events API answered something that isn't a list of events; writing nothing"
    exit 1
  fi
  # A page with warnings may hold part of the events only.
  if jq -e '(.meta.warnings // []) | length > 0' "$page" >/dev/null; then
    say "the Events API warns $(jq -c '[.meta.warnings[] | .title // .code // .]' "$page"); its events may be in part only, writing nothing"
    exit 1
  fi
  jq -c '(.data // [])[]' "$page" >>"$all"
  last=$cursor
  cursor=$(jq -r '.meta.page.after // empty' "$page")
  if [ -z "$cursor" ] || [ "$(jq '(.data // []) | length' "$page")" -eq 0 ]; then
    break
  fi
  if [ "$cursor" = "$last" ]; then
    say "the same page again; stopping, writing nothing"
    exit 1
  fi
done
jq -c -s --argjson since "$since" --argjson until "$until" -f "$here/datadog.jq" "$all"
