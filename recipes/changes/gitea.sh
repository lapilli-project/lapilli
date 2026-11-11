#!/bin/sh
# Opens a pull request on Gitea or Forgejo for each change Lapilli was
# approved to make, and tells Lapilli what became of it (see lib.sh for
# the rest).
#
#   GITEA_API         such as https://git.example.com/api/v1
#   GITEA_REPO        such as example/monitoring
#   GITEA_TOKEN_FILE  a file holding a token that may open pull requests
#                     in it, and say whose it is (scopes write:repository
#                     and read:user)
recipe=gitea.sh
here=$(dirname "$0")
: "${GITEA_API:?such as https://git.example.com/api/v1}"
: "${GITEA_REPO:?such as example/monitoring}"
: "${GITEA_TOKEN_FILE:?a file holding a token that may open pull requests}"
case $GITEA_REPO in */*/* | *[!A-Za-z0-9._/-]* | /* | */) echo "gitea.sh: GITEA_REPO $GITEA_REPO: want OWNER/REPO" >&2; exit 1 ;; esac
case $GITEA_REPO in */*) ;; *) echo "gitea.sh: GITEA_REPO $GITEA_REPO: want OWNER/REPO" >&2; exit 1 ;; esac

host_auth() {
  printf 'Authorization: token %s\nAccept: application/json\n' "$(cat "$GITEA_TOKEN_FILE")"
}

# host_me writes the account the token is.
host_me() {
  code=$(host GET "$GITEA_API/user")
  case $code in 2??) ;; *) return 1 ;; esac
  jq -r '.login // ""' "$out"
}

find_pr() {
  me=
  if [ "${2:-}" != anyone ]; then
    me=$(host_me) || me=
    if [ -z "$me" ]; then
      say "Gitea didn't say who the token is (GET /user, scope read:user): an open pull request is taken as this recipe's own only when Gitea refuses another"
      return 0
    fi
  fi
  code=$(host GET "$GITEA_API/repos/$GITEA_REPO/pulls?state=open&limit=50")
  case $code in 2??) ;; *) say "Gitea didn't list pull requests ($code): $(head -c 300 "$out")"; return 1 ;; esac
  jq -r --arg b "$1" --arg repo "$GITEA_REPO" --arg base "$BASE" --arg me "$me" '[.[] | select(.head.ref == $b
    and (.head.repo.full_name // "" | ascii_downcase) == ($repo | ascii_downcase) and .base.ref == $base
    and ($me == "" or (.user.login // "" | ascii_downcase) == ($me | ascii_downcase)))][0].html_url // ""' "$out"
}

create_pr() {
  jq --arg head "$1" --arg base "$BASE" '{title, body, head: $head, base: $base}' "$2" >"$tmp/body"
  code=$(host POST "$GITEA_API/repos/$GITEA_REPO/pulls" "$tmp/body")
  case $code in 2??) ;; *) say "Gitea didn't open a pull request ($code): $(head -c 300 "$out")"; return 1 ;; esac
  jq -r '.html_url // ""' "$out"
}

pr_state() {
  code=$(host GET "$GITEA_API/repos/$GITEA_REPO/pulls/$1")
  case $code in 2??) ;; *) say "Gitea didn't give pull request $1 ($code)"; return 1 ;; esac
  jq -r 'if .merged == true then "merged" elif .state == "closed" then "closed" else "open" end' "$out"
}

# shellcheck source=lib.sh
. "$here/lib.sh"
main
