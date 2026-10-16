#!/bin/bash
# Stops the lab's sshd instances.
H="$(cd "$(dirname "$0")" && pwd)"
for p in "$H"/run/sshd-*.pid; do
  [ -f "$p" ] && kill "$(cat "$p")" 2>/dev/null && rm -f "$p"
done
echo "lab stopped"
