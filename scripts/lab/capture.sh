#!/bin/bash
# capture.sh NAME PORT CLIENT_CONFIG_LINES... -- [extra ssh args]
# Writes logs/NAME.log (ssh -vvv stderr) and logs/NAME.exit.
set -uo pipefail
H="$(cd "$(dirname "$0")" && pwd)"
name="$1"; port="$2"; shift 2
cfg="$H/run/client-$name.conf"
: > "$cfg"
while [ $# -gt 0 ] && [ "$1" != "--" ]; do echo "$1" >> "$cfg"; shift; done
[ "${1:-}" = "--" ] && shift
dest="${DEST:-$USER@127.0.0.1}"
# alarm survives exec: a successful -N session is killed after 6s
perl -e 'alarm shift; exec @ARGV' 6 \
  ssh -vvv -F "$cfg" -o BatchMode=yes -o ConnectTimeout=4 -N -p "$port" "$@" "$dest" \
  2> "$H/logs/$name.log" < /dev/null
echo $? > "$H/logs/$name.exit"
printf '%-22s exit=%s  %s\n' "$name" "$(cat "$H/logs/$name.exit")" "$(grep -E 'Permission denied|Authenticated to|Too many|Host key verification|Unable to negotiate|Connection refused|timed out|Could not resolve|kex_exchange|Bad owner' "$H/logs/$name.log" | tail -1)"
