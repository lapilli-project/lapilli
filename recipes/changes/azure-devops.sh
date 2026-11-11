#!/bin/sh
# Opens a pull request in Azure Repos (Azure DevOps) for each change
# Lapilli was approved to make, and tells Lapilli what became of it (see
# lib.sh for the rest).
#
#   AZURE_DEVOPS_ORG_URL     such as https://dev.azure.com/example
#   AZURE_DEVOPS_PROJECT     the project's name
#   AZURE_DEVOPS_REPO        the repository's name
#   AZURE_DEVOPS_TOKEN_FILE  a file holding a personal access token with
#                            Code (read and write)
recipe=azure-devops.sh
here=$(dirname "$0")
: "${AZURE_DEVOPS_ORG_URL:?such as https://dev.azure.com/example}"
: "${AZURE_DEVOPS_PROJECT:?the project}"
: "${AZURE_DEVOPS_REPO:?the repository}"
: "${AZURE_DEVOPS_TOKEN_FILE:?a file holding a personal access token}"
org=${AZURE_DEVOPS_ORG_URL%/}
project=$(printf '%s' "$AZURE_DEVOPS_PROJECT" | jq -sRr @uri)
repo=$(printf '%s' "$AZURE_DEVOPS_REPO" | jq -sRr @uri)
base_api=$org/$project/_apis/git/repositories/$repo

host_auth() {
  printf 'Authorization: Basic %s\nAccept: application/json\n' "$(printf ':%s' "$(cat "$AZURE_DEVOPS_TOKEN_FILE")" | base64 | tr -d '\n')"
}

# A pull request's page, built from its number: Azure's answer gives the
# API's URL, not the page's.
page() { printf '%s/%s/_git/%s/pullrequest/%s' "$org" "$project" "$repo" "$1"; }

find_pr() {
  ref=$(printf 'refs/heads/%s' "$1" | jq -sRr @uri)
  code=$(host GET "$base_api/pullrequests?searchCriteria.sourceRefName=$ref&searchCriteria.status=active&api-version=7.1")
  case $code in 2??) ;; *) say "Azure DevOps didn't list pull requests ($code): $(head -c 300 "$out")"; return 1 ;; esac
  n=$(jq -r '.value[0].pullRequestId // ""' "$out")
  case $n in '') ;; *[!0-9]*) say "Azure DevOps gave a pull request with no number"; return 1 ;; *) page "$n" ;; esac
}

create_pr() {
  # Azure takes a description of 4,000 characters at most.
  jq --arg head "refs/heads/$1" --arg base "refs/heads/$BASE" '{title, description: .body[0:3900], sourceRefName: $head, targetRefName: $base}' "$2" >"$tmp/body"
  code=$(host POST "$base_api/pullrequests?api-version=7.1" "$tmp/body")
  case $code in 2??) ;; *) say "Azure DevOps didn't open a pull request ($code): $(head -c 300 "$out")"; return 1 ;; esac
  n=$(jq -r '.pullRequestId // ""' "$out")
  case $n in '' | *[!0-9]*) say "Azure DevOps gave a pull request with no number"; return 1 ;; esac
  page "$n"
}

pr_state() {
  code=$(host GET "$base_api/pullrequests/$1?api-version=7.1")
  case $code in 2??) ;; *) say "Azure DevOps didn't give pull request $1 ($code)"; return 1 ;; esac
  jq -r 'if .status == "completed" then "merged" elif .status == "abandoned" then "closed" else "open" end' "$out"
}

# shellcheck source=lib.sh
. "$here/lib.sh"
main
