#!/bin/sh
# Opens a pull request on Bitbucket Cloud for each change Lapilli was
# approved to make, and tells Lapilli what became of it (see lib.sh for
# the rest).
#
#   BITBUCKET_REPO        such as example/monitoring: WORKSPACE/REPO
#   BITBUCKET_TOKEN_FILE  a file holding a repository access token with
#                         the pull requests write scope
#   BITBUCKET_API         default https://api.bitbucket.org/2.0
recipe=bitbucket.sh
here=$(dirname "$0")
: "${BITBUCKET_REPO:?such as example/monitoring}"
: "${BITBUCKET_TOKEN_FILE:?a file holding a repository access token}"
api=${BITBUCKET_API:-https://api.bitbucket.org/2.0}
case $BITBUCKET_REPO in */*/* | *[!A-Za-z0-9._/-]* | /* | */) echo "bitbucket.sh: BITBUCKET_REPO $BITBUCKET_REPO: want WORKSPACE/REPO" >&2; exit 1 ;; esac
case $BITBUCKET_REPO in */*) ;; *) echo "bitbucket.sh: BITBUCKET_REPO $BITBUCKET_REPO: want WORKSPACE/REPO" >&2; exit 1 ;; esac

host_auth() {
  printf 'Authorization: Bearer %s\nAccept: application/json\n' "$(cat "$BITBUCKET_TOKEN_FILE")"
}

# host_me writes the account the token is.
host_me() {
  code=$(host GET "$api/user")
  case $code in 2??) ;; *) return 1 ;; esac
  jq -r '.uuid // ""' "$out"
}

# A pull request from a fork has the fork as its source repository: only one
# from the repository itself is taken.
find_pr() {
  me=
  if [ "${2:-}" != anyone ]; then
    me=$(host_me) || me=
    if [ -z "$me" ]; then
      say "Bitbucket didn't say who the token is (GET /user): an open pull request is taken as this recipe's own only when Bitbucket refuses another"
      return 0
    fi
  fi
  q=$(printf 'state="OPEN" AND source.branch.name="%s" AND destination.branch.name="%s"' "$1" "$BASE" | jq -sRr @uri)
  code=$(host GET "$api/repositories/$BITBUCKET_REPO/pullrequests?q=$q")
  case $code in 2??) ;; *) say "Bitbucket didn't list pull requests ($code): $(head -c 300 "$out")"; return 1 ;; esac
  jq -r --arg b "$1" --arg repo "$BITBUCKET_REPO" --arg base "$BASE" --arg me "$me" '[.values[] | select(.source.branch.name == $b
    and (.source.repository.full_name // "" | ascii_downcase) == ($repo | ascii_downcase)
    and .destination.branch.name == $base and ($me == "" or .author.uuid == $me))][0].links.html.href // ""' "$out"
}

create_pr() {
  jq --arg head "$1" --arg base "$BASE" '{title, description: .body, source: {branch: {name: $head}},
    destination: {branch: {name: $base}}, close_source_branch: true}' "$2" >"$tmp/body"
  code=$(host POST "$api/repositories/$BITBUCKET_REPO/pullrequests" "$tmp/body")
  case $code in 2??) ;; *) say "Bitbucket didn't open a pull request ($code): $(head -c 300 "$out")"; return 1 ;; esac
  jq -r '.links.html.href // ""' "$out"
}

pr_state() {
  code=$(host GET "$api/repositories/$BITBUCKET_REPO/pullrequests/$1")
  case $code in 2??) ;; *) say "Bitbucket didn't give pull request $1 ($code)"; return 1 ;; esac
  jq -r 'if .state == "MERGED" then "merged" elif .state == "DECLINED" or .state == "SUPERSEDED" then "closed" else "open" end' "$out"
}

# shellcheck source=lib.sh
. "$here/lib.sh"
main
