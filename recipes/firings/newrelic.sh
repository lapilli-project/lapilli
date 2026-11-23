#!/bin/sh
# Writes when New Relic alert conditions had incidents open to stdout, from
# the NrAiIncident events NerdGraph's NRQL gives, as lapilli verdict
# --firings takes them: one firing a line.
#
#   SINCE, UNTIL            the period (ISO 8601; UNTIL defaults to now), as
#                           far back as the account keeps NrAiIncident
#   NEW_RELIC_ACCOUNT_ID    the account
#   NEW_RELIC_API_KEY_FILE  a file holding a user key that can query it
#   NEW_RELIC_REGION        where the account's data is: us (default), eu
#                           or jp
#   MAX_PAGES               of 1000 events, at most (default 1000)
#
# It needs to query the account's data with NRQL.
set -eu
here=$(dirname "$0")
say() { printf 'newrelic.sh: %s\n' "$*" >&2; }
case ${NEW_RELIC_REGION:-us} in
  us) api=https://api.newrelic.com/graphql ;;
  eu) api=https://api.eu.newrelic.com/graphql ;;
  jp) api=https://api.jp.newrelic.com/graphql ;;
  *)
    say "NEW_RELIC_REGION $NEW_RELIC_REGION is us, eu or jp"
    exit 1
    ;;
esac
api=${NEW_RELIC_API:-$api}
# The key goes where api points: over https, or to this machine, for
# tests, and curl speaks no other scheme (proto). Neither is said: a user
# and key before @ would be in the log.
# The scheme may be in capitals, as curl takes it. This machine is
# 127.0.0.1, [::1] or localhost, with a port or not, and no name that only
# starts with one, such as localhost.example.com.
# PLAIN_HTTP=true takes an address over plain http, to any host, for a
# self-managed tool reached inside a cluster: the token is read by
# whatever is on the way there. Off by default.
case ${PLAIN_HTTP:-false} in
  true | false) ;;
  *)
    printf '%s: PLAIN_HTTP is true or false\n' "newrelic.sh" >&2
    exit 1
    ;;
esac
proto=
case $api in
  *@*)
    say "NEW_RELIC_API holds a user or key before @, refused"
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
  say "NEW_RELIC_API is not https (PLAIN_HTTP=true takes plain http, to any host)"
  exit 1
fi
: "${NEW_RELIC_ACCOUNT_ID:?the account}"
: "${NEW_RELIC_API_KEY_FILE:?a file holding a user key}"
: "${SINCE:?the start of the period, such as 2026-11-01T00:00:00Z}"
# The account goes into the query, so only a number may.
case $NEW_RELIC_ACCOUNT_ID in
  '' | 0* | *[!0-9]*)
    say "NEW_RELIC_ACCOUNT_ID $NEW_RELIC_ACCOUNT_ID is not an account id"
    exit 1
    ;;
esac
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
# One line of the characters a key is written in, as it goes in a header:
# a second line would be a header of its own. Neither the key nor what
# else the file holds is said. Spaces, tabs and a carriage return after
# it, as an editor may leave them, are dropped.
key=$(cat "$NEW_RELIC_API_KEY_FILE")
while :; do
  case $key in
    *[[:space:]]) key=${key%?} ;;
    *) break ;;
  esac
done
case $key in
  '' | *[!A-Za-z0-9._~+/=-]*)
    say "NEW_RELIC_API_KEY_FILE doesn't hold a key: one line of letters, digits and - . _ ~ + / ="
    exit 1
    ;;
