#!/bin/sh
# Writes the incidents on stdin, as the other scripts here give them, to
# lapilli serve, one at a time, by their ids. One written again as it is
# changes nothing, so this can run every hour. An incident that is refused
# (one over 31 days, say) is reported and the rest go on; the exit status
# says whether any was, or whether what came in wasn't a list of incidents.
# Told to slow down, it waits and tries again.
#
#   put.sh URL    such as http://lapilli:8082, where --jobs-listen is:
#                 http or https, with no user before @
#   LAPILLI_TOKEN_FILE  a file holding the token in --incidents-token-file,
#                       on one line
#
# The token goes where URL points. Over plain http it is read by whatever
# is on the way: keep that to the cluster serve runs in, as its port is.
#
# In a pipe, a shell doesn't see the first command fail, so give it a file:
#   ./pagerduty.sh >incidents.json && ./put.sh http://lapilli:8082 <incidents.json
set -eu
url=${1:?where --jobs-listen is, such as http://lapilli:8082}
: "${LAPILLI_TOKEN_FILE:?a file holding the token in --incidents-token-file}"
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
  say "the URL is not http or https: want where --jobs-listen is, such as http://lapilli:8082"
  exit 1
fi
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
failed=$(mktemp)
trap 'rm -f "$input" "$failed"' EXIT
trap 'exit 1' INT TERM HUP
cat >"$input"
jq -e '.incidents | type == "array"' "$input" >/dev/null 2>&1 || {
  say 'want JSON on stdin, with a list of incidents in "incidents"'
  exit 1
}
jq -c '.incidents[]' "$input" | while IFS= read -r one; do
  id=$(printf '%s' "$one" | jq -r '.id // empty | strings')
  # Ids come from elsewhere: shown as JSON, so nothing in one acts on a terminal.
  shown=$(printf '%s' "$one" | jq -c '.id')
  if [ -z "$id" ]; then
    say "an incident has no id: $(printf '%s' "$one" | jq -c '{title, start}')"
    echo x >>"$failed"
    continue
  fi
  path=$(printf '%s' "$id" | jq -sRr @uri)
  # serve's answer isn't read, and is 16 MiB at most (max-filesize).
  code=$(printf 'Authorization: Bearer %s\n' "$token" |
    curl -q -sS --proto "$proto" --max-time 30 --max-filesize 16777216 --retry 5 --retry-max-time 120 -X PUT -H @- -H 'Content-Type: application/json' \
      --data-binary "$one" -o /dev/null -w '%{http_code}' "$url/api/v1/incidents/$path") || code=failed
  case $code in
    2??) ;;
    *)
      say "$shown wasn't written ($code)"
      echo x >>"$failed"
      ;;
  esac
done
[ ! -s "$failed" ]
