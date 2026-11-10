#!/bin/sh
# Writes the firings on stdin, as the other scripts here give them, to
# lapilli serve as a source's for a period: they take the place of the
# firings the ledger has from that source starting in it. Sent again as they
# are, they change nothing, so this can run every hour over the last day, and
# every week over all the cloud keeps.
#
#   put.sh URL SOURCE   URL is where --incidents-listen is, such as
#                       http://lapilli:8082; SOURCE names where the firings
#                       come from, such as cloudwatch-prod (lowercase letters,
#                       digits and dashes)
#   SINCE, UNTIL        the period, as the script that wrote them was given it:
#                       give both scripts the same, UNTIL too
#   LAPILLI_TOKEN_FILE  a file holding the token in --firings-token-file
#
# A period with no firings is one when nothing fired: it takes away what the
# ledger had from the source then. In a pipe, a shell doesn't see the first
# command fail, so give it a file, written only when the script worked:
#   export SINCE=2026-11-08T00:00:00Z UNTIL=2026-11-09T00:00:00Z
#   ./cloudwatch.sh >firings.jsonl && ./put.sh http://lapilli:8082 cloudwatch-prod <firings.jsonl
set -eu
url=${1:?where --incidents-listen is, such as http://lapilli:8082}
source=${2:?the source, such as cloudwatch-prod}
: "${SINCE:?the start of the period, as the script that wrote the firings was given it}"
: "${UNTIL:?the end of the period, as the script that wrote the firings was given it}"
: "${LAPILLI_TOKEN_FILE:?a file holding the token in --firings-token-file}"
say() { printf 'put.sh: %s\n' "$*" >&2; }
for t in "$SINCE" "$UNTIL"; do
  # grep reads lines: a second line could carry anything past it.
  c=$t
  case $t in *[!0-9TZ:.+-]* | '') c=x ;; esac
  # Whole seconds: the ledger keeps seconds, and periods that meet share none.
  if ! printf '%s' "$c" | grep -Eq '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(Z|[+-][0-9]{2}:[0-9]{2})$'; then
    say "$t is not a time in whole seconds, such as 2026-11-01T00:00:00Z"
    exit 1
  fi
done
case $source in
  *[!a-z0-9-]* | '' | -* | *-) say "$source: a source is lowercase letters, digits and dashes"; exit 1 ;;
esac
token=$(cat "$LAPILLI_TOKEN_FILE")
input=$(mktemp)
answer=$(mktemp)
trap 'rm -f "$input" "$answer"' EXIT
trap 'exit 1' INT TERM HUP
cat >"$input"
# Each line a firing, before anything is sent.
if ! jq -e -s 'all(type == "object" and (.rule | type == "string") and (.start | type == "string"))' "$input" >/dev/null 2>&1; then
  say 'want firings on stdin, one JSON object a line, each with rule and start'
  exit 1
fi
n=$(jq -s length "$input")
[ "$n" -gt 0 ] || say "no firings: the period is written as one when nothing fired"
from=$(printf '%s' "$SINCE" | jq -sRr @uri)
to=$(printf '%s' "$UNTIL" | jq -sRr @uri)
code=$(printf 'Authorization: Bearer %s\n' "$token" |
  curl -sS --max-time 120 --retry 5 --retry-max-time 300 -X PUT -H @- -H 'Content-Type: application/x-ndjson' \
    --data-binary "@$input" -o "$answer" -w '%{http_code}' "$url/api/v1/firings/$source?from=$from&to=$to") || code=failed
case $code in
  2??)
    # Numbers, and the source as JSON: nothing the server says acts on a
    # terminal.
    jq -r '"put.sh: \(.source | tojson): \(.firings | tonumber) firings, \(.added | tonumber) added, \(.removed | tonumber) taken away"' "$answer" >&2
    ;;
  *)
    # What the server says, as JSON, so nothing in it acts on a terminal.
    say "the firings weren't written ($code): $(jq -c '.error // .' "$answer" 2>/dev/null || jq -Rsc . "$answer")"
    exit 1
    ;;
esac