esac
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
# Events from an hour before the period, as an incident's event can be
# written a little after it opened, and up to now, to know when those
# that opened in it closed.
from=$((since - 3600000))
all=$(mktemp)
page=$(mktemp)
body=$(mktemp)
heads=$(mktemp)
failed=$(mktemp)
trap 'rm -f "$all" "$page" "$body" "$heads" "$failed"' EXIT
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
    printf 'newrelic.sh: an answer is more than 16 MiB; writing nothing\n' >&2
    return 1
  fi
  [ ! -s "$failed" ] || return 1
  i_code=$(sed -n 's/^HTTP\/[0-9.]* \([0-9][0-9][0-9]\).*/\1/p' "$heads" | tail -n 1)
  printf '%s' "${i_code:-000}"
}
# fetch: NerdGraph's answer to the query in $body, in $page. Past its
# limits (25 requests of a user's at once, NRQL's queries a minute) it
# answers 429, naming no wait: one is waited out as a Retry-After says,
# else 5 seconds, then twice as long each time. Two minutes at most a
# wait, five waits a request and ten minutes in all a run; past them, and
# on any answer but a 2xx, it stops. Called in this shell, not in $(...),
# which would forget the waits.
naps=0
waited=0
fetch() {
  waits=0
  backoff=5
  while :; do
    # The key goes on stdin, not the command line, where ps would show it.
    # An answer is 16 MiB at most (into): past it, the script stops, and
    # nothing is written.
    code=$(printf 'API-Key: %s\n' "$key" |
      into "$page" -sS --proto "$proto" --max-time 60 --max-filesize 16777216 -H @- -H 'Content-Type: application/json' --data-binary "@$body" \
        -D "$heads" "$api") || exit 1
    case $code in
      2??) return 0 ;;
      429) ;;
      *)
        say "NerdGraph answered $code; writing nothing"
        exit 1
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
    if [ "$waits" -gt 5 ] || [ $((waited + wait)) -gt 600 ]; then
      say "NerdGraph still says too many requests after $naps waits, $waited seconds in all; stopping, writing nothing"
      exit 1
    fi
    sleep "$wait"
    naps=$((naps + 1))
    waited=$((waited + wait))
  done
}
# NRQL has no cursor: each page is the next 1000 events by time, from the
# last one's millisecond, which is read again.
pages=0
while :; do
  pages=$((pages + 1))
  if [ "$pages" -gt "$max" ]; then
    say "more than $max pages; stopping, writing nothing (raise MAX_PAGES, or shorten the period)"
    exit 1
  fi
  nrql="SELECT * FROM NrAiIncident WHERE event IN ('open', 'close') SINCE $from UNTIL $now ORDER BY timestamp ASC LIMIT 1000"
  jq -nc --arg q "{ actor { account(id: $NEW_RELIC_ACCOUNT_ID) { nrql(query: \"$nrql\") { results } } } }" '{query: $q}' >"$body"
  fetch
  # NerdGraph can answer 200 and say what went wrong.
  if ! jq -e 'type == "object"' "$page" >/dev/null 2>&1; then
    say "NerdGraph answered something that isn't JSON; writing nothing"
    exit 1
  fi
  if jq -e '(.errors // []) | length > 0' "$page" >/dev/null; then
    say "NerdGraph says $(jq -c '[.errors[] | .message // .]' "$page"); writing nothing"
    exit 1
  fi
  if ! jq -e '.data.actor.account.nrql.results | type == "array"' "$page" >/dev/null; then
    say "NerdGraph answered no NRQL results; writing nothing"
    exit 1
  fi
  jq -c '.data.actor.account.nrql.results[]' "$page" >>"$all"
  [ "$(jq '.data.actor.account.nrql.results | length' "$page")" -ge 1000 ] || break
  next=$(jq -r '.data.actor.account.nrql.results[-1].timestamp | if type == "number" and . >= 1e12 then floor else "" end' "$page")
  if [ -z "$next" ]; then
    say "an event without a time in milliseconds; writing nothing"
    exit 1
  fi
  if [ "$next" -le "$from" ]; then
    say "more than 1000 events in one millisecond, or the same page again; stopping, writing nothing"
    exit 1
  fi
  from=$next
done
jq -c -s --argjson since "$since" --argjson until "$until" -f "$here/newrelic.jq" "$all"
