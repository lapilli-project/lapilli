#!/bin/sh
# Opens a pull request on GitHub for each change Lapilli was approved to
# make, and tells Lapilli what became of it (see lib.sh for the rest).
#
#   GITHUB_REPO        such as example/monitoring
#   GITHUB_TOKEN_FILE  a file holding a token that may open pull requests
#                      in it (a fine-grained token: pull requests, write)
#   GITHUB_API         default https://api.github.com; a GitHub Enterprise
#                      server's https://HOST/api/v3
#
#   LAPILLI_URL=http://lapilli:8082 LAPILLI_TOKEN_FILE=/run/secrets/changes \
#   CLONE=https://github.com/example/monitoring.git GITHUB_REPO=example/monitoring \
#   GITHUB_TOKEN_FILE=/run/secrets/github ./github.sh
recipe=github.sh
here=$(dirname "$0")
: "${GITHUB_REPO:?such as example/monitoring}"
: "${GITHUB_TOKEN_FILE:?a file holding a token that may open pull requests}"
api=${GITHUB_API:-https://api.github.com}
case $GITHUB_REPO in */*/* | *[!A-Za-z0-9._/-]* | /* | */) echo "github.sh: GITHUB_REPO $GITHUB_REPO: want OWNER/REPO" >&2; exit 1 ;; esac
case $GITHUB_REPO in */*) ;; *) echo "github.sh: GITHUB_REPO $GITHUB_REPO: want OWNER/REPO" >&2; exit 1 ;; esac
owner=${GITHUB_REPO%%/*}

host_auth() {
  printf 'Authorization: Bearer %s\nAccept: application/vnd.github+json\nX-GitHub-Api-Version: 2022-11-28\n' "$(cat "$GITHUB_TOKEN_FILE")"
}

find_pr() {
  code=$(host GET "$api/repos/$GITHUB_REPO/pulls?state=open&head=$owner:$1")
  case $code in 2??) ;; *) say "GitHub didn't list pull requests ($code): $(head -c 300 "$out")"; return 1 ;; esac
  jq -r '.[0].html_url // ""' "$out"
}

create_pr() {
  jq --arg head "$1" --arg base "$BASE" '{title, body, head: $head, base: $base}' "$2" >"$tmp/body"
  code=$(host POST "$api/repos/$GITHUB_REPO/pulls" "$tmp/body")
  case $code in 2??) ;; *) say "GitHub didn't open a pull request ($code): $(head -c 300 "$out")"; return 1 ;; esac
  jq -r '.html_url // ""' "$out"
}

pr_state() {
  code=$(host GET "$api/repos/$GITHUB_REPO/pulls/$1")
  case $code in 2??) ;; *) say "GitHub didn't give pull request $1 ($code)"; return 1 ;; esac
  jq -r 'if .merged == true then "merged" elif .state == "closed" then "closed" else "open" end' "$out"
}

# shellcheck source=lib.sh
. "$here/lib.sh"
main
