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
# The keys go where api points: over https, or to this machine, for tests.
# Neither is said: a user and key before @ would be in the log.
case $api in
  *@*)
    say "DATADOG_API holds a user or key before @, refused"
    exit 1
    ;;
  https://* | http://127.0.0.1:* | http://localhost:*) ;;
  *)
    say "DATADOG_API is not https"
    exit 1
    ;;
esac
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
# written in. Neither the key nor what else the file holds is said.
key() {
  k=$(cat "$2")
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
trap 'rm -f "$all" "$page"' EXIT
trap 'exit 1' INT TERM HUP
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
  # The keys go on stdin, not the command line, where ps would show them.
  printf '%s\n' "$headers" | curl -q -fsS --max-time 30 -H @- -H 'Accept: application/json' -o "$page" "$url"
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
