#!/bin/bash
# agent.sh NAME [ssh-add args per key ...] -- CAPTURE ARGS...
# Starts a lab-only ssh-agent on a short relative socket path, loads keys, runs capture.sh, kills the agent.
# Never falls through to the user's own agent: if the lab socket is not there, it stops.
set -uo pipefail
H="$(cd "$(dirname "$0")" && pwd)"
cd "$H"
unset SSH_AUTH_SOCK SSH_AGENT_PID SSH_ASKPASS DISPLAY
sock="run/a-$1.sock"; shift
rm -f "$sock"
eval "$(ssh-agent -a "$sock")" > /dev/null
if [ "${SSH_AUTH_SOCK:-}" != "$sock" ] || [ ! -S "$sock" ]; then
  echo "lab agent did not start; stopping" >&2; exit 1
fi
trap 'kill "$SSH_AGENT_PID" 2>/dev/null; rm -f "$sock"' EXIT
while [ $# -gt 0 ] && [ "$1" != "--" ]; do ssh-add -q $1 2>&1; shift; done
shift
echo "lab agent: $(ssh-add -l | awk '{print $3}' | tr '\n' ' ')"
"$H/capture.sh" "$@"
