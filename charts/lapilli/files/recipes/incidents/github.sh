#!/bin/sh
# Writes a repository's issues with a label to stdout, as Lapilli's incidents
# file takes them. It uses the GitHub CLI, signed in however gh is.
#
#   GITHUB_REPO   such as example/incidents
#   GITHUB_LABEL  the label your incidents have (default incident)
#   LIMIT         how many issues at most, the latest first (default 500)
set -eu
here=$(dirname "$0")
: "${GITHUB_REPO:?such as example/incidents}"
issues=$(mktemp)
trap 'rm -f "$issues"' EXIT
trap 'exit 1' INT TERM HUP
# To a file first: in a pipe, a gh that failed would go unnoticed.
gh issue list --repo "$GITHUB_REPO" --label "${GITHUB_LABEL:-incident}" --state all --limit "${LIMIT:-500}" \
  --json number,title,createdAt,closedAt,labels >"$issues"
jq -f "$here/github.jq" "$issues"
