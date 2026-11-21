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
# The key goes where api points: over https, or to this machine, for tests.
case $api in
  *@*) api=x ;;
esac
case $api in
  https://* | http://127.0.0.1:* | http://localhost:*) ;;
  *)
    say "NEW_RELIC_API $NEW_RELIC_API is not https"
    exit 1
    ;;
esac
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
# One line of the characters a key is written in; neither the key nor what
# else the file holds is said.
key=$(cat "$NEW_RELIC_API_KEY_FILE")
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
trap 'rm -f "$all" "$page" "$body"' EXIT
trap 'exit 1' INT TERM HUP
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
  # The key goes on stdin, not the command line, where ps would show it.
  printf 'API-Key: %s\n' "$key" |
    curl -q -fsS --max-time 60 -H @- -H 'Content-Type: application/json' --data-binary "@$body" -o "$page" "$api"
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
