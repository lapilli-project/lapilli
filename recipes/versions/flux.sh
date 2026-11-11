#!/bin/sh
# Writes when Flux deployed each revision, as git.sh's DEPLOYS reads it:
# "REVISION TIME" a line, the earliest first, from a Kustomization or a
# HelmRelease (or a List of them) as kubectl writes it, on stdin. It only
# reads.
#
#   DEPLOYS_KEEP   optional: a file this wrote before, kept and added to:
#                  a Kustomization keeps five snapshots
#
# A Kustomization's status.history (Flux 2.7 on) names each revision it
# applied, and when it first did; before 2.7, only the last, when it became
# ready. A HelmRelease names a chart version: one built from a git source
# ends +SHA, which is taken as the revision.
#
#   kubectl get kustomization alerts -n flux-system -o json |
#     DEPLOYS_KEEP=deploys.txt ./flux.sh > deploys.new && mv deploys.new deploys.txt
set -eu
here=$(dirname "$0")
. "$here/lib-deploys.sh"
say() { printf 'flux.sh: %s\n' "$*" >&2; }
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
trap 'exit 1' INT TERM HUP
head -c 4194305 >"$tmp/in"
if [ "$(wc -c <"$tmp/in")" -gt 4194304 ]; then
  say "what came on stdin is over 4 MB"
  exit 1
fi
if ! jq -e '[(if .kind == "List" then .items[] else . end) | .kind | IN("Kustomization", "HelmRelease")] | all and length > 0' "$tmp/in" >/dev/null 2>&1; then
  say "not a Flux Kustomization or HelmRelease, as kubectl get -o json writes one"
  exit 1
fi
jq -r --argjson source 0 -f "$here/deploys.jq" "$tmp/in" >"$tmp/new"
if [ ! -s "$tmp/new" ]; then
  say "no deploy of a git revision in it"
fi
kept "$tmp/new"
