#!/bin/sh
# Writes the responses on stdin, as the other scripts here give them
# ({"responses": [...]}), to lapilli serve as a source's for a period: they
# take the place of what the ledger has from that source of pages sent in
# it. Sent again as they are, they change nothing, so this can run every
# hour over the last day, and every week over all the tool keeps.
#
#   put.sh URL SOURCE   URL is where --incidents-listen is, such as
#                       http://lapilli:8082, http or https, with no user
#                       before @; SOURCE names where the responses come from,
#                       such as pagerduty-prod (lowercase letters, digits
#                       and dashes)
#   SINCE, UNTIL        the period, as the script that wrote them was given it:
#                       give both scripts the same, UNTIL too
#   LAPILLI_TOKEN_FILE  a file holding the token in --responses-token-file,
#                       on one line
#
# The token goes where URL points. Over plain http it is read by whatever
# is on the way: keep that to the cluster serve runs in, as its port is.
#
# A period with no pages is one when none was sent: it takes away what the
# ledger had from the source then. In a pipe, a shell doesn't see the first
# command fail, so give it a file, written only when the script worked:
#   export SINCE=2026-11-08T00:00:00Z UNTIL=2026-11-09T00:00:00Z
#   ./pagerduty.sh >responses.json && ./put.sh http://lapilli:8082 pagerduty-prod <responses.json
set -eu
url=${1:?where --incidents-listen is, such as http://lapilli:8082}
source=${2:?the source, such as pagerduty-prod}
: "${SINCE:?the start of the period, as the script that wrote the responses was given it}"
: "${UNTIL:?the end of the period, as the script that wrote the responses was given it}"
: "${LAPILLI_TOKEN_FILE:?a file holding the token in --responses-token-file}"
say() { printf 'put.sh: %s\n' "$*" >&2; }
# The token goes where url points, serve's port: https, or http as a
# Service in the cluster is reached, and curl speaks no other scheme
# (proto). The scheme may be in capitals, as curl takes it. The address
# isn't said: a user and token before @ would be in the log.
proto=
case $url in
  *@*)
    say "the URL holds a user or token before @, refused"
    exit 1
    ;;
  [Hh][Tt][Tt][Pp][Ss]://?*) proto='=https' ;;
  [Hh][Tt][Tt][Pp]://?*) proto='=http' ;;
esac
if [ -z "$proto" ]; then
  say "the URL is not http or https: want where --incidents-listen is, such as http://lapilli:8082"
  exit 1
fi
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
# secret NAME FILE: what FILE holds, one line: serve takes a token of any
# characters, a space among them, and this one goes in a header as it is,
# where a second line, or a carriage return, would be a header of its own.
# No control character is taken, a tab among them. Neither the token nor
# what else the file holds is said.
secret() {
  s=$(cat "$2")
  # Spaces, tabs and a carriage return after it, as an editor may leave
  # them, are dropped, as serve drops them of its own file.
  while :; do
    case $s in
      *[[:space:]]) s=${s%?} ;;
      *) break ;;
    esac
  done
  case $s in
    '' | *[[:cntrl:]]*)
      say "$1 doesn't hold a token: one line, with no tab or other control character"
      exit 1
      ;;
  esac
  printf '%s' "$s"
}
token=$(secret LAPILLI_TOKEN_FILE "$LAPILLI_TOKEN_FILE")
input=$(mktemp)
answer=$(mktemp)
trap 'rm -f "$input" "$answer"' EXIT
trap 'exit 1' INT TERM HUP
cat >"$input"
# A list of responses, each with an id, alerts and the time it was sent,
# before anything is sent.
if ! jq -e '(.responses | type == "array") and (.responses | all(type == "object" and (.id | type == "string") and (.alerts | type == "array") and (.sent | type == "string")))' "$input" >/dev/null 2>&1; then
  say 'want JSON on stdin, with a list of responses in "responses", each with id, alerts and sent'
  exit 1
fi
n=$(jq '.responses | length' "$input")
[ "$n" -gt 0 ] || say "no responses: the period is written as one when no page was sent"
from=$(printf '%s' "$SINCE" | jq -sRr @uri)
to=$(printf '%s' "$UNTIL" | jq -sRr @uri)
# serve's answer is 16 MiB at most (max-filesize): past it, curl fails.
code=$(printf 'Authorization: Bearer %s\n' "$token" |
  curl -q -sS --proto "$proto" --max-time 120 --max-filesize 16777216 --retry 5 --retry-max-time 300 -X PUT -H @- -H 'Content-Type: application/json' \
    --data-binary "@$input" -o "$answer" -w '%{http_code}' "$url/api/v1/responses/$source?from=$from&to=$to") || code=failed
case $code in
  2??)
    # What serve says, as JSON and cut short, so nothing in it acts on a
    # terminal.
    say "$(printf '%s' "$source" | jq -Rsc .): $n responses written ($code): $({ jq -c . "$answer" 2>/dev/null || jq -Rsc . "$answer"; } | head -c 300)"
    ;;
  *)
    # What the server says, as JSON, so nothing in it acts on a terminal.
    say "the responses weren't written ($code): $({ jq -c '.error // .' "$answer" 2>/dev/null || jq -Rsc . "$answer"; } | head -c 300)"
    exit 1
    ;;
esac
