#!/bin/sh
# Writes the incidents on stdin, as the other scripts here give them, to
# lapilli serve, one at a time, by their ids. One written again as it is
# changes nothing, so this can run every hour. An incident that is refused
# (one over 31 days, say) is reported and the rest go on; the exit status
# says whether any was, or whether what came in wasn't a list of incidents.
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
cat >"$input"
jq -e '.incidents | type == "array"' "$input" >/dev/null 2>&1 || {
  echo 'put.sh: want JSON on stdin, with a list of incidents in "incidents"' >&2
  exit 1
}
jq -c '.incidents[]' "$input" | while IFS= read -r one; do
  id=$(printf '%s' "$one" | jq -r '.id // empty')
  if [ -z "$id" ]; then
    echo "put.sh: an incident has no id: $one" >&2
    echo x >>"$failed"
    continue
  fi
  path=$(printf '%s' "$id" | jq -sRr @uri)
  printf 'Authorization: Bearer %s\n' "$token" |
    curl -fsS --max-time 30 -X PUT -H @- -H 'Content-Type: application/json' \
      --data-binary "$one" -o /dev/null "$url/api/v1/incidents/$path" || {
    echo "put.sh: $id wasn't written" >&2
    echo x >>"$failed"
  }
done
[ ! -s "$failed" ]
