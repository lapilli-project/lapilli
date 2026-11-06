#!/bin/sh
# Writes the incidents on stdin, as the other scripts here give them, to
# lapilli serve, one at a time, by their ids. One written again as it is
# changes nothing, so this can run every hour. An incident that is refused
# (one over 31 days, say) is reported and the rest go on; the exit status
# says whether any was, or whether what came in wasn't a list of incidents.
# Told to slow down, it waits and tries again.
#
#   put.sh URL    such as http://lapilli:8082, where --incidents-listen is
#   LAPILLI_TOKEN_FILE  a file holding the token in --incidents-token-file
#
# In a pipe, a shell doesn't see the first command fail, so give it a file:
#   ./pagerduty.sh >incidents.json && ./put.sh http://lapilli:8082 <incidents.json
set -eu
url=${1:?where --incidents-listen is, such as http://lapilli:8082}
: "${LAPILLI_TOKEN_FILE:?a file holding the token in --incidents-token-file}"
token=$(cat "$LAPILLI_TOKEN_FILE")
input=$(mktemp)
failed=$(mktemp)
trap 'rm -f "$input" "$failed"' EXIT
trap 'exit 1' INT TERM HUP
say() { printf 'put.sh: %s\n' "$*" >&2; }
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
  code=$(printf 'Authorization: Bearer %s\n' "$token" |
    curl -sS --max-time 30 --retry 5 --retry-max-time 120 -X PUT -H @- -H 'Content-Type: application/json' \
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
