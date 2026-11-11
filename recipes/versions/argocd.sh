#!/bin/sh
# Writes when an Argo CD Application deployed each revision, as git.sh's
# DEPLOYS reads it: "REVISION TIME" a line, the earliest first, so a version
# of the rules is named by when it was synced rather than committed. It
# only reads.
#
#   ARGOCD_URL            Argo CD's address, https; or give the Application
#                         as kubectl writes it on stdin, with the argument -
#   ARGOCD_TOKEN_FILE     a file holding a token that may get applications
#                         (p, ROLE, applications, get, PROJECT/APP, allow)
#   ARGOCD_APP            the Application's name
#   ARGOCD_APP_NAMESPACE  optional: its namespace, for Applications kept in
#                         any namespace
#   ARGOCD_PROJECT        optional: its project
#   ARGOCD_SOURCE         for an Application of several sources, which one's
#                         revisions: 0 by default
#   DEPLOYS_KEEP          optional: a file this wrote before, kept and added
#                         to: Argo CD keeps ten syncs by default
#
#   ARGOCD_URL=https://argocd.example.com ARGOCD_TOKEN_FILE=token ARGOCD_APP=alerts \
#     DEPLOYS_KEEP=deploys.txt ./argocd.sh > deploys.new && mv deploys.new deploys.txt
#   kubectl get application alerts -n argocd -o json | ./argocd.sh - > deploys.txt
set -eu
here=$(dirname "$0")
. "$here/lib-deploys.sh"
say() { printf 'argocd.sh: %s\n' "$*" >&2; }
source=${ARGOCD_SOURCE:-0}
case $source in *[!0-9]* | '') say "ARGOCD_SOURCE $source: want a number, 0 for the first source"; exit 1 ;; esac
# A Kubernetes name: what reaches a URL can't be a path or a query.
name() {
  printf '%s' "$1" | grep -Eq '^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$' || {
    say "$2 $1 isn't a Kubernetes name"
    exit 1
  }
}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
trap 'exit 1' INT TERM HUP
if [ "${1:-}" = - ]; then
  head -c 4194305 >"$tmp/app"
else
  : "${ARGOCD_URL:?the address of Argo CD, or - to read the Application on stdin}"
  : "${ARGOCD_TOKEN_FILE:?a file holding a token that may get applications}"
  : "${ARGOCD_APP:?the name of the Application}"
  case $ARGOCD_URL in
    https://*) ;;
    *) say "ARGOCD_URL must be https: the token goes with it"; exit 1 ;;
  esac
  case $ARGOCD_URL in *[!A-Za-z0-9.:/_-]*) say "ARGOCD_URL $ARGOCD_URL: want an address such as https://argocd.example.com"; exit 1 ;; esac
  name "$ARGOCD_APP" ARGOCD_APP
  q=
  if [ -n "${ARGOCD_APP_NAMESPACE:-}" ]; then
    name "$ARGOCD_APP_NAMESPACE" ARGOCD_APP_NAMESPACE
    q="?appNamespace=$ARGOCD_APP_NAMESPACE"
  fi
  if [ -n "${ARGOCD_PROJECT:-}" ]; then
    name "$ARGOCD_PROJECT" ARGOCD_PROJECT
    q="${q:-?}${q:+&}project=$ARGOCD_PROJECT"
  fi
  # The token on stdin, never on a command line; no redirect followed.
  printf 'Authorization: Bearer %s\n' "$(cat "$ARGOCD_TOKEN_FILE")" |
    curl -fsS --proto =https --max-time 30 --max-filesize 4194304 -H @- -H 'Accept: application/json' \
      "${ARGOCD_URL%/}/api/v1/applications/$ARGOCD_APP$q" >"$tmp/app"
fi
if [ "$(wc -c <"$tmp/app")" -gt 4194304 ]; then
  say "the Application is over 4 MB"
  exit 1
fi
if ! jq -e '(if .kind == "List" then .items[] else . end) | .kind == "Application"' "$tmp/app" >/dev/null 2>&1; then
  # Argo CD's API leaves out kind; what it answers is an Application.
  if jq -e '.metadata.name and (.status.history | type) == "array"' "$tmp/app" >/dev/null 2>&1; then
    jq '. + {kind: "Application"}' "$tmp/app" >"$tmp/app2" && mv "$tmp/app2" "$tmp/app"
  else
    say "not an Argo CD Application with its history"
    exit 1
  fi
fi
jq -r --argjson source "$source" -f "$here/deploys.jq" "$tmp/app" >"$tmp/new"
if [ ! -s "$tmp/new" ]; then
  say "the Application's history holds no deploy of a git revision"
fi
kept "$tmp/new"
