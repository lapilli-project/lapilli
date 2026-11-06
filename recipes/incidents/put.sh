#!/bin/sh
# Writes the incidents on stdin, as the other scripts here give them, to
# lapilli serve, one at a time, by their ids. One written again as it is
# changes nothing, so this can run every hour. An incident that is refused
# (one over 31 days, say) is reported and the rest go on; the exit status
# says whether any was.
#
#   put.sh URL    such as http://lapilli:8082, where --incidents-listen is
#   LAPILLI_TOKEN_FILE  a file holding the token in --incidents-token-file
set -eu
url=${1:?where --incidents-listen is, such as http://lapilli:8082}
: "${LAPILLI_TOKEN_FILE:?a file holding the token in --incidents-token-file}"
token=$(cat "$LAPILLI_TOKEN_FILE")
failed=$(mktemp)
trap 'rm -f "$failed"' EXIT
jq -c '.incidents[]' | while IFS= read -r one; do
  id=$(printf '%s' "$one" | jq -r '.id')
  path=$(printf '%s' "$id" | jq -sRr @uri)
  printf 'Authorization: Bearer %s\n' "$token" |
    curl -sS --fail-with-body --max-time 30 -X PUT -H @- -H 'Content-Type: application/json' \
      --data-binary "$one" -o /dev/null "$url/api/v1/incidents/$path" || {
    echo "put.sh: $id wasn't written" >&2
    echo x >>"$failed"
  }
done
[ ! -s "$failed" ]
